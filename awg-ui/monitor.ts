// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Вкладка «Монитор»: встроенный Prometheus и основные графики прямо в панели.
//   GET  /ui/monitor            — готов ли Prometheus, можно ли поставить его кнопкой, ход установки
//   POST /ui/monitor/install    — попросить локальный awg-ctrl выполнить `awg-ctrl monitor install` (root)
//   POST /ui/monitor/remove     — то же для `awg-ctrl monitor remove` (удаление вместе с накопленными данными)
//   GET  /ui/monitor/overview   — серии для графиков за 1 ч / 6 ч / 24 ч
// Панель сама пакетов не ставит и в Prometheus лезет только фиксированными запросами (не PromQL от клиента).
import axios from "axios";
import { Router, type Request, type Response } from "express";

type Ctrl = (method: string, urlPath: string, body?: unknown) => Promise<{ status: number; data: any }>;

const PROM_URL = process.env.PROM_URL ?? "http://127.0.0.1:9090";

// Шаг подбираем так, чтобы на графике было ~250–360 точек; окно rate — минимум два скрейпа (15 с).
// Дальше 30 дней не заглядываем: столько Prometheus хранит (RETENTION в cli/src/monitor.ts).
const RANGES: Record<string, { seconds: number; step: number }> = {
    "1h":  { seconds: 3600,    step: 15 },
    "6h":  { seconds: 21600,   step: 60 },
    "24h": { seconds: 86400,   step: 300 },
    "7d":  { seconds: 604800,  step: 1800 },
    "30d": { seconds: 2592000, step: 7200 },
};
// Окно rate() — не короче шага: иначе на неделе/месяце точка была бы случайной двухминутной выборкой
// из двух часов, а не средним за них. Значения «сейчас» при этом считаются отдельно, по окну 2m.
const RATE_WINDOW = "2m";
const withWindow = (q: string, w: string) => q.split(`[${RATE_WINDOW}]`).join(`[${w}]`);

type Point  = [number, number];
type Series = Map<number, { name: string; points: Point[] }>;   // server_id → ряд

// Запросы по серверам. Трафик берём со счётчиков интерфейса (монотонны), а не с сумм по пирам:
// сумма по пирам падает при удалении пира, и rate() рисовал бы ложный всплеск.
const QUERIES = {
    down:      'sum by (server_id, server) (rate(awg_link_transmit_bytes_total[2m])) * 8',
    up:        'sum by (server_id, server) (rate(awg_link_receive_bytes_total[2m])) * 8',
    online:    'sum by (server_id, server) (awg_peers_online)',
    heavy:     'sum by (server_id, server) (awg_shaping_heavy_devices)',
    drops:     'sum by (server_id, server) (rate(awg_shaping_qdisc_drops_total[2m]))',
    congested: 'max by (server_id, server) (awg_shaping_congested)',
    scrape_ok: 'max by (server_id, server) (awg_scrape_ok)',
    // Загрузка процессора: доля времени не в idle/iowait. Память: MemAvailable, а не MemFree (кэш ядра свободен).
    cpu: '100 * (1 - sum by (server_id, server) (rate(awg_host_cpu_seconds_total{mode=~"idle|iowait"}[2m])) / sum by (server_id, server) (rate(awg_host_cpu_seconds_total[2m])))',
    mem: '100 * (1 - sum by (server_id, server) (awg_host_memory_available_bytes) / sum by (server_id, server) (awg_host_memory_total_bytes))',
    cores:     'max by (server_id, server) (awg_host_cpu_cores)',
    mem_total: 'max by (server_id, server) (awg_host_memory_total_bytes)',
} as const;
type Key = keyof typeof QUERIES;

async function instantQuery(query: string, time: number): Promise<Map<number, number>> {
    const r = await axios.get(`${PROM_URL}/api/v1/query`, { params: { query, time }, timeout: 8000 });
    const out = new Map<number, number>();
    for (const m of (r.data?.data?.result ?? []) as { metric: Record<string, string>; value: [number, string] }[]) {
        const id = Number(m.metric.server_id), v = Number(m.value[1]);
        if (Number.isInteger(id) && Number.isFinite(v)) out.set(id, v);
    }
    return out;
}

async function rangeQuery(query: string, start: number, end: number, step: number): Promise<Series> {
    const r = await axios.get(`${PROM_URL}/api/v1/query_range`, {
        params: { query, start, end, step }, timeout: 8000,
    });
    const out: Series = new Map();
    for (const m of (r.data?.data?.result ?? []) as { metric: Record<string, string>; values: [number, string][] }[]) {
        const id = Number(m.metric.server_id);
        if (!Number.isInteger(id)) continue;
        out.set(id, {
            name: m.metric.server ?? String(id),
            // NaN/Inf из деления на ноль отбрасываем: на графике это дыра, а не нулевая скорость
            points: m.values.map(([t, v]) => [t, Number(v)] as Point).filter(p => Number.isFinite(p[1])),
        });
    }
    return out;
}

// Сумма рядов по меткам времени (Prometheus выравнивает точки по шагу, метки совпадают).
function total(series: Series, how: "sum" | "max" = "sum"): Point[] {
    const acc = new Map<number, number>();
    for (const s of series.values()) for (const [t, v] of s.points)
        acc.set(t, how === "sum" ? (acc.get(t) ?? 0) + v : Math.max(acc.get(t) ?? 0, v));
    return [...acc.entries()].sort((a, b) => a[0] - b[0]);
}

const last = (s?: { points: Point[] }): number | null => s && s.points.length ? s.points[s.points.length - 1][1] : null;

