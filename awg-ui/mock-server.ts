// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Локальный мок для разработки фронтенда: эмулирует awg-ui + awg-ctrl целиком в
// памяти, AWG не нужен. Логин admin / admin. Запуск: npm run dev:mock.
import express from "express";
import crypto from "crypto";
import { encodeSenLink } from "./sen";

const app = express();
app.use(express.json());

const now = () => Math.floor(Date.now() / 1000);
const rnd = (n: number) => Math.floor(Math.random() * n);
const b64 = (n: number) => crypto.randomBytes(n).toString("base64");
const GEN = "3.1";
const MOCK_VERSION = "0.3.3";

// ── пользователи (vpn://) ──
interface U { name: string; ip: string; pub_key: string; vpn_key: string; key_gen: string; online: boolean; lastHandshake: number; rx: number; tx: number }
const users: U[] = ["alice", "bob", "laptop-ivan"].map((name, i) => ({
    name, ip: `10.9.0.${i + 2}`, pub_key: b64(32), vpn_key: "vpn://" + crypto.randomBytes(48).toString("base64url"),
    key_gen: GEN, online: i !== 1, lastHandshake: now() - rnd(600), rx: rnd(5e8), tx: rnd(2e8),
}));

// ── серверы: core (id 0), нода в сети и нода, ещё не подключавшаяся ──
// Пользователи хранятся по серверам; X-Server-Id выбирает набор, как в настоящей панели.
const usersOf: Record<number, U[]> = {
    0: users,
    1: ["dave", "erin"].map((name, i) => ({
        name, ip: `10.9.0.${i + 2}`, pub_key: b64(32), vpn_key: "vpn://" + crypto.randomBytes(48).toString("base64url"),
        key_gen: "2", online: i === 0, lastHandshake: now() - rnd(600), rx: rnd(5e8), tx: rnd(2e8),
    })),
};
const srv = (q: express.Request) => { const n = Number(q.headers["x-server-id"]); return Number.isInteger(n) && usersOf[n] ? n : 0; };
const U_ = (q: express.Request) => usersOf[srv(q)];
interface MNode { id: number; name: string; ip: string; gen: string; online: boolean; pending: boolean; version: string | null }
const mnodes: MNode[] = [
    { id: 1, name: "Frankfurt", ip: "198.51.100.4", gen: "2", online: true, pending: false, version: "0.3.2" },   // отстаёт от панели
    { id: 2, name: "Tokyo", ip: "", gen: "2", online: false, pending: true, version: null },
    { id: 3, name: "Amsterdam", ip: "192.0.2.10", gen: "3.1", online: true, pending: false, version: null },  // старый агент: версию не шлёт
];
let nodeId = 3;
const joinStr = (id: number) => "awgjoin://" + Buffer.from(JSON.stringify({ v: 1, h: "localhost", p: 8443, pin: crypto.randomBytes(32).toString("base64url"), n: id, s: crypto.randomBytes(16).toString("base64url") })).toString("base64url");

// ── API-ключи ──
const apiKeys = [{ id: 1, label: "ci-bot", prefix: "awgk_Xk2mQ9aB…", server_id: 0, servers: [0], created_at: now() - 86400 * 5, last_used: now() - 3600 as number | null }];
let apiId = 1;

// ── мастер-ключи sen:// ──
interface Dev { id: number; device_id: string; device_name: string; platform: string; version: string; created_at: number; last_seen: number | null; rekey_requested: boolean; online: boolean; lastHandshake: number; rx: number; tx: number }
interface MK { id: number; uuid: string; label: string; device_limit: number; created_at: number; secret: Buffer; devs: Dev[]; servers: number[]; deleting?: number[] }
let mkId = 1, devId = 1;
const dev = (name: string, platform: string, online: boolean, version = "0.6.5"): Dev => ({ id: devId++, device_id: crypto.randomUUID(), device_name: name, platform, version, created_at: now() - 86400, last_seen: now() - rnd(900), rekey_requested: false, online, lastHandshake: now() - rnd(300), rx: rnd(9e8), tx: rnd(3e8) });
const masters: MK[] = [{ id: mkId++, uuid: crypto.randomUUID(), label: "Семья", device_limit: 3, created_at: now() - 86400 * 2, secret: crypto.randomBytes(16), devs: [dev("MacBook Ивана", "macOS", true), dev("Windows-ПК", "Windows", false, "0.6.2")], servers: [0, 1] }];
const signPub = crypto.randomBytes(32);
const mkOut = (m: MK) => ({ id: m.id, uuid: m.uuid, label: m.label, device_limit: m.device_limit, devices: m.devs.length, servers: m.servers, created_at: m.created_at, ...(m.deleting ? { deleting: m.deleting } : {}) });
// Мок: устройство «догнано» на всех серверах ключа, кроме Tokyo (id 2 — ещё не подключалась).
const devTorrent = (d: Dev) => !!torrentOf[0]?.enabled && d.id === masters[0]?.devs[1]?.id;
const devOut = (m: MK, d: Dev) => ({ ...d,
    priority: shapingOf[0]?.enabled && (d.id === masters[0]?.devs[0]?.id || devTorrent(d)) ? "background" : "normal",
    torrent: devTorrent(d), servers_total: m.servers.length, servers_ok: m.servers.filter(s => s !== 2).length });
