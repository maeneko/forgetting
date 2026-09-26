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
interface MNode { id: number; name: string; ip: string; gen: string; online: boolean; pending: boolean }
const mnodes: MNode[] = [
    { id: 1, name: "Frankfurt", ip: "198.51.100.4", gen: "2", online: true, pending: false },
    { id: 2, name: "Tokyo", ip: "", gen: "2", online: false, pending: true },
];
let nodeId = 2;
const joinStr = (id: number) => "awgjoin://" + Buffer.from(JSON.stringify({ v: 1, h: "localhost", p: 8443, pin: crypto.randomBytes(32).toString("base64url"), n: id, s: crypto.randomBytes(16).toString("base64url") })).toString("base64url");

// ── API-ключи ──
const apiKeys = [{ id: 1, label: "ci-bot", prefix: "awgk_Xk2mQ9aB…", server_id: 0, created_at: now() - 86400 * 5, last_used: now() - 3600 as number | null }];
let apiId = 1;

// ── мастер-ключи sen:// ──
interface Dev { id: number; device_id: string; device_name: string; platform: string; version: string; created_at: number; last_seen: number | null; rekey_requested: boolean; online: boolean; lastHandshake: number; rx: number; tx: number }
interface MK { id: number; label: string; device_limit: number; created_at: number; secret: Buffer; devs: Dev[] }
let mkId = 1, devId = 1;
const dev = (name: string, platform: string, online: boolean, version = "0.6.5"): Dev => ({ id: devId++, device_id: crypto.randomUUID(), device_name: name, platform, version, created_at: now() - 86400, last_seen: now() - rnd(900), rekey_requested: false, online, lastHandshake: now() - rnd(300), rx: rnd(9e8), tx: rnd(3e8) });
const masters: MK[] = [{ id: mkId++, label: "Семья", device_limit: 3, created_at: now() - 86400 * 2, secret: crypto.randomBytes(16), devs: [dev("MacBook Ивана", "macOS", true), dev("Windows-ПК", "Windows", false, "0.6.2")] }];
const signPub = crypto.randomBytes(32);
const mkOut = (m: MK) => ({ id: m.id, label: m.label, device_limit: m.device_limit, devices: m.devs.length, server_id: 0, created_at: m.created_at });
const find = (id: string) => masters.find(m => m.id === Number(id));

app.get("/ui/brand", (_q, r) => r.json({ brand: "Forgetting", channel: "Beta", version: "0.2.2-mock" }));
app.post("/login", (q, r) => q.body.user === "admin" && q.body.pass === "admin" ? r.json({ token: "mock." + crypto.randomBytes(8).toString("hex") + ".sig" }) : r.status(401).json({ error: "Неверный логин или пароль" }));
app.post("/logout", (_q, r) => r.json({ ok: true }));
app.get("/health", (q, r) => {
    const id = srv(q), n = mnodes.find(x => x.id === id);
    r.json({ status: "ok", server: n?.name ?? "VPN (mock)", ip: n?.ip ?? "203.0.113.7", gen: n?.gen ?? GEN, awg: { status: "ok", peers: usersOf[id].length, module: "3.0.1", tools: "1.0.0" } });
});

// ── серверы ──
const nodeView = () => [
    { id: 0, name: "VPN (mock)", local: true, online: true, pending: false, last_seen: null, health: { server: "VPN (mock)", ip: "203.0.113.7", gen: GEN, peers: users.length, awg_up: true, module: "3.0.1", tools: "1.0.0" } },
    ...mnodes.map(n => ({ id: n.id, name: n.name, local: false, online: n.online, pending: n.pending, last_seen: n.online ? now() - 5 : null,
        health: n.online ? { server: n.name, ip: n.ip, gen: n.gen, peers: (usersOf[n.id] ?? []).length, awg_up: true, module: "3.0.1", tools: "1.0.0" } : null })),
];
app.get("/ui/nodes", (_q, r) => r.json({ hub: true, local: true, nodes: nodeView() }));
app.post("/ui/nodes", (q, r) => {
    const name = String(q.body.name ?? "");
    if (!name || name.length > 40) return r.status(400).json({ error: "Имя: до 40 символов, без переводов строк и <>" });
    const id = ++nodeId; mnodes.push({ id, name, ip: "", gen: "2", online: false, pending: true });
    r.status(201).json({ id, name, join: joinStr(id), expires_in: 3600 });
});
app.post("/ui/nodes/:id/join", (q, r) => { const n = mnodes.find(x => x.id === Number(q.params.id)); n ? r.json({ id: n.id, name: n.name, join: joinStr(n.id), expires_in: 3600 }) : r.status(404).json({ error: "Не найден" }); });
app.delete("/ui/nodes/:id", (q, r) => { const i = mnodes.findIndex(x => x.id === Number(q.params.id)); if (i < 0) return r.status(404).json({ error: "Не найден" }); mnodes.splice(i, 1); r.json({ success: true }); });
app.post("/awg/restart", (_q, r) => r.json({ up: true }));
app.get("/awg/status", (_q, r) => r.json({ up: true, peers: users.length }));

