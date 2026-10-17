// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback, useRef } from 'react';
import { apiFetch, copyText, getServerId, type NodeInfo, type PageProps } from '../../lib/shared';
import { IcoCopy, IcoTrash } from '../../components/icons';
import LineChart from '../../components/LineChart';
import './monitor.css';

// Вкладка «Монитор»: встроенный Prometheus и основные графики.
// Кнопка «Подключить Prometheus» просит локальный awg-ctrl поставить его (`awg-ctrl monitor install`, root;
// сама панель пакетов не ставит). Графики читают Prometheus через фиксированные запросы панели
// (awg-ui/monitor.ts).
interface Status {
    ready: boolean; can_install: boolean; command: string;
    job: { action: 'install' | 'remove'; running: boolean; exit: number | null; log: string } | null;   // последняя операция
}
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
// Прогресс операции из строк «N/M  шаг», которые печатает сам установщик (cli/src/monitor.ts). Шаг N считаем
// выполненным наполовину: так полоска не стоит на нуле, пока идёт долгий apt, и не обещает больше, чем известно.
const STEP = /^(\d+)\/(\d+)\s+(.+)$/;
function JobProgress({ job, starting }: { job: Status['job']; starting: boolean }) {
    let step: [number, number, string] | null = null;
    for (const l of job?.log.split('\n') ?? []) { const m = STEP.exec(l.trim()); if (m) step = [Number(m[1]), Number(m[2]), m[3]]; }
    const failed = !!job && job.exit !== null && job.exit !== 0;
    const done   = !!job && job.exit === 0;
    const pct    = done ? 100 : step ? ((step[0] - 0.5) / step[1]) * 100 : 0;
    const label  = failed ? `Не удалось (код ${job!.exit})`
        : done ? (job!.action === 'install' ? 'Готово' : 'Удалено')
        : step ? `Шаг ${step[0]} из ${step[1]}: ${step[2]}` : 'Запуск…';
    return (
        <div className="mon-progress" aria-live="polite">
            <span className={`mon-pbar${failed ? ' mon-pbar--bad' : done ? ' mon-pbar--ok' : !step && (starting || job?.running) ? ' mon-pbar--wait' : ''}`}
                role="progressbar" aria-label="Ход операции" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
                <i style={{ width: `${pct}%` }} />
            </span>
            <span className="mon-progress-label">{label}</span>
        </div>
    );
}
const gb = (b: number) => `${nf(b / 2 ** 30, 1)} ГБ`;
// Объём трафика — десятичные единицы, как считают провайдеры: 1 ТБ = 10¹² байт
function vol(b: number): string {
    if (b >= 1e12) return `${nf(b / 1e12, 2)} ТБ`;
    if (b >= 1e9)  return `${nf(b / 1e9, 1)} ГБ`;
    return `${nf(b / 1e6, 0)} МБ`;
}