const find = (id: string) => masters.find(m => m.id === Number(id) && !m.deleting);

app.get("/ui/brand", (_q, r) => r.json({ brand: "Forgetting", channel: "Beta", version: MOCK_VERSION }));
app.post("/login", (q, r) => q.body.user === "admin" && q.body.pass === "admin" ? r.json({ token: "mock." + crypto.randomBytes(8).toString("hex") + ".sig" }) : r.status(401).json({ error: "Неверный логин или пароль" }));
app.post("/logout", (_q, r) => r.json({ ok: true }));
app.get("/health", (q, r) => {
    const id = srv(q), n = mnodes.find(x => x.id === id);
    r.json({ status: "ok", server: n?.name ?? "VPN (mock)", ip: n?.ip ?? "203.0.113.7", gen: n?.gen ?? GEN, awg: { status: "ok", peers: usersOf[id].length, module: "3.0.1", tools: "1.0.0" } });
});

// ── серверы ──
const nodeView = () => [
    { id: 0, name: "VPN (mock)", local: true, online: true, pending: false, last_seen: null, version: MOCK_VERSION, health: { server: "VPN (mock)", ip: "203.0.113.7", gen: GEN, peers: users.length, awg_up: true, module: "3.0.1", tools: "1.0.0" } },
    ...mnodes.map(n => ({ id: n.id, name: n.name, local: false, online: n.online, pending: n.pending, last_seen: n.online ? now() - 5 : null, version: n.version,
        health: n.online ? { server: n.name, ip: n.ip, gen: n.gen, peers: (usersOf[n.id] ?? []).length, awg_up: true, module: "3.0.1", tools: "1.0.0" } : null })),
];
app.get("/ui/nodes", (_q, r) => r.json({ hub: true, local: true, version: MOCK_VERSION, nodes: nodeView() }));
app.post("/ui/nodes", (q, r) => {
    const name = String(q.body.name ?? "");
    if (!name || name.length > 40) return r.status(400).json({ error: "Имя: до 40 символов, без переводов строк и <>" });
    const id = ++nodeId; mnodes.push({ id, name, ip: "", gen: "2", online: false, pending: true, version: null });
    r.status(201).json({ id, name, join: joinStr(id), expires_in: 3600 });
});
app.post("/ui/nodes/:id/join", (q, r) => { const n = mnodes.find(x => x.id === Number(q.params.id)); n ? r.json({ id: n.id, name: n.name, join: joinStr(n.id), expires_in: 3600 }) : r.status(404).json({ error: "Не найден" }); });
app.delete("/ui/nodes/:id", (q, r) => {
    const id = Number(q.params.id), i = mnodes.findIndex(x => x.id === id); if (i < 0) return r.status(404).json({ error: "Не найден" });
    if (masters.some(m => !m.deleting && m.servers.includes(id))) return r.status(409).json({ error: "Сервер в мастер-ключах — сначала уберите его оттуда", can_force: false });
    const waiting = masters.filter(m => m.deleting?.includes(id));
    if (waiting.length && q.query.force !== "1")
        return r.status(409).json({ error: `Сервер не подтвердил, что снял пиров удалённых мастер-ключей (${waiting.map(m => m.label).join(", ")})`, can_force: true });
    for (const m of waiting) m.deleting = m.deleting!.filter(x => x !== id);
    for (const m of waiting) if (!m.deleting!.length) masters.splice(masters.indexOf(m), 1);
    mnodes.splice(i, 1); r.json({ success: true });
});
app.post("/awg/restart", (_q, r) => r.json({ up: true }));
app.get("/awg/status", (_q, r) => r.json({ up: true, peers: users.length }));