export function createMonitor(deps: { ctrlLocal: Ctrl; localEnabled: boolean }) {
    const router = Router();

    const promReady = async (): Promise<boolean> => {
        try { return (await axios.get(`${PROM_URL}/-/ready`, { timeout: 1500, validateStatus: () => true })).status === 200; }
        catch { return false; }
    };

    // Установка возможна, только если рядом свой awg-ctrl (у панели без собственного VPN его нет —
    // тогда остаётся команда в терминале) и он достаточно новый, чтобы знать маршрут.
    type Job = { can: boolean; action: "install" | "remove" | null; running: boolean; exit: number | null; log: string };
    async function jobState(): Promise<Job> {
        const none: Job = { can: false, action: null, running: false, exit: null, log: "" };
        if (!deps.localEnabled) return none;
        try {
            const r = await deps.ctrlLocal("GET", "/api/monitor/job");
            if (r.status !== 200) return none;
            return { can: true, action: r.data.action ?? null, running: !!r.data.running, exit: r.data.exit ?? null, log: String(r.data.log ?? "") };
        } catch { return none; }
    }

    router.get("/", async (_req: Request, res: Response) => {
        const [ready, job] = await Promise.all([promReady(), jobState()]);
        res.json({
            ready,
            can_install: job.can,
            // Последняя операция (установка или удаление) и её лог; null — ещё не запускали
            job: job.can && job.action ? { action: job.action, running: job.running, exit: job.exit, log: job.log } : null,
            command: "awg-ctrl monitor install",
        });
    });

    // Установка и удаление — одно и то же: просим локальный awg-ctrl, он запускает CLI от root.
    const ask = (action: "install" | "remove") => async (_req: Request, res: Response) => {
        if (!deps.localEnabled) { res.status(409).json({ error: "У этой панели нет своего VPN-сервера: выполните команду на машине" }); return; }
        try {
            const r = await deps.ctrlLocal("POST", `/api/monitor/${action}`);
            res.status(r.status === 404 ? 501 : r.status).json(
                r.status === 404 ? { error: "awg-ctrl старой версии не умеет этого: обновите сервер или выполните команду вручную" } : r.data);
        } catch { res.status(502).json({ error: "awg-ctrl недоступен" }); }
    };
    router.post("/install", ask("install"));
    router.post("/remove",  ask("remove"));

    router.get("/overview", async (req: Request, res: Response) => {
        const range = RANGES[String(req.query.range ?? "1h")];
        if (!range) { res.status(400).json({ error: "range: 1h, 6h, 24h, 7d или 30d" }); return; }
        const end   = Math.floor(Date.now() / 1000);
        const start = end - range.seconds;
        try {
            const keys = Object.keys(QUERIES) as Key[];
            const window = range.step > 120 ? `${range.step}s` : RATE_WINDOW;
            const got  = await Promise.all(keys.map(k => rangeQuery(withWindow(QUERIES[k], window), start, end, range.step)));
            // Окно расширено — «сейчас» с конца ряда было бы средним за шаг; берём мгновенные значения по 2m.
            const rated: Key[] = ["down", "up", "cpu"];
            const now   = window === RATE_WINDOW ? null
                : new Map(await Promise.all(rated.map(async k => [k, await instantQuery(QUERIES[k], end)] as const)));
            // Вкладка показывает один сервер — тот, что выбран в сервер-баре (?server=<id>).
            const only = Number.isInteger(Number(req.query.server)) && req.query.server !== undefined ? Number(req.query.server) : null;
            const s = Object.fromEntries(keys.map((k, i) => [k,
                only === null ? got[i] : new Map([...got[i]].filter(([id]) => id === only))])) as Record<Key, Series>;

            // Список серверов — из любого ряда, где они есть; «последнее значение» берём с конца окна.
            const ids = new Set<number>();
            for (const k of keys) for (const id of s[k].keys()) ids.add(id);
            // Объём за весь период: increase() по тем же монотонным счётчикам интерфейса (сбросы счётчика он учитывает)
            const [usedDown, usedUp] = await Promise.all([
                instantQuery(`sum by (server_id, server) (increase(awg_link_transmit_bytes_total[${range.seconds}s]))`, end),
                instantQuery(`sum by (server_id, server) (increase(awg_link_receive_bytes_total[${range.seconds}s]))`, end),
            ]);
            const cur = (k: Key, id: number) => now?.has(k) ? (now.get(k)!.get(id) ?? null) : last(s[k].get(id));
            const servers = [...ids].sort((a, b) => a - b).map(id => ({
                id,
                name:      (s.scrape_ok.get(id) ?? s.down.get(id) ?? s.online.get(id))?.name ?? String(id),
                ok:        last(s.scrape_ok.get(id)) === 1,
                down:      cur("down", id),
                up:        cur("up", id),
                online:    last(s.online.get(id)),
                heavy:     last(s.heavy.get(id)),
                congested: last(s.congested.get(id)) === 1,
                cpu:       cur("cpu", id),
                mem:       last(s.mem.get(id)),
                cores:     last(s.cores.get(id)),
                mem_total: last(s.mem_total.get(id)),
                used_down: usedDown.get(id) ?? null,
                used_up:   usedUp.get(id) ?? null,
            }));


            res.json({
                range: req.query.range ?? "1h", step: range.step, from: start, to: end,
                servers,
                total: {
                    down: total(s.down), up: total(s.up), online: total(s.online),
                    heavy: total(s.heavy), drops: total(s.drops),
                    cpu: total(s.cpu, "max"), mem: total(s.mem, "max"),
                },
            });
        } catch (e) {
            // Prometheus стоит, но не ответил: график важнее ошибки, но причину оператору покажем
            res.status(502).json({ error: axios.isAxiosError(e) ? "Prometheus не отвечает" : (e as Error).message });
        }
    });

    return { router };
}
