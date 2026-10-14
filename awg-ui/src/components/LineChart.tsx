// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useMemo, useRef, useState } from 'react';

// Линейный график для вкладки «Монитор». Одна ось, линия 2 px, конечная точка с кольцом цвета поверхности,
// перекрестие с подсказкой по всем рядам, легенда при ≥ 2 рядах, табличный вид и управление стрелками.
// Цвета рядов — токены темы (--series-N в monitor.css), подписи и значения — всегда цветом текста.
export interface ChartSeries { id: string; name: string; color: string; points: [number, number][] }

const M = { left: 46, right: 14, top: 10, bottom: 24 };

// Шкала оси: круглый шаг (1, 2, 2.5, 5 × 10ⁿ) и 3–5 интервалов, чтобы подписи были 0 / 50 / 100 / 150, а не 0 / 63 / 125.
function niceScale(max: number): { top: number; ticks: number[] } {
    if (!(max > 0)) return { top: 1, ticks: [0, 1] };
    const raw  = max / 4;
    const p    = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = ([1, 2, 2.5, 5, 10].map(f => f * p).find(s => s >= raw)) ?? 10 * p;
    const n    = Math.max(1, Math.ceil(max / step - 1e-9));
    return { top: step * n, ticks: Array.from({ length: n + 1 }, (_, i) => step * i) };
}

const timeLabel = (t: number, span: number) => {
    const d = new Date(t * 1000);
    const hm = d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
    return span > 86400 * 1.5 ? `${d.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit' })} ${hm}` : hm;
};