app.get("/api/users", (q, r) => r.json({ users: U_(q) }));
app.get("/api/users/stats", (q, r) => r.json({ users: U_(q).map(({ vpn_key, pub_key, ...u }) => u) }));
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
app.post("/ui/apikeys", (q, r) => { const k = { id: ++apiId, label: q.body.label, prefix: "awgk_" + crypto.randomBytes(4).toString("hex") + "…", server_id: 0, created_at: now(), last_used: null }; apiKeys.push(k); r.status(201).json({ ...k, key: "awgk_" + crypto.randomBytes(24).toString("base64url") }); });
app.delete("/ui/apikeys/:id", (q, r) => { const i = apiKeys.findIndex(k => k.id === Number(q.params.id)); if (i >= 0) apiKeys.splice(i, 1); r.json({ success: true }); });

app.get("/ui/masterkeys", (_q, r) => r.json({ enabled: true, tls: false, keys: masters.map(mkOut) }));
app.post("/ui/masterkeys", (q, r) => {
    const limit = q.body.device_limit ?? 3;
    if (!q.body.label || !Number.isInteger(limit) || limit < 1 || limit > 100) return r.status(400).json({ error: "Неверные параметры" });
    const m: MK = { id: mkId++, label: q.body.label, device_limit: limit, created_at: now(), secret: crypto.randomBytes(16), devs: [] };
    masters.push(m); r.status(201).json(mkOut(m));
});
app.patch("/ui/masterkeys/:id", (q, r) => { const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" }); if (q.body.label) m.label = q.body.label; if (q.body.device_limit) m.device_limit = q.body.device_limit; r.json(mkOut(m)); });
app.post("/ui/masterkeys/:id/rotate", (q, r) => { const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" }); m.secret = crypto.randomBytes(16); r.json({ id: m.id }); });
app.post("/ui/masterkeys/:id/rekey", (q, r) => { const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" }); m.devs.forEach(d => d.rekey_requested = true); r.json({ id: m.id }); });
app.delete("/ui/masterkeys/:id", (q, r) => { const i = masters.findIndex(m => m.id === Number(q.params.id)); if (i >= 0) masters.splice(i, 1); r.json({ success: true }); });
app.get("/ui/masterkeys/:id/link", (q, r) => {
    const m = find(q.params.id); if (!m) return r.status(404).json({ error: "Не найден" });
    r.json({ link: encodeSenLink({ tls: false, addrs: [{ host: "203.0.113.7", port: 41234 }], secret: m.secret, signPub, name: m.label }), tls: false });
});
app.get("/ui/masterkeys/:id/devices", (q, r) => { const m = find(q.params.id); m ? r.json({ devices: m.devs }) : r.status(404).json({ error: "Не найден" }); });
app.delete("/ui/devices/:id", (q, r) => { for (const m of masters) m.devs = m.devs.filter(d => d.id !== Number(q.params.id)); r.json({ success: true }); });
app.post("/ui/devices/:id/rekey", (q, r) => { masters.forEach(m => m.devs.forEach(d => { if (d.id === Number(q.params.id)) d.rekey_requested = true; })); r.json({}); });
app.post("/ui/devices/:id/psk", (_q, r) => r.json({}));

const MOCK_PORT = Number(process.env.MOCK_PORT) || 8080;
app.listen(MOCK_PORT, () => console.log(`mock: http://localhost:${MOCK_PORT} (login admin / admin)`));