// Монитор: «установка» и «удаление» Prometheus с ходом процесса (как лог CLI на реальном сервере).
const MON_LOGS = {
    install: ["1/4  установка пакета prometheus", "Reading package lists…", "Setting up prometheus (2.45.0)…", "2/4  токен и конфиг", "3/4  слушаем только 127.0.0.1:9090, хранение 31d", "4/4  запуск", "✓ Prometheus работает на 127.0.0.1:9090, метрики панели скрейпятся каждые 15 с."],
    remove:  ["1/3  остановка сервиса", "2/3  токен", "3/3  удаление пакета и накопленных данных", "Removing prometheus (2.45.0)…", "Purging configuration files…", "✓ Prometheus удалён вместе с накопленными графиками."],
};
let monJob: { action: "install" | "remove"; start: number } | null = null;
let monReady = false;
const MON_MS = 6000;
const monRunning = () => !!monJob && Date.now() - monJob.start < MON_MS;
app.get("/ui/monitor", (_q, r) => {
    if (monJob && !monRunning()) monReady = monJob.action === "install";
    let job = null;
    if (monJob) {
        const lines = MON_LOGS[monJob.action];
        const n = monRunning() ? Math.max(1, Math.floor(((Date.now() - monJob.start) / MON_MS) * lines.length)) : lines.length;
        job = { action: monJob.action, running: monRunning(), exit: monRunning() ? null : 0, log: lines.slice(0, n).join("\n") };
    }
    r.json({ ready: monReady, can_install: true, command: "awg-ctrl monitor install", job });
});
for (const action of ["install", "remove"] as const) {
    app.post(`/ui/monitor/${action}`, (_q, r) => { if (!monRunning()) monJob = { action, start: Date.now() }; r.status(202).json({ running: true, action }); });
}
app.get("/ui/monitor/overview", (q, r) => {
    const R: Record<string, [number, number]> = { "1h": [3600, 15], "6h": [21600, 60], "24h": [86400, 300], "7d": [604800, 1800], "30d": [2592000, 7200] };
    const [sec, step] = R[String(q.query.range ?? "1h")] ?? R["1h"];
    const to = Math.floor(Date.now() / step) * step, from = to - sec;
    const pts = (f: (i: number, t: number) => number): [number, number][] => Array.from({ length: Math.floor(sec / step) + 1 }, (_, i) => [from + i * step, Math.max(0, f(i, from + i * step))]);
    const wave = (t: number, per: number, ph = 0) => Math.sin((t / per) * 2 * Math.PI + ph);
    const per = sec / 3;
    const down = pts((_, t) => 110e6 + 70e6 * wave(t, per) + 25e6 * wave(t, per / 7) + rnd(12e6));
    const up   = pts((_, t) => 14e6 + 7e6 * wave(t, per, 1) + rnd(3e6));
    const online = pts((_, t) => Math.round(9 + 3 * wave(t, per * 1.6) + (rnd(100) > 90 ? 1 : 0)));
    const heavy  = pts((_, t) => (wave(t, per) > 0.55 ? 1 : 0));
    const drops  = pts((_, t) => (wave(t, per) > 0.55 ? 3 + rnd(6) : rnd(10) > 8 ? 0.4 : 0));
    const cpu = pts((_, t) => 38 + 22 * wave(t, per * 0.8, 2) + 12 * wave(t, per / 5) + rnd(6));
    const mem = pts((_, t) => 54 + 6 * wave(t, per * 3) + rnd(2));
    const scale = (a: [number, number][], k: number) => a.map(([t, v]) => [t, v * k] as [number, number]);
    // Один сервер — тот, что выбран в сервер-баре (?server=<id>): ядро (0) или нода
    const id = Number(q.query.server ?? 0);
    const k = id === 0 ? 0.7 : 0.3;
    const down1 = scale(down, k), up1 = scale(up, k);
    const used = (a: [number, number][]) => a.reduce((n, [, v]) => n + v, 0) * step / 8;   // бит/с × с → байты
    if (id === 3) return r.json({ range: q.query.range ?? "1h", step, from, to, servers: [], total: { down: [], up: [], online: [], heavy: [], drops: [], cpu: [], mem: [] } });
    r.json({ range: q.query.range ?? "1h", step, from, to,
        servers: [{ id, name: id === 0 ? "VPN (mock)" : "Frankfurt", ok: true, down: down1.at(-1)![1], up: up1.at(-1)![1], online: id === 0 ? 7 : 3,
            heavy: id === 0 ? heavy.at(-1)![1] : 0, congested: id === 0 && heavy.at(-1)![1] > 0,
            cpu: id === 0 ? cpu.at(-1)![1] : cpu.at(-1)![1] * 0.5, mem: id === 0 ? mem.at(-1)![1] : 31, cores: id === 0 ? 4 : 2, mem_total: (id === 0 ? 4 : 2) * 2 ** 30,
            used_down: used(down1), used_up: used(up1) }],
        total: { down: down1, up: up1, online: scale(online, k), heavy: id === 0 ? heavy : scale(heavy, 0), drops: id === 0 ? drops : scale(drops, 0),
            cpu: id === 0 ? cpu : scale(cpu, 0.5), mem: id === 0 ? mem : scale(mem, 0.55) } });
});

