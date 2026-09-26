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
//
// Несколько серверов. Мастер-ключ отдаёт набор серверов (master_servers): локальный
// awg-ctrl (id 0) и/или ноды. Устройство регистрирует один ключ WireGuard, и он
// становится пиром на каждом сервере набора — IP и PSK на каждом свои (device_peers).
// Нода может быть не в сети, поэтому изменения не «толкаются», а сводятся: device_peers
// хранит, какой ключ сейчас стоит на сервере, peer_tombstones — что там надо удалить,
// а reconcile() приводит сервер к нужному состоянию, как только до него можно достучаться
// (сразу при действии, при подключении ноды и раз в минуту).
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

/** Что подписке нужно от реестра серверов (awg-ui/nodes.ts). */
export interface Servers {
    ctrlFor(id: number): Ctrl;
    exists(id: number): boolean;
    isOnline(id: number): boolean;
    ids(): number[];
    defaultId(): number;
    onOnline(cb: (id: number) => void): void;
}

interface MasterRow {
    id: number; label: string; secret: Buffer; device_limit: number;
    server_id: number; created_at: number; revoked_at: number | null;
}
interface DeviceRow {
    id: number; master_id: number; device_id: string; device_name: string; platform: string;
    pub_key: string; auth_pub: string; created_at: number; last_seen: number | null;
    rekey_requested: number; version: string;
}
interface LinkRow {
    device_id: number; server_id: number; peer_pub: string | null;
    ip: string | null; psk: string | null; psk_pending: number;
}
interface Profile {
    name: string; endpoint: string; server_pub: string; gen: string;
    dns: string[]; keepalive: string; mtu: number; awg: Record<string, string>;
}

const TS_WINDOW    = 300;           // секунд, ±
const DEFAULT_LIMIT = 3;
const MAX_SERVERS   = 32;           // серверов в одном мастер-ключе
const SYNC_EVERY    = 60_000;       // фоновое сведение серверов
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

