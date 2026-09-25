// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// sen://-подписка: реестр мастер-ключей/устройств (ui.db), админ-роуты панели
// (/ui/masterkeys, /ui/devices, за JWT) и публичный листенер /sub/v1/* на
// отдельном порту SUB_PORT. Протокол и формат ссылки — docs/sen-link.md.
//
// awg-ctrl про мастер-ключи ничего не знает: сюда он отдаёт только пиров по
// публичному ключу (/api/peers*) и общий профиль сервера (/api/profile).
import crypto from "crypto";
import fs from "fs";
import http from "http";
import https from "https";
import path from "path";
import express, { Request, Response, NextFunction, Router } from "express";
import type Database from "better-sqlite3";
import {
    encodeSenLink, rawPublicKey, publicKeyFromRaw, spkiPin,
    signResponse, verifyRequest,
} from "./sen";

type Ctrl = (method: string, urlPath: string, body?: unknown) => Promise<{ status: number; data: any }>;

interface MasterRow {
    id: number; label: string; secret: Buffer; device_limit: number;
    server_id: number; created_at: number; revoked_at: number | null;
}
interface DeviceRow {
    id: number; master_id: number; device_id: string; device_name: string; platform: string;
    pub_key: string; auth_pub: string; created_at: number; last_seen: number | null;
    rekey_requested: number; version: string;
}

const TS_WINDOW    = 300;           // секунд, ±
const DEFAULT_LIMIT = 3;
const B64_PUB       = /^[A-Za-z0-9+/]{43}=$/;
const CLIENT_VERSION = /^[\w.+-]{1,32}$/;   // версия приложения, чисто информационная

// pub_key WireGuard в пути к awg-ctrl — base64url (в обычном base64 есть «/»).
const pubParam = (pub: string) => Buffer.from(pub, "base64").toString("base64url");

// ── Простой лимитер по IP ──────────────────────────────────────────────────
function makeLimiter(max: number, windowMs: number) {
    const hits = new Map<string, { n: number; reset: number }>();
    return (ip: string): boolean => {
        const now = Date.now();
        const e = hits.get(ip);
        if (!e || e.reset < now) { hits.set(ip, { n: 1, reset: now + windowMs }); return true; }
        if (e.n >= max) return false;
        e.n++;
        return true;
    };
}