// Prometheus: токен метрик и сам /metrics (без проверки токена — мок).
let promTok: { prefix: string; created_at: number; last_used: number | null } | null = null;
const promView = () => promTok ? { exists: true, ...promTok, per_peer: false } : { exists: false, per_peer: false };
app.get("/ui/metrics-token", (_q, r) => r.json(promView()));
app.post("/ui/metrics-token", (_q, r) => {
    const token = "awgm_" + crypto.randomBytes(24).toString("base64url");
    promTok = { prefix: token.slice(0, 13) + "…", created_at: now(), last_used: null };
    r.status(201).json({ ...promView(), token });
});
app.delete("/ui/metrics-token", (_q, r) => { promTok = null; r.json(promView()); });
app.get("/metrics", (_q, r) => r.type("text/plain").send('awg_panel_info{version="mock"} 1\nawg_servers 4\n'));

// Деление канала: состояние по серверам; usersOf[1] играет «старую ноду» без маршрута (404 → «обнови сервер»).
const shapingOf: Record<number, { enabled: boolean; downMbit: number; upMbit: number; auto: boolean }> = {};
const shapingView = (id: number) => {
    const settings = shapingOf[id] ?? { enabled: false, downMbit: 0, upMbit: 0, auto: true };
    const on = settings.enabled;
    return {
        settings, active: on, congested: on && id === 0,
        heavy: on && id === 0 ? ["10.9.0.2", ...(torrentOf[0]?.enabled ? [TORRENT_IP, "10.9.0.12"] : [])] : [],
        devices: on ? 7 : 0, rateDown: on ? Math.round(settings.downMbit * 0.9 * 10) / 10 : 0, rateUp: on ? 12.4 : 0,
    };
};
app.get("/api/shaping", (q, r) => srv(q) === 1 ? r.status(404).json({ error: "Not found" }) : r.json(shapingView(srv(q))));
app.put("/api/shaping", (q, r) => {
    const id = srv(q), b = q.body ?? {};
    if (id === 1) return r.status(404).json({ error: "Not found" });
    if (b.enabled && !(b.downMbit >= 1 && b.upMbit >= 1)) return r.status(400).json({ error: "Для включения укажите ширину канала в обе стороны" });
    shapingOf[id] = { enabled: !!b.enabled, downMbit: Number(b.downMbit) || 0, upMbit: Number(b.upMbit) || 0, auto: !!b.auto };
    r.json(shapingView(id));
});

// Блокировка торрентов: на сервере 0 качает пользователь 10.9.0.3 и второе устройство первого мастер-ключа;
// Amsterdam (3) изображает старый nftables без TCP-проверки; Frankfurt (1) — старая нода без маршрута (404).
// Сервер берём прямо из X-Server-Id: srv() подменяет сервер без своих пользователей на 0.
const torrentOf: Record<number, { enabled: boolean; since: number }> = {};
const tsid = (q: express.Request) => Number(q.headers["x-server-id"] ?? 0) || 0;
const TORRENT_IP = "10.9.0.3";
const torrentSeenIp = (id: number, ip: string) => id === 0 && !!torrentOf[0]?.enabled && ip === TORRENT_IP;
const torrentView = (id: number) => {
    const t = torrentOf[id] ?? { enabled: false, since: 0 };
    const seen = id === 0 && t.enabled
        ? [{ ip: TORRENT_IP, name: users.find(u => u.ip === TORRENT_IP)?.name ?? "" }, { ip: "10.9.0.12", name: "" }]
        : [];
    return { settings: { enabled: t.enabled }, active: t.enabled, tcp: t.enabled && id !== 3,
        hits: t.enabled && id === 0 ? 1840 + Math.floor((now() - t.since) * 3.7) : 0, seen };
};
app.get("/api/torrent", (q, r) => tsid(q) === 1 ? r.status(404).json({ error: "Not found" }) : r.json(torrentView(tsid(q))));
app.put("/api/torrent", (q, r) => {
    const id = tsid(q), b = q.body ?? {};
    if (id === 1) return r.status(404).json({ error: "Not found" });
    if (typeof b.enabled !== "boolean") return r.status(400).json({ error: "enabled должен быть true/false" });
    torrentOf[id] = { enabled: b.enabled, since: b.enabled && !torrentOf[id]?.enabled ? now() : (torrentOf[id]?.since ?? now()) };
    r.json(torrentView(id));
});