export default function MonitorPage({ token, showMsg }: PageProps) {
    const [status, setStatus]   = useState<Status | null>(null);
    const [range, setRange]     = useState('1h');
    const [ov, setOv]           = useState<Overview | null>(null);
    const [ovErr, setOvErr]     = useState('');
    const [loading, setLoading] = useState(false);
    const [pending, setPending] = useState<'install' | 'remove' | null>(null);     // операция запущена из этой вкладки
    const [server, setServer] = useState<NodeInfo | null>(null);     // показанный сервер: выбран в другой вкладке, здесь не меняется
    const alive = useRef(true);
    // StrictMode монтирует эффект дважды: флаг надо вернуть в true, иначе все ответы отбрасываются
    useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

    const loadStatus = useCallback(async () => {
        try { const s = await apiFetch('GET', '/ui/monitor', token) as Status; if (alive.current) setStatus(s); return s; }
        catch { return null; }
    }, [token]);

    const loadOverview = useCallback(async () => {
        setLoading(true);
        try {
            const o = await apiFetch('GET', `/ui/monitor/overview?range=${range}&server=${getServerId() ?? 0}`, token) as Overview;
            if (alive.current) { setOv(o); setOvErr(''); }
        } catch (e: any) { if (alive.current) setOvErr(e?.response?.data?.error ?? 'Не удалось получить данные'); }
        finally { if (alive.current) setLoading(false); }
    }, [token, range]);

    useEffect(() => { void loadStatus(); }, [loadStatus]);

    // Название и состояние сервера для заголовка; список серверов на этой вкладке не показываем
    useEffect(() => {
        const load = () => apiFetch('GET', '/ui/nodes', token)
            .then(r => { if (alive.current) setServer((r.nodes as NodeInfo[]).find(n => n.id === (getServerId() ?? 0)) ?? null); })
            .catch(() => { /* заголовок необязателен */ });
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

    // Пока идёт установка или удаление — следим за ходом: раз в 2 с читаем статус и лог
    useEffect(() => {
        if (!pending && !status?.job?.running) return;
        const t = setInterval(async () => {
            const s = await loadStatus();
            if (s && !s.job?.running) setPending(null);
        }, 2000);
        return () => clearInterval(t);
    }, [pending, status?.job?.running, loadStatus]);

    const run = async (action: 'install' | 'remove') => {
        if (action === 'remove' && !confirm('Удалить Prometheus? Он будет остановлен, а накопленные графики (до 31 дня) сотрутся. Панель продолжит отдавать /metrics.')) return;
        setPending(action);
        try { await apiFetch('POST', `/ui/monitor/${action}`, token); await loadStatus(); }
        catch (e: any) { setPending(null); showMsg(e?.response?.data?.error ?? (action === 'install' ? 'Не удалось запустить установку' : 'Не удалось запустить удаление')); }
    };
    const working = !!pending || !!status?.job?.running;

    // Прогресс рядом с кнопкой: для установки — пока Prometheus нет (там же виден итог удаления), для удаления — только удаление
    const progressFor = (action: 'install' | 'remove' | 'any') => {
        const j = status?.job;
        const mine = !!j && (action === 'any' || j.action === action) && (j.running || j.exit !== null);
        const starting = !!pending && (action === 'any' || action === pending);
        return mine || starting ? <JobProgress job={mine ? j! : null} starting={starting} /> : null;
    };

    // Полный лог — только при ошибке: оператору нужна причина, а не поток строк apt
    const failureLog = () => {
        const j = status?.job;
        if (!j || j.exit === null || j.exit === 0) return null;
        return (
            <>
                <pre className="mon-log">{j.log.trim()}</pre>
                <p className="login-error">
                    {j.action === 'install' ? 'Установка' : 'Удаление'} не удалась. Если дело в правах или пакетах, выполните вручную: <b>{`awg-ctrl monitor ${j.action}`}</b>
                </p>
            </>
        );
    };

    // ───────── установка ─────────
    const setup = status && !status.ready && (
        <section className="mon-card" aria-labelledby="mon-setup">
            <div className="mon-head">
                <h3 className="mon-title" id="mon-setup">Мониторинг</h3>
                <span className="chip chip--offline"><span className="chip-dot" />не подключён</span>
            </div>
            <p className="mon-hint">
                Панель поставит Prometheus на этот сервер и сама подключит его к своим метрикам: основные графики —
                скорость, устройства онлайн, нагрузка на канал, — появятся здесь через минуту-две. Prometheus слушает
                только 127.0.0.1, наружу он не открывается.
            </p>

            {status.can_install ? (
                <div className="mon-actions">
                    <button className="btn btn--primary" onClick={() => run('install')} disabled={working}>
                        {pending === 'install' || (status.job?.running && status.job.action === 'install') ? 'Устанавливаем…' : 'Подключить Prometheus'}
                    </button>
                    {progressFor('any')}
                </div>
            ) : (
                <>
                    <p className="mon-hint">Выполните на сервере от root (у этой панели нет своего awg-ctrl, который мог бы сделать это сам):</p>
                    <code className="mon-code">{status.command}</code>
                    <div className="mon-actions">
                        <button className="btn btn--tonal" onClick={() => copyText(status.command).then(() => showMsg('Команда скопирована'))}>
                            <IcoCopy /> Скопировать
                        </button>
                    </div>
                </>
            )}

            {failureLog()}
        </section>
    );

    // Удаление: только пока Prometheus есть
    const manage = status?.ready && (
        <section className="mon-card" aria-labelledby="mon-manage">
            <div className="mon-head">
                <h3 className="mon-title" id="mon-manage">Prometheus</h3>
                <span className="chip chip--online"><span className="chip-dot" />работает</span>
            </div>
            <p className="mon-hint">
                Стоит на этом сервере и слушает только 127.0.0.1:9090, графики хранит 31 день. Удаление остановит его, снимет
                пакет и сотрёт накопленные данные.
            </p>
            {status.can_install ? (
                <div className="mon-actions">
                    <button className="btn btn--danger" onClick={() => run('remove')} disabled={working}>
                        <IcoTrash /> {pending === 'remove' || (status.job?.running && status.job.action === 'remove') ? 'Удаляем…' : 'Удалить Prometheus'}
                    </button>
                    {progressFor('remove')}
                </div>
            ) : (
                <>
                    <p className="mon-hint">Выполните на сервере от root:</p>
                    <code className="mon-code">awg-ctrl monitor remove</code>
                </>
            )}
            {status.job?.action === 'remove' && failureLog()}
        </section>
    );

    // ───────── дашборд ─────────
    // Вкладка показывает один сервер — выбранный в сервер-баре; при смене сервера App монтирует её заново.
    const one  = ov?.servers[0];
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
            {/* Период — единственный фильтр: он задаёт окно для всего ниже */}
            <div className="mon-filters" role="group" aria-label="Период">
                {RANGES.map(([id, label]) => (
                    <button key={id} type="button" className={`mon-range${range === id ? ' mon-range--on' : ''}`}
                        aria-pressed={range === id} onClick={() => setRange(id)}>{label}</button>
                ))}
            </div>

            {ovErr && <p className="login-error">{ovErr}</p>}
            {ov && !one?.ok && (
                <p className="mon-hint mon-notice">Этот сервер сейчас не отдаёт метрики: графики могут быть неполными или пустыми.</p>
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
                                <span className="mon-tile-sub">процессор{one?.cores ? ` · ${nf(one.cores, 0)} ядер` : ''}</span>
                            </span>
                            <Bar v={one?.cpu} label="Загрузка процессора" />
                        </div>
                        <div className="mon-tile-meter">
                            <span className="mon-tile-value mon-tile-rate">
                                {one?.mem != null ? `${nf(one.mem, 0)} %` : '—'}
                                <span className="mon-tile-sub">память{one?.mem_total ? ` · из ${gb(one.mem_total)}` : ''}</span>
                            </span>
                            <Bar v={one?.mem} label="Занято оперативной памяти" />
                        </div>
                    </div>
                </div>
            </div>

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
            {server && (
                <p className="mon-server">
                    <span className="mon-server-label">Сервер</span>
                    <b>{server.health?.server || server.name}</b>
                    <span className={`chip chip--${server.online ? 'online' : 'offline'}`}><span className="chip-dot" />{server.online ? 'на связи' : 'нет связи'}</span>
                </p>
            )}

            {!status && <p className="mon-hint">Загрузка…</p>}
            {setup}
            {dashboard}
            {manage}
        </>
    );
}
