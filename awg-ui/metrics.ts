// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Prometheus: GET /metrics на ядре. Метрики ядра и всех нод собираются здесь же —
// у нод через уже открытый WSS (awg-agent пропускает только /api/*, поэтому на самих
// нодах отдельного порта для скрейпа не нужно). Доступ — отдельный токен «только
// чтение метрик»; он не открывает ни /api/v1, ни панель.
import crypto from "crypto";
import fs from "fs";
import { Router, type Request, type Response, type NextFunction } from "express";
import type Database from "better-sqlite3";

interface ServerInfo {
    id: number; name: string; local: boolean; online: boolean; pending: boolean;
    version: string | null;
    health: { gen: string; module: string; tools: string } | null;
}

interface MetricsDeps {
    uidb:    Database.Database;
    version: string;
    servers: {
        list:    () => ServerInfo[];
        isOnline: (id: number) => boolean;
        ctrlFor: (id: number) => (method: string, urlPath: string) => Promise<{ status: number; data: any }>;
    };
}

const SCRAPE_DEADLINE_MS = 5_000;
const TOUCH_EVERY_S      = 60;          // last_used пишем не чаще раза в минуту, а не на каждый скрейп

const hashToken = (t: string) => crypto.createHash("sha256").update(t).digest();

// Токен встроенного Prometheus (`awg-ctrl monitor install`): сам токен лежит только у Prometheus,
// здесь — его хэш. Файл читаем при каждой проверке, пересчитывая по mtime: установка не требует перезапуска панели.
const LOCAL_HASH_FILE = process.env.MONITOR_HASH_FILE ?? "/etc/amnezia/amneziawg/monitor_token.sha256";
let localHash: { mtime: number; hash: Buffer | null } = { mtime: -1, hash: null };
function localTokenHash(): Buffer | null {
    try {
        const mtime = fs.statSync(LOCAL_HASH_FILE).mtimeMs;
        if (mtime !== localHash.mtime) {
            const hex = fs.readFileSync(LOCAL_HASH_FILE, "utf8").trim();
            localHash = { mtime, hash: /^[0-9a-f]{64}$/.test(hex) ? Buffer.from(hex, "hex") : null };
        }
        return localHash.hash;
    } catch { localHash = { mtime: -1, hash: null }; return null; }
}

// Пер-пировые серии (метка peer = внутренний IP, name = имя vpn://-пользователя) выключены:
// это и приватность, и кардинальность. Включается явно.
const PER_PEER = (process.env.METRICS_PEERS ?? "off").toLowerCase() === "on";

type Labels = Record<string, string | number>;
type Sample = [Labels, number];