export function createSub(deps: { uidb: Database.Database; ctrl: Ctrl; baseDir: string }) {
    const { uidb, ctrl } = deps;

    // ── Настройка из окружения ─────────────────────────────────────────────
    const SUB_PORT = Number(process.env.SUB_PORT) || 0;
    const SUB_TLS  = (process.env.SUB_TLS ?? "off").toLowerCase() === "on";
    const file = (env: string | undefined, name: string) => env || path.join(deps.baseDir, name);

    let signKey: crypto.KeyObject | null = null;
    let signPub: Buffer | null = null;
    try {
        signKey = crypto.createPrivateKey(fs.readFileSync(file(process.env.SUB_SIGN_KEY_FILE, "sub_sign.key")));
        signPub = rawPublicKey(crypto.createPublicKey(signKey));
    } catch {
        console.warn("sub: ключ подписи не найден — sen://-подписка отключена");
    }

    let tlsMaterial: { key: Buffer; cert: Buffer } | null = null;
    let tlsPin: Buffer | null = null;
    if (SUB_TLS) {
        try {
            tlsMaterial = {
                key:  fs.readFileSync(file(process.env.SUB_TLS_KEY_FILE,  "sub_tls.key")),
                cert: fs.readFileSync(file(process.env.SUB_TLS_CERT_FILE, "sub_tls.crt")),
            };
            tlsPin = spkiPin(tlsMaterial.cert.toString());
        } catch {
            console.warn("sub: SUB_TLS=on, но сертификат не найден — подписка отключена");
            signKey = null;
        }
    }

    // ── Схема ──────────────────────────────────────────────────────────────
    uidb.exec(`
        CREATE TABLE IF NOT EXISTS master_keys (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            label        TEXT    NOT NULL,
            secret       BLOB    NOT NULL UNIQUE,
            device_limit INTEGER NOT NULL DEFAULT ${DEFAULT_LIMIT},
            server_id    INTEGER NOT NULL DEFAULT 0,
            created_at   INTEGER NOT NULL,
            revoked_at   INTEGER
        );
        CREATE TABLE IF NOT EXISTS devices (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            master_id       INTEGER NOT NULL REFERENCES master_keys(id) ON DELETE CASCADE,
            device_id       TEXT    NOT NULL,
            device_name     TEXT    NOT NULL DEFAULT '',
            platform        TEXT    NOT NULL DEFAULT '',
            pub_key         TEXT    NOT NULL UNIQUE,
            auth_pub        TEXT    NOT NULL UNIQUE,
            created_at      INTEGER NOT NULL,
            last_seen       INTEGER,
            rekey_requested INTEGER NOT NULL DEFAULT 0,
            UNIQUE (master_id, device_id)
        );
    `);
    // Версия клиента — необязательное поле, добавленное позже: у уже созданной
    // таблицы колонки может не быть.
    if (!(uidb.prepare("PRAGMA table_info(devices)").all() as { name: string }[]).some(c => c.name === "version"))
        uidb.exec("ALTER TABLE devices ADD COLUMN version TEXT NOT NULL DEFAULT ''");
    uidb.pragma("foreign_keys = ON");

    const q = {
        masters:      uidb.prepare("SELECT m.*, (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m WHERE revoked_at IS NULL ORDER BY id"),
        masterById:   uidb.prepare<[number]>("SELECT * FROM master_keys WHERE id = ? AND revoked_at IS NULL"),
        masterBySecret: uidb.prepare<[Buffer]>("SELECT * FROM master_keys WHERE secret = ? AND revoked_at IS NULL"),
        masterInsert: uidb.prepare<[string, Buffer, number, number]>("INSERT INTO master_keys (label, secret, device_limit, created_at) VALUES (?, ?, ?, ?)"),
        masterUpdate: uidb.prepare<[string, number, number]>("UPDATE master_keys SET label = ?, device_limit = ? WHERE id = ?"),
        masterSecret: uidb.prepare<[Buffer, number]>("UPDATE master_keys SET secret = ? WHERE id = ?"),
        masterDelete: uidb.prepare<[number]>("DELETE FROM master_keys WHERE id = ?"),
        devicesOf:    uidb.prepare<[number]>("SELECT * FROM devices WHERE master_id = ? ORDER BY id"),
        deviceById:   uidb.prepare<[number]>("SELECT * FROM devices WHERE id = ?"),
        deviceByKey:  uidb.prepare<[number, string]>("SELECT * FROM devices WHERE master_id = ? AND device_id = ?"),
        deviceCount:  uidb.prepare<[number]>("SELECT COUNT(*) AS n FROM devices WHERE master_id = ?"),
        deviceInsert: uidb.prepare<[number, string, string, string, string, string, number, string]>(
            "INSERT INTO devices (master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"),
        deviceVersion: uidb.prepare<[string, number]>("UPDATE devices SET version = ? WHERE id = ?"),
        deviceDelete: uidb.prepare<[number]>("DELETE FROM devices WHERE id = ?"),
        deviceSeen:   uidb.prepare<[number, number]>("UPDATE devices SET last_seen = ? WHERE id = ?"),
        deviceRekeyed: uidb.prepare<[string, number]>("UPDATE devices SET pub_key = ?, rekey_requested = 0 WHERE id = ?"),
        deviceFlag:   uidb.prepare<[number, number]>("UPDATE devices SET rekey_requested = 1 WHERE id = ? OR master_id = ?"),
    };

    const now = () => Math.floor(Date.now() / 1000);
    const hasCtrlOk = (s: number) => s >= 200 && s < 300;

    // ── Общие куски ────────────────────────────────────────────────────────
    async function profile() {
        const r = await ctrl("GET", "/api/profile");
        if (!hasCtrlOk(r.status)) throw new Error("profile");
        return r.data as {
            name: string; endpoint: string; server_pub: string; gen: string;
            dns: string[]; keepalive: string; mtu: number; awg: Record<string, string>;
        };
    }

    const hostOf = (endpoint: string) => endpoint.slice(0, endpoint.lastIndexOf(":"));

    async function buildConfig(dev: DeviceRow) {
        const [prof, peers] = await Promise.all([profile(), ctrl("GET", "/api/peers")]);
        if (!hasCtrlOk(peers.status)) throw new Error("peers");
        const peer = (peers.data.peers as { pub_key: string; ip: string; psk_key: string }[])
            .find(p => p.pub_key === dev.pub_key);
        if (!peer) throw new Error("peer missing");
        const cfg = {
            rekey: dev.rekey_requested === 1,
            endpoints: [`${hostOf(prof.endpoint)}:${SUB_PORT}`],
            servers: [{
                id: 0, name: prof.name, endpoint: prof.endpoint, server_pub: prof.server_pub,
                psk: peer.psk_key, address: `${peer.ip}/32`, dns: prof.dns,
                keepalive: prof.keepalive, mtu: prof.mtu, gen: prof.gen, awg: prof.awg,
            }],
        };
        // rev меняется при любом изменении; клиент по нему решает, трогать ли туннель.
        const rev = crypto.createHash("sha256").update(JSON.stringify(cfg)).digest("hex").slice(0, 16);
        return { rev, ...cfg };
    }

    async function dropPeer(pub: string) {
        await ctrl("DELETE", `/api/peers/${pubParam(pub)}`); // 404 — уже нет, не страшно
    }

    // ── Публичная часть: /sub/v1 ───────────────────────────────────────────
    const sub = express();
    sub.disable("x-powered-by");
    sub.use(express.json({
        limit: "4kb",
        verify: (req, _res, buf) => { (req as any).rawBody = buf; },
    }));

    const registerLimit = makeLimiter(30, 15 * 60 * 1000);
    const failLimit     = makeLimiter(30, 15 * 60 * 1000);
    const seenSigs      = new Map<string, number>(); // подпись → когда забыть

    function send(res: Response, status: number, payload: unknown) {
        res.status(status).json(signResponse(payload, signKey as crypto.KeyObject));
    }

    // Проверка подписи запроса + защита от повторов. auth_pub — 32 байта.
    function checkSigned(req: Request, authPub: Buffer): boolean {
        const ts  = Number(req.headers["x-sen-ts"]);
        const sig = String(req.headers["x-sen-sig"] ?? "");
        if (!Number.isInteger(ts) || Math.abs(ts - now()) > TS_WINDOW || !sig) return false;
        const body = ((req as any).rawBody as Buffer | undefined) ?? Buffer.alloc(0);
        if (!verifyRequest(req.method, req.path, ts, body, sig, authPub)) return false;
        const t = Date.now();
        for (const [k, exp] of seenSigs) if (exp < t) seenSigs.delete(k);
        if (seenSigs.has(sig)) return false;
        seenSigs.set(sig, t + 2 * TS_WINDOW * 1000);
        return true;
    }

    interface DevReq extends Request { device?: DeviceRow }

    function deviceAuth(req: Request, res: Response, next: NextFunction) {
        const id = Number(req.headers["x-sen-device"]);
        const dev = Number.isInteger(id) ? q.deviceById.get(id) as DeviceRow | undefined : undefined;
        if (dev && checkSigned(req, Buffer.from(dev.auth_pub, "base64url"))) {
            (req as DevReq).device = dev;
            next();
            return;
        }
        // Лимитируем только неудачи: живой клиент, опрашивающий config, под лимит не попадает.
        if (!failLimit(req.socket.remoteAddress ?? "unknown")) { send(res, 429, { error: "rate_limited" }); return; }
        send(res, 401, { error: "unauthorized" });
    }

    const wrap = (fn: (req: Request, res: Response) => Promise<void>) =>
        (req: Request, res: Response) => { fn(req, res).catch(() => { send(res, 502, { error: "unavailable" }); }); };

    sub.post("/sub/v1/register", wrap(async (req, res) => {
        const ip = req.socket.remoteAddress ?? "unknown";
        if (!registerLimit(ip)) { send(res, 429, { error: "rate_limited" }); return; }

        const b = (req.body ?? {}) as Record<string, unknown>;
        const str = (v: unknown, re: RegExp) => typeof v === "string" && re.test(v) ? v : null;
        const secretB = str(b.sub, /^[A-Za-z0-9_-]{22}$/);
        const deviceId = str(b.device_id, /^[\w.-]{1,64}$/);
        const pubKey = str(b.pub_key, B64_PUB);
        const authPubS = str(b.auth_pub, /^[A-Za-z0-9_-]{43}$/);
        const name = typeof b.device_name === "string" ? b.device_name.slice(0, 64) : "";
        const platform = typeof b.platform === "string" ? b.platform.slice(0, 32) : "";
        const version = str(b.version, CLIENT_VERSION) ?? "";
        if (!secretB || !deviceId || !pubKey || !authPubS) { send(res, 400, { error: "bad_request" }); return; }

        // Регистрация подписана ключом auth_pub из самого тела — доказательство владения.
        const authPub = Buffer.from(authPubS, "base64url");
        if (authPub.length !== 32 || !checkSigned(req, authPub)) { send(res, 401, { error: "unauthorized" }); return; }

        const master = q.masterBySecret.get(Buffer.from(secretB, "base64url")) as MasterRow | undefined;
        if (!master) { send(res, 404, { error: "not_found" }); return; }

        let deviceRowId = 0;
        let oldPub: string | null = null;
        try {
            uidb.transaction(() => {
                const existing = q.deviceByKey.get(master.id, deviceId) as DeviceRow | undefined;
                if (existing) { oldPub = existing.pub_key; q.deviceDelete.run(existing.id); }
                else if ((q.deviceCount.get(master.id) as { n: number }).n >= master.device_limit)
                    throw Object.assign(new Error("limit"), { code: "limit" });
                deviceRowId = Number(q.deviceInsert.run(master.id, deviceId, name, platform, pubKey, authPubS, now(), version).lastInsertRowid);
            })();
        } catch (e: any) {
            if (e?.code === "limit") { send(res, 403, { error: "device_limit" }); return; }
            send(res, 409, { error: "conflict" }); return; // UNIQUE: pub_key/auth_pub уже заняты
        }

        if (oldPub) await dropPeer(oldPub);
        const r = await ctrl("POST", "/api/peers", { pub_key: pubKey, owner: `m${master.id}/d${deviceRowId}` });
        if (!hasCtrlOk(r.status)) {
            q.deviceDelete.run(deviceRowId);
            send(res, r.status === 409 ? 409 : 502, { error: r.status === 409 ? "conflict" : "unavailable" });
            return;
        }
        const dev = q.deviceById.get(deviceRowId) as DeviceRow;
        send(res, 201, { device: dev.id, config: await buildConfig(dev) });
    }));

    sub.get("/sub/v1/config", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        q.deviceSeen.run(now(), dev.id);
        // Версия клиента меняется при обновлении приложения; заголовок не подписан — это лишь подпись в панели.
        const v = String(req.headers["x-sen-version"] ?? "");
        if (CLIENT_VERSION.test(v) && v !== dev.version) q.deviceVersion.run(v, dev.id);
        send(res, 200, { config: await buildConfig(dev) });
    }));

    sub.post("/sub/v1/rekey", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        const pub = (req.body as { pub_key?: unknown } | undefined)?.pub_key;
        if (typeof pub !== "string" || !B64_PUB.test(pub)) { send(res, 400, { error: "bad_request" }); return; }
        const r = await ctrl("PUT", `/api/peers/${pubParam(dev.pub_key)}`, { pub_key: pub });
        if (!hasCtrlOk(r.status)) { send(res, r.status === 409 ? 409 : 502, { error: r.status === 409 ? "conflict" : "unavailable" }); return; }
        q.deviceRekeyed.run(pub, dev.id);
        send(res, 200, { config: await buildConfig(q.deviceById.get(dev.id) as DeviceRow) });
    }));

    sub.delete("/sub/v1/device", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        await dropPeer(dev.pub_key);
        q.deviceDelete.run(dev.id);
        send(res, 200, { ok: true });
    }));

    sub.use((_req, res) => { send(res, 404, { error: "not_found" }); });
    // Битый JSON и т. п.: ответ всё равно подписан, чтобы клиент мог ему доверять.
    sub.use((_err: unknown, _req: Request, res: Response, _next: NextFunction) => { send(res, 400, { error: "bad_request" }); });

    // ── Админ-роуты панели (монтируются в server.ts за JWT) ─────────────────
    const aw = (fn: (req: Request, res: Response) => Promise<void>) =>
        (req: Request, res: Response) => { fn(req, res).catch(() => { res.status(502).json({ error: "awg-ctrl недоступен" }); }); };
    const idOf = (req: Request) => Number(req.params.id);
    const LABEL = /^[^\r\n<>]{1,40}$/;
    const limitOk = (n: unknown): n is number => Number.isInteger(n) && (n as number) >= 1 && (n as number) <= 100;

    const masterKeys = Router();

    masterKeys.get("/", (_req, res) => {
        res.json({
            enabled: signKey !== null,
            tls: tlsPin !== null,
            keys: (q.masters.all() as (MasterRow & { devices: number })[]).map(m => ({
                id: m.id, label: m.label, device_limit: m.device_limit,
                devices: m.devices, server_id: m.server_id, created_at: m.created_at,
            })),
        });
    });

    masterKeys.post("/", (req, res) => {
        const { label, device_limit } = (req.body ?? {}) as { label?: string; device_limit?: number };
        if (typeof label !== "string" || !LABEL.test(label)) { res.status(400).json({ error: "Метка: до 40 символов, без переводов строк и <>" }); return; }
        const limit = device_limit === undefined ? DEFAULT_LIMIT : device_limit;
        if (!limitOk(limit)) { res.status(400).json({ error: "Лимит устройств: от 1 до 100" }); return; }
        const info = q.masterInsert.run(label, crypto.randomBytes(16), limit, now());
        res.status(201).json({ id: Number(info.lastInsertRowid), label, device_limit: limit, devices: 0 });
    });

    masterKeys.patch("/:id", (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        const { label, device_limit } = (req.body ?? {}) as { label?: string; device_limit?: number };
        const nl = label ?? m.label, nd = device_limit ?? m.device_limit;
        if (typeof nl !== "string" || !LABEL.test(nl)) { res.status(400).json({ error: "Неверная метка" }); return; }
        if (!limitOk(nd)) { res.status(400).json({ error: "Лимит устройств: от 1 до 100" }); return; }
        q.masterUpdate.run(nl, nd, m.id);
        res.json({ id: m.id, label: nl, device_limit: nd });
    });

    masterKeys.post("/:id/rotate", (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        q.masterSecret.run(crypto.randomBytes(16), m.id); // устройства продолжают работать: у них auth_pub
        res.json({ id: m.id });
    });

    masterKeys.post("/:id/rekey", (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        q.deviceFlag.run(-1, m.id);
        res.json({ id: m.id });
    });

    masterKeys.delete("/:id", aw(async (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        for (const d of q.devicesOf.all(m.id) as DeviceRow[]) await dropPeer(d.pub_key);
        q.masterDelete.run(m.id); // devices уйдут каскадом
        res.json({ success: true, id: m.id });
    }));

    masterKeys.get("/:id/link", aw(async (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        if (!signKey || !signPub || !SUB_PORT) { res.status(503).json({ error: "Подписка не настроена на сервере" }); return; }
        const prof = await profile();
        const link = encodeSenLink({
            tls: tlsPin !== null,
            addrs: [{ host: hostOf(prof.endpoint), port: SUB_PORT }],
            secret: m.secret, signPub, tlsPin: tlsPin ?? undefined, name: m.label,
        });
        res.json({ link, tls: tlsPin !== null });
    }));

    masterKeys.get("/:id/devices", aw(async (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        const stats = new Map<string, { online: boolean; lastHandshake: number; rx: number; tx: number }>();
        const r = await ctrl("GET", "/api/peers");
        if (hasCtrlOk(r.status)) for (const p of r.data.peers) stats.set(p.pub_key, p);
        res.json({
            devices: (q.devicesOf.all(m.id) as DeviceRow[]).map(d => ({
                id: d.id, device_id: d.device_id, device_name: d.device_name, platform: d.platform, version: d.version, created_at: d.created_at,
                last_seen: d.last_seen, rekey_requested: d.rekey_requested === 1,
                ...pick(stats.get(d.pub_key)),
            })),
        });
    }));

    function pick(p?: { online: boolean; lastHandshake: number; rx: number; tx: number }) {
        return { online: p?.online ?? false, lastHandshake: p?.lastHandshake ?? 0, rx: p?.rx ?? 0, tx: p?.tx ?? 0 };
    }

    const devices = Router();

    devices.delete("/:id", aw(async (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        await dropPeer(d.pub_key);
        q.deviceDelete.run(d.id);
        res.json({ success: true, id: d.id });
    }));

    devices.post("/:id/rekey", (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        q.deviceFlag.run(d.id, -1);
        res.json({ id: d.id });
    });

    devices.post("/:id/psk", aw(async (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        const r = await ctrl("POST", `/api/peers/${pubParam(d.pub_key)}/psk`);
        res.status(hasCtrlOk(r.status) ? 200 : 502).json(hasCtrlOk(r.status) ? { id: d.id } : { error: "awg-ctrl отказал" });
    }));

    // ── Листенер ───────────────────────────────────────────────────────────
    function start() {
        if (!signKey) return;
        if (!SUB_PORT) { console.warn("sub: SUB_PORT не задан — подписка отключена"); signKey = null; return; }
        const server = tlsMaterial ? https.createServer(tlsMaterial, sub) : http.createServer(sub);
        server.listen(SUB_PORT, () => {
            console.log(`sub: listening on :${SUB_PORT} (${tlsMaterial ? "https" : "http"})`);
        });
    }

    return { masterKeys, devices, start };
}