export function createSub(deps: { uidb: Database.Database; servers: Servers; baseDir: string }) {
    const { uidb, servers } = deps;

    // ── Настройка из окружения ─────────────────────────────────────────────
    const SUB_PORT = Number(process.env.SUB_PORT) || 0;
    const SUB_TLS  = (process.env.SUB_TLS ?? "off").toLowerCase() === "on";
    // Публичный адрес панели для ссылок и endpoints. Без него — адрес локального сервера,
    // а у панели без своего VPN — тот, по которому к ней пришли.
    const SUB_HOST = (process.env.SUB_HOST ?? "").trim();
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
        -- Серверы мастер-ключа. master_keys.server_id остался от времени одного сервера и
        -- дальше не читается (кроме переноса ниже).
        CREATE TABLE IF NOT EXISTS master_servers (
            master_id INTEGER NOT NULL REFERENCES master_keys(id) ON DELETE CASCADE,
            server_id INTEGER NOT NULL,
            PRIMARY KEY (master_id, server_id)
        );
        -- Пир устройства на сервере. peer_pub — ключ, который сейчас стоит на сервере
        -- (NULL — ещё не добавлен); расходится с devices.pub_key после смены ключа, пока
        -- сервер не догнали. ip/psk — как их выдал awg-ctrl этого сервера.
        CREATE TABLE IF NOT EXISTS device_peers (
            device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            server_id   INTEGER NOT NULL,
            peer_pub    TEXT,
            ip          TEXT,
            psk         TEXT,
            psk_pending INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY (device_id, server_id)
        );
        -- Пиры, которые надо удалить с сервера, когда до него дотянемся.
        CREATE TABLE IF NOT EXISTS peer_tombstones (
            server_id INTEGER NOT NULL,
            pub_key   TEXT    NOT NULL,
            PRIMARY KEY (server_id, pub_key)
        );
        -- Последний известный профиль сервера: config собирается из него, даже когда
        -- нода не в сети, — иначе сервер пропадал бы из подписки на время обрыва.
        CREATE TABLE IF NOT EXISTS server_profiles (
            server_id  INTEGER PRIMARY KEY,
            profile    TEXT    NOT NULL,
            updated_at INTEGER NOT NULL
        );
    `);
    // Версия клиента — необязательное поле, добавленное позже: у уже созданной
    // таблицы колонки может не быть.
    if (!(uidb.prepare("PRAGMA table_info(devices)").all() as { name: string }[]).some(c => c.name === "version"))
        uidb.exec("ALTER TABLE devices ADD COLUMN version TEXT NOT NULL DEFAULT ''");
    uidb.pragma("foreign_keys = ON");

    // Перенос со времени одного сервера: ключ отдавал master_keys.server_id, а пир устройства
    // уже стоит там под devices.pub_key (ip/psk подтянет reconcile). У ключа всегда есть хотя
    // бы один сервер, поэтому «ни одной строки» бывает только у ключей из старой схемы.
    uidb.exec(`
        INSERT OR IGNORE INTO master_servers (master_id, server_id)
            SELECT id, server_id FROM master_keys
            WHERE id NOT IN (SELECT master_id FROM master_servers);
        INSERT OR IGNORE INTO device_peers (device_id, server_id, peer_pub)
            SELECT d.id, m.server_id, d.pub_key FROM devices d JOIN master_keys m ON m.id = d.master_id
            WHERE d.id NOT IN (SELECT device_id FROM device_peers);
    `);

    const q = {
        masters:      uidb.prepare("SELECT m.*, (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m WHERE revoked_at IS NULL ORDER BY id"),
        masterById:   uidb.prepare<[number]>("SELECT * FROM master_keys WHERE id = ? AND revoked_at IS NULL"),
        masterBySecret: uidb.prepare<[Buffer]>("SELECT * FROM master_keys WHERE secret = ? AND revoked_at IS NULL"),
        masterInsert: uidb.prepare<[string, Buffer, number, number, number]>("INSERT INTO master_keys (label, secret, device_limit, server_id, created_at) VALUES (?, ?, ?, ?, ?)"),
        masterUpdate: uidb.prepare<[string, number, number]>("UPDATE master_keys SET label = ?, device_limit = ? WHERE id = ?"),
        masterSecret: uidb.prepare<[Buffer, number]>("UPDATE master_keys SET secret = ? WHERE id = ?"),
        masterDelete: uidb.prepare<[number]>("DELETE FROM master_keys WHERE id = ?"),
        serversOf:    uidb.prepare<[number]>("SELECT server_id FROM master_servers WHERE master_id = ? ORDER BY server_id"),
        serverAdd:    uidb.prepare<[number, number]>("INSERT OR IGNORE INTO master_servers (master_id, server_id) VALUES (?, ?)"),
        serverDel:    uidb.prepare<[number, number]>("DELETE FROM master_servers WHERE master_id = ? AND server_id = ?"),
        usedServers:  uidb.prepare("SELECT server_id FROM master_servers UNION SELECT server_id FROM peer_tombstones"),
        devicesOf:    uidb.prepare<[number]>("SELECT * FROM devices WHERE master_id = ? ORDER BY id"),
        deviceById:   uidb.prepare<[number]>("SELECT * FROM devices WHERE id = ?"),
        deviceByKey:  uidb.prepare<[number, string]>("SELECT * FROM devices WHERE master_id = ? AND device_id = ?"),
        deviceCount:  uidb.prepare<[number]>("SELECT COUNT(*) AS n FROM devices WHERE master_id = ?"),
        deviceInsert: uidb.prepare<[number, string, string, string, string, string, number, string]>(
            "INSERT INTO devices (master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"),
        deviceVersion: uidb.prepare<[string, number]>("UPDATE devices SET version = ? WHERE id = ?"),
        deviceDelete: uidb.prepare<[number]>("DELETE FROM devices WHERE id = ?"),
        deviceSeen:   uidb.prepare<[number, number]>("UPDATE devices SET last_seen = ? WHERE id = ?"),
        devicePub:    uidb.prepare<[string, number, number]>("UPDATE devices SET pub_key = ?, rekey_requested = ? WHERE id = ?"),
        deviceFlag:   uidb.prepare<[number, number]>("UPDATE devices SET rekey_requested = 1 WHERE id = ? OR master_id = ?"),
        linksOf:      uidb.prepare<[number]>("SELECT * FROM device_peers WHERE device_id = ? ORDER BY server_id"),
        linkAdd:      uidb.prepare<[number, number]>("INSERT OR IGNORE INTO device_peers (device_id, server_id) VALUES (?, ?)"),
        linkDel:      uidb.prepare<[number, number]>("DELETE FROM device_peers WHERE device_id = ? AND server_id = ?"),
        linkSet:      uidb.prepare<[string | null, string | null, string | null, number, number, number]>(
            "UPDATE device_peers SET peer_pub = ?, ip = ?, psk = ?, psk_pending = ? WHERE device_id = ? AND server_id = ?"),
        linkPskDue:   uidb.prepare<[number]>("UPDATE device_peers SET psk_pending = 1 WHERE device_id = ? AND peer_pub IS NOT NULL"),
        // Что на сервере расходится с нужным: пира нет, стоит старый ключ, не знаем ip/psk, ждёт новый PSK.
        linksToFix:   uidb.prepare<[number]>(`
            SELECT l.*, d.pub_key AS dev_pub, d.master_id FROM device_peers l JOIN devices d ON d.id = l.device_id
            WHERE l.server_id = ? AND (l.peer_pub IS NULL OR l.peer_pub <> d.pub_key
                                       OR l.ip IS NULL OR l.psk IS NULL OR l.psk_pending = 1)`),
        tombsOf:      uidb.prepare<[number]>("SELECT pub_key FROM peer_tombstones WHERE server_id = ?"),
        tombAdd:      uidb.prepare<[number, string]>("INSERT OR IGNORE INTO peer_tombstones (server_id, pub_key) VALUES (?, ?)"),
        tombDel:      uidb.prepare<[number, string]>("DELETE FROM peer_tombstones WHERE server_id = ? AND pub_key = ?"),
        profileGet:   uidb.prepare<[number]>("SELECT profile FROM server_profiles WHERE server_id = ?"),
        profileSet:   uidb.prepare<[number, string, number]>(
            "INSERT INTO server_profiles (server_id, profile, updated_at) VALUES (?, ?, ?) ON CONFLICT(server_id) DO UPDATE SET profile = excluded.profile, updated_at = excluded.updated_at"),
    };

    const now = () => Math.floor(Date.now() / 1000);
    const hasCtrlOk = (s: number) => s >= 200 && s < 300;
    const serverIdsOf = (masterId: number) => (q.serversOf.all(masterId) as { server_id: number }[]).map(r => r.server_id);

    // ── Сведение серверов ──────────────────────────────────────────────────
    function storedProfile(id: number): Profile | null {
        const row = q.profileGet.get(id) as { profile: string } | undefined;
        try { return row ? JSON.parse(row.profile) as Profile : null; } catch { return null; }
    }

    async function refreshProfile(id: number, ctrl: Ctrl): Promise<boolean> {
        const r = await ctrl("GET", "/api/profile");
        if (!hasCtrlOk(r.status) || typeof r.data?.endpoint !== "string") return false;
        const json = JSON.stringify(r.data);
        const row = q.profileGet.get(id) as { profile: string } | undefined;
        if (row?.profile !== json) q.profileSet.run(id, json, now());
        return true;
    }

    // Один проход на сервер за раз: параллельные проходы добавили бы одного пира дважды.
    const locks = new Map<number, Promise<void>>();
    function reconcile(id: number): Promise<void> {
        const next = (locks.get(id) ?? Promise.resolve()).then(() => reconcileNow(id)).catch(e => {
            console.warn(`sub: сервер #${id} не сведён: ${(e as Error).message}`);
        });
        locks.set(id, next);
        return next;
    }
    const reconcileAll = (ids: Iterable<number>) => Promise.all([...new Set(ids)].map(reconcile));

    async function reconcileNow(id: number) {
        if (!servers.exists(id) || !servers.isOnline(id)) return;
        const ctrl = servers.ctrlFor(id);
        if (!await refreshProfile(id, ctrl)) return;            // сервер не отвечает — попробуем позже

        // Список пиров сервера — лениво, только если понадобится (409, потерянный ip/psk).
        let listed: Map<string, { ip: string; psk_key: string }> | null = null;
        const onServer = async (pub: string) => {
            if (!listed) {
                const r = await ctrl("GET", "/api/peers");
                if (!hasCtrlOk(r.status)) throw new Error(`peers ${r.status}`);
                listed = new Map((r.data.peers as { pub_key: string; ip: string; psk_key: string }[]).map(p => [p.pub_key, p]));
            }
            return listed.get(pub) ?? null;
        };
        const gone = (s: number) => s === 502 || s === 504;    // связь пропала посреди прохода

        for (const { pub_key } of q.tombsOf.all(id) as { pub_key: string }[]) {
            const r = await ctrl("DELETE", `/api/peers/${pubParam(pub_key)}`);
            if (hasCtrlOk(r.status) || r.status === 404) q.tombDel.run(id, pub_key);
            else if (gone(r.status)) return;
        }

        type Peer = { ip: string; psk_key: string };
        for (const l of q.linksToFix.all(id) as (LinkRow & { dev_pub: string; master_id: number })[]) {
            // null — не вышло, попробуем в следующий проход; "gone" — связь пропала, проход прекращаем.
            const add = async (): Promise<Peer | null | "gone"> => {
                const r = await ctrl("POST", "/api/peers", { pub_key: l.dev_pub, owner: `m${l.master_id}/d${l.device_id}` });
                if (hasCtrlOk(r.status)) return r.data;
                if (r.status === 409) return onServer(l.dev_pub);      // уже добавлен, но ответ потерялся
                return gone(r.status) ? "gone" : null;
            };

            let peer: Peer | null | "gone";
            if (l.peer_pub === null) {
                peer = await add();
            } else if (l.peer_pub !== l.dev_pub) {
                // Ключ сменили, пока сервер был недоступен: тот же IP и PSK, новый ключ.
                const r = await ctrl("PUT", `/api/peers/${pubParam(l.peer_pub)}`, { pub_key: l.dev_pub });
                peer = hasCtrlOk(r.status) ? r.data
                    : r.status === 404 ? await add()                    // старого пира на сервере уже нет
                    : r.status === 409 ? await onServer(l.dev_pub)
                    : gone(r.status) ? "gone" : null;
            } else {
                peer = await onServer(l.dev_pub) ?? await add();         // ip/psk не знаем или пир пропал с сервера
            }
            if (peer === "gone") return;
            if (!peer) continue;

            let pending = 0;
            if (l.psk_pending === 1) {
                const r = await ctrl("POST", `/api/peers/${pubParam(l.dev_pub)}/psk`);
                if (hasCtrlOk(r.status)) peer = r.data as Peer;
                else if (gone(r.status)) return;
                else pending = 1;                                        // пир на месте, PSK сменим позже
            }
            q.linkSet.run(l.dev_pub, peer.ip, peer.psk_key, pending, l.device_id, id);
        }
    }

    /** Устройство удаляется: его пиров — в список на удаление, строку — из реестра. */
    function forgetDevice(dev: DeviceRow): number[] {
        const links = q.linksOf.all(dev.id) as LinkRow[];
        uidb.transaction(() => {
            for (const l of links) if (l.peer_pub) q.tombAdd.run(l.server_id, l.peer_pub);
            q.deviceDelete.run(dev.id);                         // device_peers уйдут каскадом
        })();
        return links.map(l => l.server_id);
    }

    // ── config ─────────────────────────────────────────────────────────────
    const hostOf = (endpoint: string) => endpoint.slice(0, endpoint.lastIndexOf(":"));

    function subHost(req?: Request): string {
        if (SUB_HOST) return SUB_HOST;
        const local = servers.exists(0) ? storedProfile(0) : null;
        if (local) return hostOf(local.endpoint);
        return (req?.hostname ?? "").replace(/^\[|\]$/g, "");
    }

    // В servers[] — только серверы, где уже стоит текущий ключ устройства и известны ip/psk.
    // Сервер, который ещё не догнали (нода не в сети при регистрации или смене ключа), появится,
    // когда его сведут, — rev при этом сменится, и клиент подтянет.
    function buildConfig(dev: DeviceRow, req?: Request) {
        const list = [];
        for (const l of q.linksOf.all(dev.id) as LinkRow[]) {
            const prof = storedProfile(l.server_id);
            if (!prof || l.peer_pub !== dev.pub_key || !l.ip || !l.psk) continue;
            list.push({
                id: l.server_id, name: prof.name, endpoint: prof.endpoint, server_pub: prof.server_pub,
                psk: l.psk, address: `${l.ip}/32`, dns: prof.dns,
                keepalive: prof.keepalive, mtu: prof.mtu, gen: prof.gen, awg: prof.awg,
            });
        }
        const cfg = {
            rekey: dev.rekey_requested === 1,
            endpoints: [`${subHost(req)}:${SUB_PORT}`],
            servers: list,
        };
        // rev меняется при любом изменении; клиент по нему решает, трогать ли туннель.
        const rev = crypto.createHash("sha256").update(JSON.stringify(cfg)).digest("hex").slice(0, 16);
        return { rev, ...cfg };
    }

    // Конфиг, в котором есть хотя бы один сервер. Пустой не отдаём: клиент снёс бы туннель
    // из-за временной недоступности нод — пусть лучше живёт со старым конфигом.
    async function readyConfig(dev: DeviceRow, req: Request) {
        let cfg = buildConfig(dev, req);
        if (cfg.servers.length === 0) {
            await reconcileAll((q.linksOf.all(dev.id) as LinkRow[]).map(l => l.server_id));
            cfg = buildConfig(q.deviceById.get(dev.id) as DeviceRow, req);
        }
        if (cfg.servers.length === 0) throw new Error("no servers");
        return cfg;
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
        const serverIds = serverIdsOf(master.id).filter(id => servers.exists(id));
        if (serverIds.length === 0) { send(res, 502, { error: "unavailable" }); return; }

        let deviceRowId = 0;
        let touched: number[] = [];
        try {
            uidb.transaction(() => {
                const existing = q.deviceByKey.get(master.id, deviceId) as DeviceRow | undefined;
                if (existing) touched = forgetDevice(existing);         // переустановка — старые пиры на удаление
                else if ((q.deviceCount.get(master.id) as { n: number }).n >= master.device_limit)
                    throw Object.assign(new Error("limit"), { code: "limit" });
                deviceRowId = Number(q.deviceInsert.run(master.id, deviceId, name, platform, pubKey, authPubS, now(), version).lastInsertRowid);
                for (const sid of serverIds) q.linkAdd.run(deviceRowId, sid);
            })();
        } catch (e: any) {
            if (e?.code === "limit") { send(res, 403, { error: "device_limit" }); return; }
            send(res, 409, { error: "conflict" }); return; // UNIQUE: pub_key/auth_pub уже заняты
        }

        await reconcileAll([...touched, ...serverIds]);
        const dev = q.deviceById.get(deviceRowId) as DeviceRow;
        const cfg = buildConfig(dev, req);
        if (cfg.servers.length === 0) {
            // Ни один сервер пира не принял — регистрацию откатываем, клиент повторит позже.
            await reconcileAll(forgetDevice(dev));
            send(res, 502, { error: "unavailable" });
            return;
        }
        // devices / device_limit: сколько мест занято с этим устройством — клиент показывает это сразу,
        // без отдельного запроса списка.
        send(res, 201, {
            device: dev.id, config: cfg,
            devices: (q.deviceCount.get(master.id) as { n: number }).n, device_limit: master.device_limit,
        });
    }));

    // Сколько мест у ключа занято — клиент показывает это, пока ссылку только вставили и ещё ничего не
    // регистрировали. Запрос не подписан устройством (его ещё нет): доступ по секрету из ссылки, как у register,
    // а ответ подписан, как всегда. Неверный секрет считается в failLimit — секрет не перебрать.
    sub.post("/sub/v1/peek", wrap(async (req, res) => {
        const ip = req.socket.remoteAddress ?? "unknown";
        const secretB = typeof (req.body as any)?.sub === "string" && /^[A-Za-z0-9_-]{22}$/.test((req.body as any).sub) ? (req.body as any).sub as string : null;
        const master = secretB ? q.masterBySecret.get(Buffer.from(secretB, "base64url")) as MasterRow | undefined : undefined;
        if (!master) {
            if (!failLimit(ip)) { send(res, 429, { error: "rate_limited" }); return; }
            send(res, secretB ? 404 : 400, { error: secretB ? "not_found" : "bad_request" });
            return;
        }
        send(res, 200, { devices: (q.deviceCount.get(master.id) as { n: number }).n, device_limit: master.device_limit });
    }));

    sub.get("/sub/v1/config", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        q.deviceSeen.run(now(), dev.id);
        // Версия клиента меняется при обновлении приложения; заголовок не подписан — это лишь подпись в панели.
        const v = String(req.headers["x-sen-version"] ?? "");
        if (CLIENT_VERSION.test(v) && v !== dev.version) q.deviceVersion.run(v, dev.id);
        send(res, 200, { config: await readyConfig(dev, req) });
    }));

    // Устройства того же мастер-ключа: клиент показывает их на вкладке «Ключ». Только чтение — отвязать
    // можно лишь себя (DELETE /sub/v1/device); чужие устройства убирает панель.
    sub.get("/sub/v1/devices", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        const master = q.masterById.get(dev.master_id) as MasterRow | undefined;
        if (!master) { send(res, 404, { error: "not_found" }); return; }
        send(res, 200, {
            name: master.label,
            device_limit: master.device_limit,
            devices: (q.devicesOf.all(master.id) as DeviceRow[]).map(d => ({
                id: d.id, name: d.device_name, platform: d.platform, version: d.version,
                created_at: d.created_at, last_seen: d.last_seen, current: d.id === dev.id,
            })),
        });
    }));

    // Новый ключ ставится на все серверы ключа: где сервер доступен — сразу (тот же IP и PSK),
    // где нет — когда он появится. Если не принял ни один — ключ не меняем, клиент повторит.
    sub.post("/sub/v1/rekey", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        const pub = (req.body as { pub_key?: unknown } | undefined)?.pub_key;
        if (typeof pub !== "string" || !B64_PUB.test(pub)) { send(res, 400, { error: "bad_request" }); return; }
        if (pub === dev.pub_key) { send(res, 200, { config: await readyConfig(dev, req) }); return; }
        try { q.devicePub.run(pub, 0, dev.id); }
        catch { send(res, 409, { error: "conflict" }); return; }  // этот ключ уже у другого устройства

        const ids = (q.linksOf.all(dev.id) as LinkRow[]).map(l => l.server_id);
        await reconcileAll(ids);
        const cfg = buildConfig(q.deviceById.get(dev.id) as DeviceRow, req);
        if (cfg.servers.length === 0) {
            q.devicePub.run(dev.pub_key, dev.rekey_requested, dev.id);
            await reconcileAll(ids);                             // вернуть старый ключ туда, где успели сменить
            send(res, 502, { error: "unavailable" });
            return;
        }
        send(res, 200, { config: cfg });
    }));

    sub.delete("/sub/v1/device", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        await reconcileAll(forgetDevice(dev));
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

    // Набор серверов из тела запроса: непустой, без повторов, только существующие.
    function parseServers(v: unknown): number[] | string {
        if (!Array.isArray(v) || v.length === 0) return "Нужен хотя бы один сервер";
        if (v.length > MAX_SERVERS) return `Не больше ${MAX_SERVERS} серверов`;
        const ids = [...new Set(v)];
        if (!ids.every(id => Number.isInteger(id) && servers.exists(id as number))) return "Такого сервера нет";
        return (ids as number[]).sort((a, b) => a - b);
    }

    const masterKeys = Router();

    masterKeys.get("/", (_req, res) => {
        res.json({
            enabled: signKey !== null,
            tls: tlsPin !== null,
            keys: (q.masters.all() as (MasterRow & { devices: number })[]).map(m => ({
                id: m.id, label: m.label, device_limit: m.device_limit,
                devices: m.devices, servers: serverIdsOf(m.id), created_at: m.created_at,
            })),
        });
    });

    masterKeys.post("/", (req, res) => {
        const { label, device_limit, servers: wanted } = (req.body ?? {}) as { label?: string; device_limit?: number; servers?: unknown };
        if (typeof label !== "string" || !LABEL.test(label)) { res.status(400).json({ error: "Метка: до 40 символов, без переводов строк и <>" }); return; }
        const limit = device_limit === undefined ? DEFAULT_LIMIT : device_limit;
        if (!limitOk(limit)) { res.status(400).json({ error: "Лимит устройств: от 1 до 100" }); return; }
        const ids = parseServers(wanted === undefined ? [servers.defaultId()] : wanted);
        if (typeof ids === "string") { res.status(400).json({ error: wanted === undefined ? "Сначала добавьте сервер" : ids }); return; }
        let id = 0;
        uidb.transaction(() => {
            id = Number(q.masterInsert.run(label, crypto.randomBytes(16), limit, ids[0], now()).lastInsertRowid);
            for (const sid of ids) q.serverAdd.run(id, sid);
        })();
        res.status(201).json({ id, label, device_limit: limit, devices: 0, servers: ids });
    });

    masterKeys.patch("/:id", aw(async (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        const { label, device_limit, servers: wanted } = (req.body ?? {}) as { label?: string; device_limit?: number; servers?: unknown };
        const nl = label ?? m.label, nd = device_limit ?? m.device_limit;
        if (typeof nl !== "string" || !LABEL.test(nl)) { res.status(400).json({ error: "Неверная метка" }); return; }
        if (!limitOk(nd)) { res.status(400).json({ error: "Лимит устройств: от 1 до 100" }); return; }

        let ids = serverIdsOf(m.id);
        const affected: number[] = [];
        if (wanted !== undefined) {
            const next = parseServers(wanted);
            if (typeof next === "string") { res.status(400).json({ error: next }); return; }
            const added = next.filter(s => !ids.includes(s)), removed = ids.filter(s => !next.includes(s));
            const devices = q.devicesOf.all(m.id) as DeviceRow[];
            uidb.transaction(() => {
                for (const sid of removed) {
                    q.serverDel.run(m.id, sid);
                    for (const d of devices) {
                        const l = (q.linksOf.all(d.id) as LinkRow[]).find(x => x.server_id === sid);
                        if (l?.peer_pub) q.tombAdd.run(sid, l.peer_pub);
                        q.linkDel.run(d.id, sid);
                    }
                }
                for (const sid of added) {
                    q.serverAdd.run(m.id, sid);
                    for (const d of devices) q.linkAdd.run(d.id, sid);
                }
            })();
            affected.push(...added, ...removed);
            ids = next;
        }
        q.masterUpdate.run(nl, nd, m.id);
        await reconcileAll(affected);
        res.json({ id: m.id, label: nl, device_limit: nd, servers: ids });
    }));

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
        const affected: number[] = [];
        for (const d of q.devicesOf.all(m.id) as DeviceRow[]) affected.push(...forgetDevice(d));
        q.masterDelete.run(m.id); // master_servers уйдут каскадом
        await reconcileAll(affected);
        res.json({ success: true, id: m.id });
    }));

    masterKeys.get("/:id/link", (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        if (!signKey || !signPub || !SUB_PORT) { res.status(503).json({ error: "Подписка не настроена на сервере" }); return; }
        const host = subHost(req);
        if (!host) { res.status(503).json({ error: "Не удалось определить адрес панели — задайте SUB_HOST" }); return; }
        const link = encodeSenLink({
            tls: tlsPin !== null,
            addrs: [{ host, port: SUB_PORT }],
            secret: m.secret, signPub, tlsPin: tlsPin ?? undefined, name: m.label,
        });
        res.json({ link, tls: tlsPin !== null });
    });

    // Статистика устройства — по всем серверам ключа: онлайн, если есть свежий handshake хоть
    // на одном; трафик суммируется. servers_ok — на скольких серверах уже стоит его текущий ключ.
    masterKeys.get("/:id/devices", aw(async (req, res) => {
        const m = q.masterById.get(idOf(req)) as MasterRow | undefined;
        if (!m) { res.status(404).json({ error: "Не найден" }); return; }
        const stats = new Map<string, { online: boolean; lastHandshake: number; rx: number; tx: number }>();
        await Promise.all(serverIdsOf(m.id).filter(id => servers.isOnline(id)).map(async id => {
            const r = await servers.ctrlFor(id)("GET", "/api/peers");
            if (!hasCtrlOk(r.status)) return;
            for (const p of r.data.peers as { pub_key: string; online: boolean; lastHandshake: number; rx: number; tx: number }[]) {
                const s = stats.get(p.pub_key) ?? { online: false, lastHandshake: 0, rx: 0, tx: 0 };
                stats.set(p.pub_key, {
                    online: s.online || p.online, lastHandshake: Math.max(s.lastHandshake, p.lastHandshake),
                    rx: s.rx + p.rx, tx: s.tx + p.tx,
                });
            }
        }));
        res.json({
            devices: (q.devicesOf.all(m.id) as DeviceRow[]).map(d => {
                const links = q.linksOf.all(d.id) as LinkRow[];
                const s = stats.get(d.pub_key);
                return {
                    id: d.id, device_id: d.device_id, device_name: d.device_name, platform: d.platform, version: d.version, created_at: d.created_at,
                    last_seen: d.last_seen, rekey_requested: d.rekey_requested === 1,
                    online: s?.online ?? false, lastHandshake: s?.lastHandshake ?? 0, rx: s?.rx ?? 0, tx: s?.tx ?? 0,
                    servers_total: links.length, servers_ok: links.filter(l => l.peer_pub === d.pub_key && l.ip).length,
                };
            }),
        });
    }));

    const devices = Router();

    devices.delete("/:id", aw(async (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        await reconcileAll(forgetDevice(d));
        res.json({ success: true, id: d.id });
    }));

    devices.post("/:id/rekey", (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        q.deviceFlag.run(d.id, -1);
        res.json({ id: d.id });
    });

    // Новый PSK на каждом сервере; недоступные получат его, когда появятся.
    devices.post("/:id/psk", aw(async (req, res) => {
        const d = q.deviceById.get(idOf(req)) as DeviceRow | undefined;
        if (!d) { res.status(404).json({ error: "Не найдено" }); return; }
        q.linkPskDue.run(d.id);
        await reconcileAll((q.linksOf.all(d.id) as LinkRow[]).map(l => l.server_id));
        const pending = (q.linksOf.all(d.id) as LinkRow[]).filter(l => l.psk_pending === 1).length;
        res.json({ id: d.id, pending });
    }));

    // ── Листенер и фоновое сведение ────────────────────────────────────────
    servers.onOnline(id => { void reconcile(id); });

    function start() {
        // Сводим и без подписки: у ключей могут остаться хвосты на удаление.
        setInterval(() => {
            void reconcileAll((q.usedServers.all() as { server_id: number }[]).map(r => r.server_id));
        }, SYNC_EVERY).unref();

        if (!signKey) return;
        if (!SUB_PORT) { console.warn("sub: SUB_PORT не задан — подписка отключена"); signKey = null; return; }
        const server = tlsMaterial ? https.createServer(tlsMaterial, sub) : http.createServer(sub);
        server.listen(SUB_PORT, () => {
            console.log(`sub: listening on :${SUB_PORT} (${tlsMaterial ? "https" : "http"})`);
        });
    }

    return { masterKeys, devices, start };
}