const esc = (v: string | number) => String(v).replace(/\\/g, "\\\\").replace(/\n/g, "\\n").replace(/"/g, '\\"');
const fmtLabels = (l: Labels) => {
    const parts = Object.entries(l).map(([k, v]) => `${k}="${esc(v)}"`);
    return parts.length ? `{${parts.join(",")}}` : "";
};

class Exposition {
    private lines: string[] = [];
    private seen = new Set<string>();
    add(name: string, type: "gauge" | "counter", help: string, samples: Sample[]) {
        if (!samples.length) return;
        if (!this.seen.has(name)) {
            this.seen.add(name);
            this.lines.push(`# HELP ${name} ${help.replace(/\\/g, "\\\\").replace(/\n/g, "\\n")}`, `# TYPE ${name} ${type}`);
        }
        for (const [l, v] of samples) this.lines.push(`${name}${fmtLabels(l)} ${Number.isFinite(v) ? v : 0}`);
    }
    toString() { return this.lines.join("\n") + "\n"; }
}

export function createMetrics(deps: MetricsDeps) {
    const { uidb } = deps;

    uidb.exec(`
        CREATE TABLE IF NOT EXISTS metrics_token (
            id         INTEGER PRIMARY KEY CHECK (id = 1),
            token_hash BLOB    NOT NULL,
            prefix     TEXT    NOT NULL,
            created_at INTEGER NOT NULL,
            last_used  INTEGER
        )
    `);
    const q = {
        get:    uidb.prepare("SELECT token_hash, prefix, created_at, last_used FROM metrics_token WHERE id = 1"),
        set:    uidb.prepare("INSERT OR REPLACE INTO metrics_token (id, token_hash, prefix, created_at, last_used) VALUES (1, ?, ?, ?, NULL)"),
        del:    uidb.prepare("DELETE FROM metrics_token WHERE id = 1"),
        touch:  uidb.prepare("UPDATE metrics_token SET last_used = ? WHERE id = 1"),
    };
    type TokenRow = { token_hash: Buffer; prefix: string; created_at: number; last_used: number | null };

    // ── Управление токеном (роуты панели, за JWT) ──
    const router = Router();
    const view = () => {
        const r = q.get.get() as TokenRow | undefined;
        return r ? { exists: true, prefix: r.prefix, created_at: r.created_at, last_used: r.last_used, per_peer: PER_PEER }
                 : { exists: false, per_peer: PER_PEER };
    };
    router.get("/", (_req: Request, res: Response) => { res.json(view()); });
    // Выпуск и перевыпуск — одно и то же: старый токен перестаёт работать. Значение отдаётся один раз.
    router.post("/", (_req: Request, res: Response) => {
        const token = "awgm_" + crypto.randomBytes(24).toString("base64url");
        q.set.run(hashToken(token), token.slice(0, 13) + "…", Math.floor(Date.now() / 1000));
        res.status(201).json({ ...view(), token });
    });
    router.delete("/", (_req: Request, res: Response) => { q.del.run(); res.json(view()); });

    // ── Доступ к /metrics ──
    // Нет токена или он неверный — одинаковый 401: не подсказываем, выключены метрики или токен не тот.
    function requireToken(req: Request, res: Response, next: NextFunction) {
        const h = req.headers.authorization ?? "";
        const given = h.startsWith("Bearer ") ? h.slice(7) : "";
        const row = q.get.get() as TokenRow | undefined;
        const gh = given.startsWith("awgm_") ? hashToken(given) : null;
        const panelOk = !!row && !!gh && crypto.timingSafeEqual(gh, row.token_hash);
        const local   = localTokenHash();
        const localOk = !!gh && !!local && crypto.timingSafeEqual(gh, local);
        if (!panelOk && !localOk) { res.status(401).set("WWW-Authenticate", "Bearer").type("text/plain").send("unauthorized\n"); return; }
        // last_used — про токен, выпущенный в панели (внешний Prometheus); встроенный в нём не участвует.
        const now = Math.floor(Date.now() / 1000);
        if (panelOk && row && (!row.last_used || now - row.last_used >= TOUCH_EVERY_S)) q.touch.run(now);
        next();
    }

    // Один сервер за скрейп: сам сервер не отвечает — остальные не страдают.
    async function scrapeServer(s: ServerInfo): Promise<any | null> {
        if (s.pending || !deps.servers.isOnline(s.id)) return null;
        let timer: NodeJS.Timeout | undefined;
        try {
            const r = await Promise.race([
                deps.servers.ctrlFor(s.id)("GET", "/api/metrics"),
                new Promise<never>((_, rej) => { timer = setTimeout(() => rej(new Error("deadline")), SCRAPE_DEADLINE_MS); }),
            ]);
            return r.status === 200 ? r.data : null;
        } catch { return null; }
        finally { clearTimeout(timer); }
    }

    async function render(): Promise<string> {
        const t0 = process.hrtime.bigint();
        const list = deps.servers.list();
        const data = await Promise.all(list.map(scrapeServer));
        const out = new Exposition();
        const L = (s: ServerInfo, extra: Labels = {}): Labels => ({ server: s.name, server_id: s.id, ...extra });
        // Серии только тех серверов, чьи метрики получены; fn всегда отдаёт массив серий.
        const per = (fn: (s: ServerInfo, d: any) => Sample[]): Sample[] =>
            list.flatMap((s, i) => (data[i] ? fn(s, data[i]) : []));

        out.add("awg_panel_info", "gauge", "Версия панели Forgetting.", [[{ version: deps.version }, 1]]);
        out.add("awg_servers", "gauge", "Серверов в панели (ядро и ноды).", [[{}, list.length]]);
        out.add("awg_server_online", "gauge", "Сервер на связи с панелью (1/0).", list.map(s => [L(s), s.online ? 1 : 0] as Sample));
        out.add("awg_scrape_ok", "gauge", "Метрики сервера получены в этом скрейпе (1/0).", list.map((s, i) => [L(s), data[i] ? 1 : 0] as Sample));
        out.add("awg_node_info", "gauge", "Версия Forgetting и поколение AWG на сервере.",
            list.filter(s => !s.pending).map(s => [L(s, { version: s.version ?? "", gen: s.health?.gen ?? "", module: s.health?.module ?? "", tools: s.health?.tools ?? "" }), 1] as Sample));

        out.add("awg_up", "gauge", "Интерфейс AWG поднят (1/0).", per((s, d) => [[L(s), d.up ? 1 : 0] as Sample]));
        out.add("awg_peers", "gauge", "Пиров в интерфейсе.", per((s, d) => [[L(s), d.peers?.total ?? 0] as Sample]));
        out.add("awg_peers_online", "gauge", "Пиров с рукопожатием за последние 3 минуты.", per((s, d) => [[L(s), d.peers?.online ?? 0] as Sample]));
        // Сумма по пирам — gauge, а не counter: при удалении пира или смене его ключа сумма уменьшается,
        // и rate() принял бы это за сброс счётчика с ложным всплеском. Для скорости берите awg_link_*_bytes_total.
        out.add("awg_peers_received_bytes", "gauge", "Принято от клиентов (их отдача), байт, сумма по пирам. Уменьшается при удалении пира.", per((s, d) => [[L(s), d.rx_bytes ?? 0] as Sample]));
        out.add("awg_peers_sent_bytes", "gauge", "Отправлено клиентам (их загрузка), байт, сумма по пирам. Уменьшается при удалении пира.", per((s, d) => [[L(s), d.tx_bytes ?? 0] as Sample]));
        for (const [k, name, help] of [
            ["rx_bytes",   "receive_bytes",    "байт"], ["tx_bytes",   "transmit_bytes",   "байт"],
            ["rx_packets", "receive_packets",  "пакетов"], ["tx_packets", "transmit_packets", "пакетов"],
            ["rx_dropped", "receive_dropped",  "потерянных пакетов"], ["tx_dropped", "transmit_dropped", "потерянных пакетов"],
            ["rx_errors",  "receive_errors",   "ошибок"], ["tx_errors",  "transmit_errors",  "ошибок"],
        ] as const) {
            out.add(`awg_link_${name}_total`, "counter", `Интерфейс awg1: ${help} (${k}).`, per((s, d) => [[L(s), d.link?.[k] ?? 0] as Sample]));
        }

        // Нагрузка хоста (ядро и ноды). CPU — счётчики секунд по режимам: доля = rate() в Prometheus.
        out.add("awg_host_cpu_seconds_total", "counter", "Время процессора по режимам, секунд (как node_exporter).",
            per((s, d) => Object.entries(d.host?.cpu ?? {}).map(([mode, v]) => [L(s, { mode }), Number(v)] as Sample)));
        out.add("awg_host_cpu_cores", "gauge", "Ядер процессора.", per((s, d) => d.host ? [[L(s), d.host.cores ?? 0] as Sample] : []));
        out.add("awg_host_memory_total_bytes", "gauge", "Оперативной памяти всего, байт.", per((s, d) => d.host ? [[L(s), d.host.mem?.total ?? 0] as Sample] : []));
        out.add("awg_host_memory_available_bytes", "gauge", "Доступной памяти (MemAvailable), байт.", per((s, d) => d.host ? [[L(s), d.host.mem?.available ?? 0] as Sample] : []));
        out.add("awg_host_load", "gauge", "Средняя загрузка системы (load average).",
            per((s, d) => d.host ? (["1m", "5m", "15m"] as const).map((p, i) => [L(s, { period: p }), d.host.load?.[i] ?? 0] as Sample) : []));

        // Деление канала (CAKE)
        const sh = (fn: (d: any) => number) => per((s, d) => d.shaping ? [[L(s), fn(d.shaping)] as Sample] : []);
        out.add("awg_shaping_enabled", "gauge", "Деление канала включено в настройках (1/0).", sh(x => x.settings?.enabled ? 1 : 0));
        out.add("awg_shaping_active", "gauge", "CAKE реально стоит на интерфейсе (1/0).", sh(x => x.active ? 1 : 0));
        out.add("awg_shaping_congested", "gauge", "Канал считается перегруженным (1/0).", sh(x => x.congested ? 1 : 0));
        out.add("awg_shaping_heavy_devices", "gauge", "Устройств в фоновом классе.", sh(x => x.heavy?.length ?? 0));
        out.add("awg_shaping_active_devices", "gauge", "Активных устройств в окне наблюдения.", sh(x => x.devices ?? 0));
        const dirs = (fn: (x: any, dir: "down" | "up") => number | undefined) =>
            per((s, d) => d.shaping ? (["down", "up"] as const).flatMap(dir => {
                const v = fn(d.shaping, dir);
                return v === undefined ? [] : [[L(s, { direction: dir }), v] as Sample];
            }) : []);
        out.add("awg_shaping_bandwidth_bits", "gauge", "Заданная ширина канала, бит/с.", dirs((x, dir) => (dir === "down" ? x.settings?.downMbit : x.settings?.upMbit) * 1e6));
        out.add("awg_shaping_rate_bits", "gauge", "Скорость за последний тик сэмплера, бит/с.", dirs((x, dir) => (dir === "down" ? x.rateDown : x.rateUp) * 1e6));
        out.add("awg_shaping_qdisc_drops_total", "counter", "Пакетов, отброшенных CAKE.", dirs((x, dir) => x[dir]?.drops));
        out.add("awg_shaping_qdisc_backlog_bytes", "gauge", "Байт в очереди CAKE.", dirs((x, dir) => x[dir]?.backlog));

        if (PER_PEER) {
            const peers = (fn: (p: any) => number) => per((s, d) => (d.list ?? []).map((p: any) =>
                [L(s, { peer: p.ip, name: p.name ?? "", kind: p.kind }), fn(p)] as Sample));
            out.add("awg_peer_online", "gauge", "Пир на связи (1/0).", peers(p => p.online ? 1 : 0));
            out.add("awg_peer_background", "gauge", "Пир в фоновом классе: канал занят, а он давно качает больше своей доли (1/0).", peers(p => p.priority === "background" ? 1 : 0));
            out.add("awg_peer_received_bytes_total", "counter", "Принято от пира, байт.", peers(p => p.rx));
            out.add("awg_peer_sent_bytes_total", "counter", "Отправлено пиру, байт.", peers(p => p.tx));
        }

        let masters = 0, devices = 0;
        // Таблицы создаёт sub: готовим запросы здесь, а не при создании модуля, — порядок
        // инициализации в server.ts не должен решать, упадёт ли панель на старте.
        try {
            masters = (uidb.prepare("SELECT COUNT(*) AS n FROM master_keys WHERE revoked_at IS NULL").get() as { n: number }).n;
            devices = (uidb.prepare("SELECT COUNT(*) AS n FROM devices").get() as { n: number }).n;
        } catch { /* sub ещё не создал таблицы */ }
        out.add("awg_master_keys", "gauge", "Мастер-ключей sen:// (без удаляемых).", [[{}, masters]]);
        out.add("awg_devices", "gauge", "Устройств sen://.", [[{}, devices]]);
        out.add("awg_scrape_duration_seconds", "gauge", "Сколько панель собирала метрики.", [[{}, Number(process.hrtime.bigint() - t0) / 1e9]]);
        return out.toString();
    }

    async function handler(_req: Request, res: Response) {
        try {
            res.type("text/plain; version=0.0.4; charset=utf-8").send(await render());
        } catch (e) {
            console.warn(`metrics: ${(e as Error).message}`);
            res.status(500).type("text/plain").send("error\n");
        }
    }

    return { router, requireToken, handler };
}