export default function LineChart({ title, series, from, to, step, format, formatAxis, max, dim = false }: {
    title:        string;
    series:       ChartSeries[];
    from:         number;
    to:           number;
    step:         number;                       // шаг данных, с: разрыв длиннее трёх шагов рвёт линию, а не рисует мост
    format:       (v: number) => string;        // значение в подсказке и таблице
    formatAxis?:  (v: number) => string;        // компактная подпись оси
    max?:         number;                       // фиксированный верх оси (проценты — 100); без него подбирается по данным
    dim?:         boolean;                      // идёт перезагрузка: держим прежний кадр бледнее
}) {
    const wrap = useRef<HTMLDivElement>(null);     // содержимое карточки без её padding: от него зависит ширина svg
    const [w, setW] = useState(640);
    const [hover, setHover] = useState<number | null>(null);     // индекс по оси времени
    const [table, setTable] = useState(false);
    const H = 200;

    useEffect(() => {
        const el = wrap.current;
        if (!el) return;
        const ro = new ResizeObserver(() => setW(Math.max(260, el.clientWidth)));
        ro.observe(el); setW(Math.max(260, el.clientWidth));
        return () => ro.disconnect();
    }, []);

    // Общая ось времени: метки первого непустого ряда (Prometheus выравнивает точки по шагу).
    const times = useMemo(() => {
        const set = new Set<number>();
        for (const s of series) for (const p of s.points) set.add(p[0]);
        return [...set].sort((a, b) => a - b);
    }, [series]);
    const byTime = useMemo(() => series.map(s => new Map(s.points)), [series]);

    const empty = times.length === 0;
    const { top: yMax, ticks: yTicks } = max !== undefined
        ? { top: max, ticks: [0, 1, 2, 3, 4].map(i => (max / 4) * i) }
        : niceScale(Math.max(0, ...series.flatMap(s => s.points.map(p => p[1]))) * 1.04);
    const x = (t: number) => M.left + ((t - from) / Math.max(1, to - from)) * (w - M.left - M.right);
    const y = (v: number) => H - M.bottom - (v / yMax) * (H - M.top - M.bottom);

    const path = (pts: [number, number][]) => {
        let d = '', prev: number | null = null;
        for (const [t, v] of pts) {
            d += `${prev !== null && t - prev <= step * 3 ? 'L' : 'M'}${x(t).toFixed(1)},${y(v).toFixed(1)}`;
            prev = t;
        }
        return d;
    };

    const span   = to - from;
    const xTicks = [0, 1, 2, 3, 4].map(i => from + (span / 4) * i);
    const axis   = formatAxis ?? format;

    const onMove = (e: React.PointerEvent<SVGRectElement>) => {
        if (!times.length) return;
        const r  = e.currentTarget.getBoundingClientRect();
        const t  = from + ((e.clientX - r.left) / Math.max(1, r.width)) * span;
        let best = 0;
        for (let i = 1; i < times.length; i++) if (Math.abs(times[i] - t) < Math.abs(times[best] - t)) best = i;
        setHover(best);
    };
    const onKey = (e: React.KeyboardEvent) => {
        if (!times.length) return;
        if (e.key === 'ArrowLeft')  { e.preventDefault(); setHover(h => Math.max(0, (h ?? times.length) - 1)); }
        if (e.key === 'ArrowRight') { e.preventDefault(); setHover(h => Math.min(times.length - 1, (h ?? -1) + 1)); }
        if (e.key === 'Escape')     setHover(null);
    };

    const ht = hover !== null ? times[hover] : null;
    // Подсказка не вылезает за правый край: правее середины открывается влево от перекрестия
    const flip = ht !== null && x(ht) > w * 0.55;

    // Табличный вид — те же числа без наведения: ~12 равномерно выбранных строк, последняя — самая свежая
    const rows = useMemo(() => {
        if (!times.length) return [] as number[];
        const n = Math.min(12, times.length);
        return Array.from({ length: n }, (_, i) => times[Math.round(((times.length - 1) * (i + 1)) / n)]).reverse();
    }, [times]);

    return (
        <figure className={`mon-chart${dim ? ' mon-chart--dim' : ''}`}>
            <figcaption className="mon-chart-head">
                <span className="mon-chart-title">{title}</span>
                {series.length >= 2 && (
                    <span className="mon-legend">
                        {series.map(s => (
                            <span className="mon-legend-item" key={s.id}>
                                <i className="mon-key" style={{ background: s.color }} aria-hidden="true" />{s.name}
                            </span>
                        ))}
                    </span>
                )}
                <button type="button" className="mon-chart-toggle" aria-pressed={table} onClick={() => setTable(t => !t)}>
                    {table ? 'График' : 'Таблица'}
                </button>
            </figcaption>

            <div ref={wrap}>
            {table ? (
                <div className="mon-table-wrap">
                    <table className="mon-table">
                        <thead><tr><th>Время</th>{series.map(s => <th key={s.id}>{s.name}</th>)}</tr></thead>
                        <tbody>
                            {rows.length === 0 ? <tr><td colSpan={series.length + 1}>Нет данных</td></tr> : rows.map(t => (
                                <tr key={t}>
                                    <td>{timeLabel(t, span)}</td>
                                    {series.map((s, i) => <td key={s.id}>{byTime[i].has(t) ? format(byTime[i].get(t)!) : '—'}</td>)}
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </div>
            ) : (
                <div className="mon-plot">
                    <svg width={w} height={H} role="img" tabIndex={0} aria-label={`${title}. Стрелки влево и вправо — значения по времени.`}
                        onKeyDown={onKey} onBlur={() => setHover(null)}>
                        {yTicks.map((v, i) => (
                            <g key={i}>
                                <line className="mon-grid" x1={M.left} x2={w - M.right} y1={y(v)} y2={y(v)} />
                                <text className="mon-tick" x={M.left - 8} y={y(v) + 4} textAnchor="end">{axis(v)}</text>
                            </g>
                        ))}
                        {xTicks.map((t, i) => (
                            <text key={i} className="mon-tick" x={x(t)} y={H - 6} textAnchor={i === 0 ? 'start' : i === 4 ? 'end' : 'middle'}>
                                {timeLabel(t, span)}
                            </text>
                        ))}
                        {!empty && series.map(s => (
                            <path key={s.id} d={path(s.points)} fill="none" style={{ stroke: s.color }} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
                        ))}
                        {/* конечная точка: r 4 + кольцо цвета поверхности, чтобы читалась поверх линий */}
                        {!empty && series.map(s => s.points.length > 0 && (
                            <circle key={s.id} cx={x(s.points[s.points.length - 1][0])} cy={y(s.points[s.points.length - 1][1])}
                                r={4} style={{ fill: s.color }} className="mon-end" />
                        ))}
                        {ht !== null && (
                            <g>
                                <line className="mon-cross" x1={x(ht)} x2={x(ht)} y1={M.top} y2={H - M.bottom} />
                                {series.map((s, i) => byTime[i].has(ht) && (
                                    <circle key={s.id} cx={x(ht)} cy={y(byTime[i].get(ht)!)} r={4} style={{ fill: s.color }} className="mon-end" />
                                ))}
                            </g>
                        )}
                        {/* зона наведения шире линии: указатель ищет момент времени, а не 2-пиксельную линию */}
                        <rect x={M.left} y={M.top} width={Math.max(0, w - M.left - M.right)} height={H - M.top - M.bottom}
                            fill="transparent" onPointerMove={onMove} onPointerLeave={() => setHover(null)} />
                    </svg>
                    {empty && <p className="mon-empty">Данные появятся через минуту-две после запуска Prometheus</p>}
                    {ht !== null && (
                        <div className="mon-tip" style={{ left: flip ? undefined : x(ht) + 12, right: flip ? w - x(ht) + 12 : undefined, top: M.top }} role="status">
                            <span className="mon-tip-time">{timeLabel(ht, span)}</span>
                            {series.map((s, i) => (
                                <span className="mon-tip-row" key={s.id}>
                                    <i className="mon-key" style={{ background: s.color }} aria-hidden="true" />
                                    <b>{byTime[i].has(ht) ? format(byTime[i].get(ht)!) : '—'}</b>
                                    <span>{s.name}</span>
                                </span>
                            ))}
                        </div>
                    )}
                </div>
            )}
            </div>
        </figure>
    );
}
