// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback, useRef } from 'react';
import { apiFetch, getServerId, openTab, announcePrometheus, type NodeInfo, type PageProps } from '../../lib/shared';
import { IcoGear } from '../../components/icons';
import type { MonitorStatus as Status } from '../settings/prometheus';
import LineChart from '../../components/LineChart';
import ServerSelect, { ALL_SERVERS } from '../../components/ServerSelect';
import './monitor.css';

// Вкладка «Монитор»: основные графики со встроенного Prometheus. Ставится и удаляется он в «Настройках»,
// раздел «Prometheus» (tabs/settings/prometheus); пока его нет, здесь — карточка со ссылкой туда.
// Графики читают Prometheus через фиксированные запросы панели (internal/ui/monitor.go).
type Pts = [number, number][];
interface Overview {
    range: string; step: number; from: number; to: number;
    servers: {
        id: number; name: string; ok: boolean; down: number | null; up: number | null; online: number | null; heavy: number | null; congested: boolean;
        cpu: number | null; mem: number | null; cores: number | null; mem_total: number | null;
        used_down: number | null; used_up: number | null;   // байт за весь период
    }[];
    total: Series;
}
interface Series { down: Pts; up: Pts; online: Pts; heavy: Pts; drops: Pts; cpu: Pts; mem: Pts }
type Row = Overview['servers'][number];
type NumKey = 'down' | 'up' | 'online' | 'heavy' | 'cpu' | 'mem' | 'cores' | 'mem_total' | 'used_down' | 'used_up';

// «Все серверы» — сводка «сейчас» для плиток: скорости, устройства, ядра, память и объём складываем,
// загрузку процессора и памяти — среднее по серверам (как и ряды total в monitor.go).
function sumUp(rows: Row[]): Row | undefined {
    if (rows.length <= 1) return rows[0];
    const sum = (k: NumKey) => rows.some(r => r[k] != null) ? rows.reduce((n, r) => n + (r[k] ?? 0), 0) : null;
    const avg = (k: NumKey) => {
        const v = rows.map(r => r[k]).filter((x): x is number => x != null);
        return v.length ? v.reduce((a, b) => a + b, 0) / v.length : null;
    };
    return {
        id: ALL_SERVERS, name: 'Все серверы', ok: rows.every(r => r.ok), congested: rows.some(r => r.congested),
        down: sum('down'), up: sum('up'), online: sum('online'), heavy: sum('heavy'), cpu: avg('cpu'), mem: avg('mem'),
        cores: sum('cores'), mem_total: sum('mem_total'), used_down: sum('used_down'), used_up: sum('used_up'),
    };
}

const RANGES: [string, string][] = [['1h', '1 час'], ['6h', '6 часов'], ['24h', '24 часа'], ['7d', '7 дней'], ['30d', '30 дней']];

const nf = (v: number, d = 1) => v.toLocaleString('ru-RU', { maximumFractionDigits: d });
// Скорость в битах/с → «12,4 Мбит/с»
function rate(v: number, d = 1): string {
    if (v >= 1e9) return `${nf(v / 1e9, d)} Гбит/с`;
    if (v >= 1e6) return `${nf(v / 1e6, d)} Мбит/с`;
    if (v >= 1e3) return `${nf(v / 1e3, d)} Кбит/с`;
    return `${nf(v, 0)} бит/с`;
}
const lastOf = (p: Pts) => (p.length ? p[p.length - 1][1] : null);
// Полоска нагрузки: цвет заливки — серьёзность (норма → внимание → опасно), дорожка — светлая ступень той же шкалы.
// Цифра рядом всегда есть: цвет не единственный носитель смысла.
const level = (v: number) => (v >= 90 ? 'mon-bar--bad' : v >= 70 ? 'mon-bar--warn' : '');
function Bar({ v, label }: { v: number | null | undefined; label: string }) {
    const pct = v == null ? 0 : Math.min(100, Math.max(0, v));
    return (
        <span className="mon-bar" role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={100}
            aria-valuenow={v == null ? undefined : Math.round(pct)}>
            <i className={v == null ? '' : level(pct)} style={{ width: `${pct}%` }} />
        </span>
    );
}
const gb = (b: number) => `${nf(b / 2 ** 30, 1)} ГБ`;
// Объём трафика — десятичные единицы, как считают провайдеры: 1 ТБ = 10¹² байт
function vol(b: number): string {
    if (b >= 1e12) return `${nf(b / 1e12, 2)} ТБ`;
    if (b >= 1e9)  return `${nf(b / 1e9, 1)} ГБ`;
    return `${nf(b / 1e6, 0)} МБ`;
}