app.get("/api/users", (q, r) => r.json({ users: U_(q) }));
// Приоритет в моке: пока на сервере 0 включено деление канала, первый пользователь «фоновый».
// С меткой «торренты» — тоже фоновый (пока включено деление канала).
const prioOf = (q: express.Request, ip: string) =>
    (srv(q) === 0 && shapingOf[0]?.enabled && (ip === "10.9.0.2" || torrentSeenIp(tsid(q), ip)) ? "background" : "normal");
app.get("/api/users/stats", (q, r) => r.json({ users: U_(q).map(({ vpn_key, pub_key, ...u }) => ({ ...u, priority: prioOf(q, u.ip), torrent: torrentSeenIp(tsid(q), u.ip) })) }));
app.post("/api/users/reissue", (q, r) => r.json({ total: U_(q).length, reissued: U_(q).length, regenerated: [], backup: "" }));
app.post("/api/users", (q, r) => {
    const users = U_(q);
    const name = String(q.body.name ?? "");
    if (!/^[a-zA-Z0-9_-]{1,32}$/.test(name)) return r.status(400).json({ error: "Имя: буквы, цифры, _ и -, до 32 символов" });
    if (users.some(u => u.name === name)) return r.status(409).json({ error: "Пользователь уже существует" });
    const u: U = { name, ip: `10.9.0.${users.length + 2}`, pub_key: b64(32), vpn_key: "vpn://" + crypto.randomBytes(48).toString("base64url"), key_gen: GEN, online: false, lastHandshake: 0, rx: 0, tx: 0 };
    users.push(u); r.status(201).json(u);
});
app.post("/api/users/:name", (q, r) => { const u = U_(q).find(x => x.name === q.params.name); u ? r.json(u) : r.status(404).json({ error: "Пользователь не найден" }); });
app.delete("/api/users/:name", (q, r) => { const users = U_(q); const i = users.findIndex(x => x.name === q.params.name); if (i >= 0) users.splice(i, 1); r.json({ success: true }); });

app.get("/ui/apikeys", (_q, r) => r.json({ keys: apiKeys }));
app.post("/ui/apikeys", (q, r) => { const sid = Number(q.body.server_id ?? 0); const k = { id: ++apiId, label: q.body.label, prefix: "awgk_" + crypto.randomBytes(4).toString("hex") + "…", server_id: sid, servers: q.body.servers ?? [sid], created_at: now(), last_used: null }; apiKeys.push(k); r.status(201).json({ ...k, key: "awgk_" + crypto.randomBytes(24).toString("base64url") }); });
app.patch("/ui/apikeys/:id", (q, r) => {
    const k = apiKeys.find(x => x.id === Number(q.params.id)); if (!k) return r.status(404).json({ error: "Не найден" });
    if (!Array.isArray(q.body.servers) || !q.body.servers.length) return r.status(400).json({ error: "Нужен хотя бы один сервер" });
    k.servers = [...q.body.servers].sort(); if (!k.servers.includes(k.server_id)) k.server_id = k.servers[0];
    r.json({ id: k.id, server_id: k.server_id, servers: k.servers });
});
app.delete("/ui/apikeys/:id", (q, r) => { const i = apiKeys.findIndex(k => k.id === Number(q.params.id)); if (i >= 0) apiKeys.splice(i, 1); r.json({ success: true }); });

let mockAuditSettings = { clean: false, every_h: 1, max: 10 };
let mockCleaned: unknown[] = [];
let mockGhost = true;
const mockAudit = () => ({ enabled: true, settings: mockAuditSettings, cleaned: mockCleaned, report: { at: Math.floor(Date.now() / 1000) - 600, servers: [
    { server_id: 0, status: "ok", ghosts_checked: true, foreign: 0, orphans: [], ghosts: mockGhost ? [
        { pub_key: "Zm9yZ2V0dGluZy16b21iaWUta2V5LTAwMDAwMDAwMDA=", online: true, lastHandshake: Math.floor(Date.now() / 1000) - 30, seen_before: true } ] : [] } ] } });
app.get("/ui/masterkeys/audit", (_q, r) => r.json(mockAudit()));
app.post("/ui/masterkeys/audit", (_q, r) => r.json(mockAudit()));
app.post("/ui/masterkeys/audit/clean", (_q, r) => {
    if (mockGhost) mockCleaned = [{ at: Math.floor(Date.now() / 1000), server_id: 0, pub_key: "Zm9yZ2V0dGluZy16b21iaWUta2V5LTAwMDAwMDAwMDA=", kind: "ghost", auto: false }, ...mockCleaned];
    mockGhost = false; r.json(mockAudit());
});
app.put("/ui/masterkeys/audit/settings", (q, r) => { mockAuditSettings = { ...mockAuditSettings, ...q.body }; r.json(mockAudit()); });
app.get("/ui/masterkeys", (_q, r) => r.json({ enabled: true, tls: false, keys: masters.map(mkOut) }));
app.post("/ui/masterkeys", (q, r) => {
    const limit = q.body.device_limit ?? 3;
    if (!q.body.label || !Number.isInteger(limit) || limit < 1 || limit > 100) return r.status(400).json({ error: "Неверные параметры" });
    const m: MK = { id: mkId++, uuid: crypto.randomUUID(), label: q.body.label, device_limit: limit, created_at: now(), secret: crypto.randomBytes(16), devs: [], servers: q.body.servers ?? [0] };
    masters.push(m); r.status(201).json(mkOut(m));
});
app.patch("/ui/masterkeys/:id", (q, r) => {
    const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" });
    if (q.body.servers !== undefined) { if (!Array.isArray(q.body.servers) || !q.body.servers.length) return r.status(400).json({ error: "Нужен хотя бы один сервер" }); m.servers = [...q.body.servers].sort(); }
    if (q.body.label) m.label = q.body.label; if (q.body.device_limit) m.device_limit = q.body.device_limit;
    // Как sub.ts: лишние устройства сверх лимита — отвязать, начиная с подключившихся последними.
    const unbound = [...m.devs].sort((a, b) => b.created_at - a.created_at || b.id - a.id).slice(0, Math.max(0, m.devs.length - m.device_limit)).map(d => d.id);
    m.devs = m.devs.filter(d => !unbound.includes(d.id));
    r.json({ ...mkOut(m), unbound });
});
app.post("/ui/masterkeys/:id/rotate", (q, r) => { const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" }); m.secret = crypto.randomBytes(16); r.json({ id: m.id }); });
app.post("/ui/masterkeys/:id/rekey", (q, r) => { const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" }); m.devs.forEach(d => d.rekey_requested = true); r.json({ id: m.id }); });
app.delete("/ui/masterkeys/:id", (q, r) => {
    const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" });
    const pending = m.servers.filter(id => id !== 0 && !mnodes.find(n => n.id === id)?.online);
    if (pending.length) { m.deleting = pending; m.devs = []; } else masters.splice(masters.indexOf(m), 1);
    r.json({ success: true, id: m.id, pending });
});
app.get("/ui/masterkeys/:id/link", (q, r) => {
    const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" });
    r.json({ link: encodeSenLink({ tls: false, addrs: [{ host: "203.0.113.7", port: 41234 }], secret: m.secret, signPub, name: m.label }), tls: false });
});
app.get("/ui/masterkeys/:id/devices", (q, r) => { const m = find(q.params.id); m ? r.json({ devices: m.devs.map(d => devOut(m, d)) }) : r.status(404).json({ error: "Не найден" }); });
app.delete("/ui/devices/:id", (q, r) => { for (const m of masters) m.devs = m.devs.filter(d => d.id !== Number(q.params.id)); r.json({ success: true }); });
app.post("/ui/devices/:id/rekey", (q, r) => { masters.forEach(m => m.devs.forEach(d => { if (d.id === Number(q.params.id)) d.rekey_requested = true; })); r.json({}); });
app.post("/ui/devices/:id/psk", (_q, r) => r.json({ pending: 0 }));

const MOCK_PORT = Number(process.env.MOCK_PORT) || 8080;
app.listen(MOCK_PORT, () => console.log(`mock: http://localhost:${MOCK_PORT} (login admin / admin)`));