export default function MonitorPage({ token }: PageProps) {
    const [status, setStatus]   = useState<Status | null>(null);
    const [range, setRange]     = useState('1h');
    const [ov, setOv]           = useState<Overview | null>(null);
    const [ovErr, setOvErr]     = useState('');
    const [loading, setLoading] = useState(false);
    const [nodes, setNodes]     = useState<NodeInfo[]>([]);
    // Чьи графики: сервер (по умолчанию — выбранный в шапке) или ALL_SERVERS. Выбор свой, шапку не меняет.
    const [view, setView]       = useState(() => getServerId() ?? 0);
    const alive = useRef(true);
    // StrictMode монтирует эффект дважды: флаг надо вернуть в true, иначе все ответы отбрасываются
    useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

    const loadStatus = useCallback(async () => {
        try { const s = await apiFetch('GET', `/ui/monitor?server=${getServerId() ?? 0}`, token) as Status; announcePrometheus(s.ready); if (alive.current) setStatus(s); return s; }
        catch { return null; }
    }, [token]);

    const loadOverview = useCallback(async () => {
        setLoading(true);
        try {
            // Без ?server= monitor.go отдаёт все серверы: total — сумма (процессор и память — среднее), servers — каждый
            const q = view === ALL_SERVERS ? '' : `&server=${view}`;
            const o = await apiFetch('GET', `/ui/monitor/overview?range=${range}${q}`, token) as Overview;
            if (alive.current) { setOv(o); setOvErr(''); }
        } catch (e: any) { if (alive.current) setOvErr(e?.response?.data?.error ?? 'Не удалось получить данные'); }
        finally { if (alive.current) setLoading(false); }
    }, [token, range, view]);

    useEffect(() => { void loadStatus(); }, [loadStatus]);

    // Серверы для выбора (и их связь — точки на плашках)
    useEffect(() => {
        const load = () => apiFetch('GET', '/ui/nodes', token)
            .then(r => { if (alive.current) setNodes(r.nodes ?? []); })
            .catch(() => { /* выбор необязателен — останется текущий сервер */ });
        void load();
        const t = setInterval(load, 15000);
        return () => clearInterval(t);
    }, [token]);

    // Пока Prometheus готов — обновляем графики; пока ставится — следим за ходом установки.
    useEffect(() => {
        if (!status?.ready) return;
        void loadOverview();
        const t = setInterval(() => { void loadOverview(); }, 15000);
        return () => clearInterval(t);
    }, [status?.ready, loadOverview]);

    // Пока Prometheus нет — раз в 10 с проверяем: его могли поставить в «Настройках»
    useEffect(() => {
        if (!status || status.ready) return;
        const t = setInterval(() => { void loadStatus(); }, 10000);
        return () => clearInterval(t);
    }, [status, loadStatus]);

    // Prometheus нет — графиков не будет: ведём в «Настройки», где он ставится
    const setup = status && !status.ready && (
        <section className="mon-card" aria-labelledby="mon-setup">
            <div className="mon-head">
                <h3 className="mon-title" id="mon-setup">Мониторинг</h3>
                <span className="chip chip--offline">
                    <span className="chip-dot" />{status.job?.running && status.job.action === 'install' ? 'ставится' : 'не подключён'}
                </span>
            </div>
            <p className="mon-hint">
                Графики — скорость, устройства онлайн, нагрузка на канал — строит встроенный Prometheus.
                Он ставится в «Настройках», на вкладке «Prometheus»; графики появятся здесь через минуту-две после установки.
            </p>
            <div className="mon-actions">
                <button className="btn btn--primary" onClick={() => openTab('settings', 'prom')}>
                    <IcoGear /> Открыть настройки
                </button>
            </div>
        </section>
    );

    // ───────── дашборд ─────────
    // Один сервер или сводка по всем (sumUp) — плитки читают одно и то же
    const all  = view === ALL_SERVERS;
    const one  = all ? sumUp(ov?.servers ?? []) : ov?.servers[0];
    // Кого нет в сумме: сервер не отдал метрики (ok = false) или на связи, а рядов нет совсем (старый агент)
    const silent = !all || !ov ? [] : [
        ...ov.servers.filter(r => !r.ok).map(r => r.name),
        ...nodes.filter(n => n.online && !ov.servers.some(r => r.id === n.id)).map(n => n.health?.server || n.name),
    ];
    const data = ov?.total ?? null;
    const cur  = data && {
        // Скорость «сейчас» — от сервера: на неделе/месяце последняя точка ряда — среднее за шаг, а не текущее
        down: one?.down ?? lastOf(data.down), up: one?.up ?? lastOf(data.up), online: lastOf(data.online), heavy: lastOf(data.heavy),
        busy: !!one?.congested,
    };
    const used = one?.used_down != null && one.used_up != null ? one.used_down + one.used_up : null;
    // Одна единица на всю ось (Мбит/с, а не «50 Мбит» у каждой риски): выбирается по максимуму ряда
    const peak = data ? Math.max(0, ...data.down.map(p => p[1]), ...data.up.map(p => p[1])) : 0;
    const unit = peak >= 1e9 ? { name: 'Гбит/с', div: 1e9 } : peak >= 1e6 ? { name: 'Мбит/с', div: 1e6 } : peak >= 1e3 ? { name: 'Кбит/с', div: 1e3 } : { name: 'бит/с', div: 1 };
    const dashboard = status?.ready && (
        <>
            {/* Период слева задаёт окно для всего ниже; справа — чьи графики: кнопка и меню, как выбор
                сервера в шапке, «Все серверы» — сводка по всем */}
            <div className="mon-toolbar">
                <div className="mon-filters" role="group" aria-label="Период">
                    {RANGES.map(([id, label]) => (
                        <button key={id} type="button" className={`mon-range${range === id ? ' mon-range--on' : ''}`}
                            aria-pressed={range === id} onClick={() => setRange(id)}>{label}</button>
                    ))}
                </div>
                {nodes.length > 0 && (
                    <ServerSelect name="mon-server-menu" nodes={nodes} value={view} onChange={setView} all pickOffline />
                )}
            </div>

            {ovErr && <p className="login-error">{ovErr}</p>}
            {ov && !all && !one?.ok && (
                <p className="mon-hint mon-notice">Этот сервер сейчас не отдаёт метрики: графики могут быть неполными или пустыми.</p>
            )}
            {ov && all && (ov.servers.length === 0 || silent.length > 0) && (
                <p className="mon-hint mon-notice">
                    {ov.servers.length === 0 ? 'Метрики сейчас не отдаёт ни один сервер.'
                        : `Не отдают метрики: ${silent.join(', ')} — в сумме их нет.`}
                </p>
            )}

            <div className="mon-tiles">
                <div className="mon-tile">
                    <span className="mon-tile-label">Трафик сейчас</span>
                    {/* Стрелки окрашены как серии графика «Трафик» ниже */}
                    <div className="mon-tile-rates">
                        <span className="mon-tile-value mon-tile-rate">
                            <span className="mon-tile-dir" style={{ color: 'var(--series-1)' }} role="img" aria-label="загрузка">↓</span>
                            {cur?.down != null ? rate(cur.down) : '—'}
                        </span>
                        <span className="mon-tile-value mon-tile-rate">
                            <span className="mon-tile-dir" style={{ color: 'var(--series-2)' }} role="img" aria-label="отдача">↑</span>
                            {cur?.up != null ? rate(cur.up) : '—'}
                        </span>
                    </div>
                    <span className="mon-tile-value mon-tile-rate mon-tile-total">
                        {used != null ? vol(used) : '—'}
                        <span className="mon-tile-sub">потрачено за {RANGES.find(([id]) => id === range)?.[1]}</span>
                    </span>
                </div>
                <div className="mon-tile">
                    <span className="mon-tile-label">Устройства</span>
                    {/* Точки окрашены как серии графика «Устройства» ниже */}
                    <div className="mon-tile-rates">
                        <span className="mon-tile-value mon-tile-rate">
                            <span className="mon-tile-dot" style={{ background: 'var(--series-1)' }} aria-hidden="true" />
                            {cur?.online != null ? nf(cur.online, 0) : '—'}
                            <span className="mon-tile-sub">онлайн</span>
                        </span>
                        <span className="mon-tile-value mon-tile-rate">
                            <span className="mon-tile-dot" style={{ background: 'var(--series-2)' }} aria-hidden="true" />
                            {cur?.heavy != null ? nf(cur.heavy, 0) : '—'}
                            <span className="mon-tile-sub">в фоне · {cur?.busy ? 'канал занят' : 'канал свободен'}</span>
                        </span>
                    </div>
                </div>
                <div className="mon-tile">
                    <span className="mon-tile-label">Нагрузка</span>
                    <div className="mon-tile-rates">
                        <div className="mon-tile-meter">
                            <span className="mon-tile-value mon-tile-rate">
                                {one?.cpu != null ? `${nf(one.cpu, 0)} %` : '—'}
                                <span className="mon-tile-sub">процессор{all ? ` · в среднем по ${ov?.servers.length ?? 0}` : one?.cores ? ` · ${nf(one.cores, 0)} ядер` : ''}</span>
                            </span>
                            <Bar v={one?.cpu} label="Загрузка процессора" />
                        </div>
                        <div className="mon-tile-meter">
                            <span className="mon-tile-value mon-tile-rate">
                                {one?.mem != null ? `${nf(one.mem, 0)} %` : '—'}
                                <span className="mon-tile-sub">память{all ? ` · в среднем по ${ov?.servers.length ?? 0}` : one?.mem_total ? ` · из ${gb(one.mem_total)}` : ''}</span>
                            </span>
                            <Bar v={one?.mem} label="Занято оперативной памяти" />
                        </div>
                    </div>
                </div>
            </div>

            {/* Сводка по всем — и разбивка: кто сколько качает и насколько загружен */}
            {all && ov && ov.servers.length > 1 && (
                <section className="mon-chart mon-per-server" aria-labelledby="mon-per">
                    <div className="mon-chart-head"><span className="mon-chart-title" id="mon-per">По серверам</span></div>
                    <div className="mon-table-wrap">
                        <table className="mon-table">
                            <thead>
                                <tr><th>Сервер</th><th>Загрузка</th><th>Отдача</th><th>Онлайн</th><th>Процессор</th><th>Память</th><th>Потрачено</th></tr>
                            </thead>
                            <tbody>
                                {ov.servers.map(r => (
                                    <tr key={r.id}>
                                        <td>{r.name}{!r.ok && <span className="mon-tile-sub"> · нет метрик</span>}</td>
                                        <td>{r.down != null ? rate(r.down) : '—'}</td>
                                        <td>{r.up != null ? rate(r.up) : '—'}</td>
                                        <td>{r.online != null ? nf(r.online, 0) : '—'}</td>
                                        <td>{r.cpu != null ? `${nf(r.cpu, 0)} %` : '—'}</td>
                                        <td>{r.mem != null ? `${nf(r.mem, 0)} %` : '—'}</td>
                                        <td>{r.used_down != null && r.used_up != null ? vol(r.used_down + r.used_up) : '—'}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                </section>
            )}

            {ov && data && (
                <div className="mon-charts">
                    <LineChart title={`Трафик, ${unit.name}`} from={ov.from} to={ov.to} step={ov.step} dim={loading}
                        format={v => rate(v)} formatAxis={v => nf(v / unit.div, unit.div > 1 && v / unit.div < 10 ? 1 : 0)}
                        series={[
                            { id: 'down', name: 'загрузка', color: 'var(--series-1)', points: data.down },
                            { id: 'up',   name: 'отдача',   color: 'var(--series-2)', points: data.up },
                        ]} />
                    <LineChart title="Процессор и память, %" from={ov.from} to={ov.to} step={ov.step} dim={loading} max={100}
                        format={v => `${nf(v, 0)} %`}
                        series={[
                            { id: 'cpu', name: 'процессор', color: 'var(--series-1)', points: data.cpu },
                            { id: 'mem', name: 'память',    color: 'var(--series-2)', points: data.mem },
                        ]} />
                    <LineChart title="Устройства" from={ov.from} to={ov.to} step={ov.step} dim={loading}
                        format={v => nf(v, 0)}
                        series={[
                            { id: 'online', name: 'онлайн', color: 'var(--series-1)', points: data.online },
                            { id: 'heavy',  name: 'в фоне', color: 'var(--series-2)', points: data.heavy },
                        ]} />
                    <LineChart title="Отброшено очередью, пакетов/с" from={ov.from} to={ov.to} step={ov.step} dim={loading}
                        format={v => nf(v, 1)}
                        series={[{ id: 'drops', name: 'отброшено', color: 'var(--series-1)', points: data.drops }]} />
                </div>
            )}
        </>
    );

    return (
        <>
            {!status && <p className="mon-hint">Загрузка…</p>}
            {setup}
            {dashboard}
        </>
    );
}
