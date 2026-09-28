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
//
// Каждый проход заканчивается сведением пиров панели (PUT /api/peers/sync): awg-ctrl
// получает полный список ключей, которые этой панели нужны на сервере, и снимает из её
// области всё остальное. Это страховка от пиров, чья судьба потерялась (ответ ноды не
// дошёл, устройство удалили посреди прохода). Область — id панели в owner пира
// («<panel>/m1/d2»): у ноды может быть своя панель на том же awg-ctrl, чужих не трогаем.
// Удаляем только по такому списку, никогда по времени: панель не на связи — ничего не
// происходит, неактивные устройства никто не снимает.
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
    beforeDelete(cb: (id: number, force: boolean) => Promise<{ error: string; forcible: boolean } | null>): void;
    afterDelete(cb: (id: number) => void): void;
}

interface MasterRow {
    id: number; uuid: string; label: string; secret: Buffer; device_limit: number;
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
// onLimited зовётся один раз за окно — на первом отказе, чтобы лог не забивало.
function makeLimiter(max: number, windowMs: number, onLimited?: (ip: string) => void) {
    const hits = new Map<string, { n: number; reset: number }>();
    return (ip: string): boolean => {
        const now = Date.now();
        const e = hits.get(ip);
        if (!e || e.reset < now) { hits.set(ip, { n: 1, reset: now + windowMs }); return true; }
        if (e.n >= max) {
            if (e.n === max) { e.n++; onLimited?.(ip); }
            return false;
        }
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
        CREATE TABLE IF NOT EXISTS sub_meta (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );
    `);
    // Версия клиента — необязательное поле, добавленное позже: у уже созданной
    // таблицы колонки может не быть.
    if (!(uidb.prepare("PRAGMA table_info(devices)").all() as { name: string }[]).some(c => c.name === "version"))
        uidb.exec("ALTER TABLE devices ADD COLUMN version TEXT NOT NULL DEFAULT ''");
    // uuid мастер-ключа — постоянный идентификатор: не меняется ни при перевыпуске ссылки,
    // ни при правке метки. Существующим ключам выдаётся здесь один раз.
    if (!(uidb.prepare("PRAGMA table_info(master_keys)").all() as { name: string }[]).some(c => c.name === "uuid"))
        uidb.exec("ALTER TABLE master_keys ADD COLUMN uuid TEXT");
    {
        const setUuid = uidb.prepare<[string, number]>("UPDATE master_keys SET uuid = ? WHERE id = ?");
        for (const { id } of uidb.prepare("SELECT id FROM master_keys WHERE uuid IS NULL").all() as { id: number }[])
            setUuid.run(crypto.randomUUID(), id);
    }
    uidb.exec("CREATE UNIQUE INDEX IF NOT EXISTS master_keys_uuid ON master_keys (uuid)");
    uidb.pragma("foreign_keys = ON");

    // Перенос со времени одного сервера: ключ отдавал master_keys.server_id, а пир устройства
    // уже стоит там под devices.pub_key (ip/psk подтянет reconcile). У ключа всегда есть хотя
    // бы один сервер, поэтому «ни одной строки» бывает только у ключей из старой схемы.
    uidb.exec(`
        INSERT OR IGNORE INTO master_servers (master_id, server_id)
            SELECT id, server_id FROM master_keys
            WHERE revoked_at IS NULL AND id NOT IN (SELECT master_id FROM master_servers);
        INSERT OR IGNORE INTO device_peers (device_id, server_id, peer_pub)
            SELECT d.id, m.server_id, d.pub_key FROM devices d JOIN master_keys m ON m.id = d.master_id
            WHERE d.id NOT IN (SELECT device_id FROM device_peers);
    `);

    // Id панели — область её пиров на серверах. Живёт, пока живёт ui.db; копия в panel.id
    // нужна CLI (у него нет SQLite), чтобы отличать пиров этой панели на awg-ctrl.
    uidb.prepare("INSERT OR IGNORE INTO sub_meta (key, value) VALUES ('panel_id', ?)").run(crypto.randomBytes(6).toString("hex"));
    const panelId = (uidb.prepare("SELECT value FROM sub_meta WHERE key = 'panel_id'").get() as { value: string }).value;
    try { fs.writeFileSync(path.join(deps.baseDir, "panel.id"), panelId + "\n"); }
    catch (e) { console.warn(`sub: не удалось записать panel.id: ${(e as Error).message}`); }

    const q = {
        // И отозванные: удалённый ключ висит в списке, пока все его серверы не подтвердят удаление.
        masters:      uidb.prepare("SELECT m.*, (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m ORDER BY id"),
        masterById:   uidb.prepare<[number]>("SELECT * FROM master_keys WHERE id = ? AND revoked_at IS NULL"),
        masterRow:    uidb.prepare<[number]>("SELECT m.*, (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m WHERE id = ?"),
        masterBySecret: uidb.prepare<[Buffer]>("SELECT * FROM master_keys WHERE secret = ? AND revoked_at IS NULL"),
        masterInsert: uidb.prepare<[string, string, Buffer, number, number, number]>("INSERT INTO master_keys (uuid, label, secret, device_limit, server_id, created_at) VALUES (?, ?, ?, ?, ?, ?)"),
        masterIdByUuid: uidb.prepare<[string]>("SELECT id FROM master_keys WHERE uuid = ?"),
        masterUpdate: uidb.prepare<[string, number, number]>("UPDATE master_keys SET label = ?, device_limit = ? WHERE id = ?"),
        masterSecret: uidb.prepare<[Buffer, number]>("UPDATE master_keys SET secret = ? WHERE id = ?"),
        masterRevoke: uidb.prepare<[number, number]>("UPDATE master_keys SET revoked_at = ? WHERE id = ?"),
        // Сервер сведён и надгробий на нём нет — отозванным ключам он больше не нужен.
        revokedServerDone: uidb.prepare<[number]>(
            "DELETE FROM master_servers WHERE server_id = ? AND master_id IN (SELECT id FROM master_keys WHERE revoked_at IS NOT NULL)"),
        revokedGone:  uidb.prepare("DELETE FROM master_keys WHERE revoked_at IS NOT NULL AND id NOT IN (SELECT master_id FROM master_servers)"),
        liveOnServer: uidb.prepare<[number]>(
            "SELECT m.label FROM master_servers s JOIN master_keys m ON m.id = s.master_id WHERE s.server_id = ? AND m.revoked_at IS NULL"),
        revokedOnServer: uidb.prepare<[number]>(
            "SELECT m.label FROM master_servers s JOIN master_keys m ON m.id = s.master_id WHERE s.server_id = ? AND m.revoked_at IS NOT NULL"),
        serverRelease: uidb.prepare<[number]>("DELETE FROM master_servers WHERE server_id = ?"),
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
        tombCount:    uidb.prepare<[number]>("SELECT COUNT(*) AS n FROM peer_tombstones WHERE server_id = ?"),
        // Что панели нужно на сервере: текущий ключ каждого устройства и ключ, который там стоит
        // сейчас (расходится с текущим, пока смена ключа не догнала сервер).
        keepOf:       uidb.prepare<[number, number]>(`
            SELECT d.pub_key AS pub FROM device_peers l JOIN devices d ON d.id = l.device_id WHERE l.server_id = ?
            UNION SELECT peer_pub FROM device_peers WHERE server_id = ? AND peer_pub IS NOT NULL`),
        // Связи, которые считаются сведёнными: на сервере должен стоять текущий ключ устройства.
        linksSettled: uidb.prepare<[number]>(`
            SELECT l.device_id, l.peer_pub FROM device_peers l JOIN devices d ON d.id = l.device_id
            WHERE l.server_id = ? AND l.peer_pub = d.pub_key`),
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
                const r = await ctrl("POST", "/api/peers", { pub_key: l.dev_pub, owner: `${panelId}/m${l.master_id}/d${l.device_id}` });
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

        await syncPeers(id, ctrl);
    }

    // Сведение пиров панели на сервере — в конце прохода и под его блокировкой: список строится
    // из ui.db сейчас, поэтому устройство, удалённое посреди прохода, в него уже не попадёт, и
    // ключ, поставленный для него этим проходом, снимется здесь же.
    async function syncPeers(id: number, ctrl: Ctrl) {
        const keep = (q.keepOf.all(id, id) as { pub: string }[]).map(r => r.pub);
        const r = await ctrl("PUT", "/api/peers/sync", { owner: panelId, keep });
        if (hasCtrlOk(r.status)) {
            // Самолечение: ключ считается стоящим, а его нет (откат смены ключа после потерянного
            // ответа, пир сняли руками) — забываем и ставим заново следующим проходом.
            const present = new Set(r.data?.present as string[]);
            let lost = 0;
            for (const l of q.linksSettled.all(id) as { device_id: number; peer_pub: string }[]) {
                if (present.has(l.peer_pub)) continue;
                q.linkSet.run(null, null, null, 0, l.device_id, id);
                lost++;
            }
            if (lost) void reconcile(id);
        } else if (r.status !== 400 && r.status !== 413) {
            return;     // 400/413 — awg-ctrl старше sync: чистим, как раньше, одними надгробиями
        }
        if ((q.tombCount.get(id) as { n: number }).n > 0) return;
        q.revokedServerDone.run(id);
        q.revokedGone.run();
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

    const limited = (what: string) => (ip: string) => console.warn(`sub: 429 ${what} ip=${ip} — лимит исчерпан на 15 минут`);
    const registerLimit = makeLimiter(30, 15 * 60 * 1000, limited("register"));
    const failLimit     = makeLimiter(30, 15 * 60 * 1000, limited("неудачные запросы"));
    const seenSigs      = new Map<string, number>(); // подпись → когда забыть
    const ipOf = (req: Request) => req.socket.remoteAddress ?? "unknown";

    // Ответ подписан и несёт эхо запроса: device (X-Sen-Device) и ts (X-Sen-Ts). Клиент сверяет
    // их со своим запросом — иначе подсунуть ему можно было бы чужой подписанный ответ (чужой
    // revoked, старый config), подпись ведь на запрос не завязана.
    function send(req: Request, res: Response, status: number, payload: object) {
        const echo: { device?: number; ts?: number } = {};
        const device = Number(req.headers["x-sen-device"]), ts = Number(req.headers["x-sen-ts"]);
        if (req.headers["x-sen-device"] !== undefined && Number.isSafeInteger(device)) echo.device = device;
        if (req.headers["x-sen-ts"] !== undefined && Number.isSafeInteger(ts)) echo.ts = ts;
        // Поля ответа важнее эха: device из тела register не перетрёт лишний заголовок.
        res.status(status).json(signResponse({ ...echo, ...payload }, signKey as crypto.KeyObject));
    }

    // Проверка подписи запроса + защита от повторов. auth_pub — 32 байта.
    // null — всё верно, иначе причина отказа (для лога).
    function signFailure(req: Request, authPub: Buffer): string | null {
        const ts  = Number(req.headers["x-sen-ts"]);
        const sig = String(req.headers["x-sen-sig"] ?? "");
        if (!Number.isInteger(ts) || !sig) return "no_signature";
        if (Math.abs(ts - now()) > TS_WINDOW) return `ts_window (часы клиента расходятся на ${ts - now()} с)`;
        const body = ((req as any).rawBody as Buffer | undefined) ?? Buffer.alloc(0);
        if (!verifyRequest(req.method, req.path, ts, body, sig, authPub)) return "bad_signature";
        const t = Date.now();
        for (const [k, exp] of seenSigs) if (exp < t) seenSigs.delete(k);
        if (seenSigs.has(sig)) return "replay";
        seenSigs.set(sig, t + 2 * TS_WINDOW * 1000);
        return null;
    }

    interface DevReq extends Request { device?: DeviceRow }

    function deviceAuth(req: Request, res: Response, next: NextFunction) {
        const id = Number(req.headers["x-sen-device"]);
        const dev = Number.isSafeInteger(id) && id > 0 ? q.deviceById.get(id) as DeviceRow | undefined : undefined;
        // Устройства нет — его удалили (id не переиспользуются: AUTOINCREMENT). Отвечаем 410,
        // чтобы клиент снял конфиг, и не считаем в лимит неудач: такие клиенты опрашивают
        // по таймеру и иначе выбивали бы лимит соседям за тем же NAT. Существующему
        // устройству 410 не выдать, поэтому подписанный ответ тут можно отдать без проверки.
        if (!dev && Number.isSafeInteger(id) && id > 0) { send(req, res, 410, { error: "revoked" }); return; }
        const why = dev ? signFailure(req, Buffer.from(dev.auth_pub, "base64url")) : "no_device";
        if (!why) {
            (req as DevReq).device = dev;
            next();
            return;
        }
        // Лимитируем только неудачи: живой клиент, опрашивающий config, под лимит не попадает.
        const ip = ipOf(req);
        if (!failLimit(ip)) { send(req, res, 429, { error: "rate_limited" }); return; }
        console.warn(`sub: 401 ${req.method} ${req.path} device=${req.headers["x-sen-device"] ?? "-"} ${why} ip=${ip}`);
        send(req, res, 401, { error: "unauthorized" });
    }

    const wrap = (fn: (req: Request, res: Response) => Promise<void>) =>
        (req: Request, res: Response) => { fn(req, res).catch(() => { send(req, res, 502, { error: "unavailable" }); }); };

    sub.post("/sub/v1/register", wrap(async (req, res) => {
        const ip = ipOf(req);
        if (!registerLimit(ip)) { send(req, res, 429, { error: "rate_limited" }); return; }

        const b = (req.body ?? {}) as Record<string, unknown>;
        const str = (v: unknown, re: RegExp) => typeof v === "string" && re.test(v) ? v : null;
        const secretB = str(b.sub, /^[A-Za-z0-9_-]{22}$/);
        const deviceId = str(b.device_id, /^[\w.-]{1,64}$/);
        const pubKey = str(b.pub_key, B64_PUB);
        const authPubS = str(b.auth_pub, /^[A-Za-z0-9_-]{43}$/);
        const name = typeof b.device_name === "string" ? b.device_name.slice(0, 64) : "";
        const platform = typeof b.platform === "string" ? b.platform.slice(0, 32) : "";
        const version = str(b.version, CLIENT_VERSION) ?? "";
        if (!secretB || !deviceId || !pubKey || !authPubS) { send(req, res, 400, { error: "bad_request" }); return; }

        // Регистрация подписана ключом auth_pub из самого тела — доказательство владения.
        const authPub = Buffer.from(authPubS, "base64url");
        const why = authPub.length !== 32 ? "bad_auth_pub" : signFailure(req, authPub);
        if (why) {
            console.warn(`sub: 401 register ${why} ip=${ip}`);
            send(req, res, 401, { error: "unauthorized" });
            return;
        }

        const master = q.masterBySecret.get(Buffer.from(secretB, "base64url")) as MasterRow | undefined;
        if (!master) { send(req, res, 404, { error: "not_found" }); return; }
        const serverIds = serverIdsOf(master.id).filter(id => servers.exists(id));
        if (serverIds.length === 0) { send(req, res, 502, { error: "unavailable" }); return; }

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
            if (e?.code === "limit") { send(req, res, 403, { error: "device_limit" }); return; }
            send(req, res, 409, { error: "conflict" }); return; // UNIQUE: pub_key/auth_pub уже заняты
        }

        await reconcileAll([...touched, ...serverIds]);
        const dev = q.deviceById.get(deviceRowId) as DeviceRow;
        const cfg = buildConfig(dev, req);
        if (cfg.servers.length === 0) {
            // Ни один сервер пира не принял — регистрацию откатываем, клиент повторит позже.
            await reconcileAll(forgetDevice(dev));
            send(req, res, 502, { error: "unavailable" });
            return;
        }
        // devices / device_limit: сколько мест занято с этим устройством — клиент показывает это сразу,
        // без отдельного запроса списка.
        send(req, res, 201, {
            device: dev.id, config: cfg, key_uuid: master.uuid,
            devices: (q.deviceCount.get(master.id) as { n: number }).n, device_limit: master.device_limit,
        });
    }));

    // Сколько мест у ключа занято — клиент показывает это, пока ссылку только вставили и ещё ничего не
    // регистрировали. Запрос не подписан устройством (его ещё нет): доступ по секрету из ссылки, как у register,
    // а ответ подписан, как всегда. Неверный секрет считается в failLimit — секрет не перебрать.
    sub.post("/sub/v1/peek", wrap(async (req, res) => {
        const ip = ipOf(req);
        const secretB = typeof (req.body as any)?.sub === "string" && /^[A-Za-z0-9_-]{22}$/.test((req.body as any).sub) ? (req.body as any).sub as string : null;
        const master = secretB ? q.masterBySecret.get(Buffer.from(secretB, "base64url")) as MasterRow | undefined : undefined;
        if (!master) {
            if (!failLimit(ip)) { send(req, res, 429, { error: "rate_limited" }); return; }
            send(req, res, secretB ? 404 : 400, { error: secretB ? "not_found" : "bad_request" });
            return;
        }
        send(req, res, 200, { key_uuid: master.uuid, devices: (q.deviceCount.get(master.id) as { n: number }).n, device_limit: master.device_limit });
    }));

    sub.get("/sub/v1/config", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        q.deviceSeen.run(now(), dev.id);
        // Версия клиента меняется при обновлении приложения; заголовок не подписан — это лишь подпись в панели.
        const v = String(req.headers["x-sen-version"] ?? "");
        if (CLIENT_VERSION.test(v) && v !== dev.version) q.deviceVersion.run(v, dev.id);
        send(req, res, 200, { config: await readyConfig(dev, req) });
    }));

    // Устройства того же мастер-ключа: клиент показывает их на вкладке «Ключ». Только чтение — отвязать
    // можно лишь себя (DELETE /sub/v1/device); чужие устройства убирает панель.
    sub.get("/sub/v1/devices", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        const master = q.masterById.get(dev.master_id) as MasterRow | undefined;
        if (!master) { send(req, res, 404, { error: "not_found" }); return; }
        send(req, res, 200, {
            name: master.label,
            key_uuid: master.uuid,
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
        if (typeof pub !== "string" || !B64_PUB.test(pub)) { send(req, res, 400, { error: "bad_request" }); return; }
        if (pub === dev.pub_key) { send(req, res, 200, { config: await readyConfig(dev, req) }); return; }
        try { q.devicePub.run(pub, 0, dev.id); }
        catch { send(req, res, 409, { error: "conflict" }); return; }  // этот ключ уже у другого устройства

        const ids = (q.linksOf.all(dev.id) as LinkRow[]).map(l => l.server_id);
        await reconcileAll(ids);
        const cfg = buildConfig(q.deviceById.get(dev.id) as DeviceRow, req);
        if (cfg.servers.length === 0) {
            q.devicePub.run(dev.pub_key, dev.rekey_requested, dev.id);
            await reconcileAll(ids);                             // вернуть старый ключ туда, где успели сменить
            send(req, res, 502, { error: "unavailable" });
            return;
        }
        send(req, res, 200, { config: cfg });
    }));

    sub.delete("/sub/v1/device", deviceAuth, wrap(async (req, res) => {
        const dev = (req as DevReq).device as DeviceRow;
        await reconcileAll(forgetDevice(dev));
        send(req, res, 200, { ok: true });
    }));

    sub.use((req, res) => { send(req, res, 404, { error: "not_found" }); });
    // Битый JSON и т. п.: ответ всё равно подписан, чтобы клиент мог ему доверять.
    sub.use((_err: unknown, req: Request, res: Response, _next: NextFunction) => { send(req, res, 400, { error: "bad_request" }); });

    // ── Мастер-ключи: общая логика панели и внешнего API ───────────────────
    // Панель (/ui/masterkeys, /ui/devices, за JWT) видит всё. Внешний API (/api/v1/masterkeys,
    // ключ awgk_) — только ключи, чьи серверы целиком входят в набор серверов API-ключа:
    // иначе он мог бы удалить или перенастроить раздачу на сервере, которого не видит.
    // Scope: null — панель, иначе набор серверов API-ключа.
    type Scope = number[] | null;

    class ApiError extends Error {
        constructor(public status: number, message: string) { super(message); }
    }
    const fail = (status: number, message: string): never => { throw new ApiError(status, message); };

    // ApiError — её статус; остальное (awg-ctrl не ответил посреди действия) — 502.
    const aw = (fn: (req: Request, res: Response) => unknown) => (req: Request, res: Response) => {
        Promise.resolve().then(() => fn(req, res)).catch(e => {
            if (e instanceof ApiError) res.status(e.status).json({ error: e.message });
            else res.status(502).json({ error: "awg-ctrl недоступен" });
        });
    };
    const idOf = (v: string) => { const n = Number(v); return Number.isSafeInteger(n) ? n : -1; };   // -1 — не найдётся
    // Мастер-ключ в пути — числовой id или uuid.
    const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
    const masterIdOf = (v: string) => UUID_RE.test(v)
        ? (q.masterIdByUuid.get(v.toLowerCase()) as { id: number } | undefined)?.id ?? -1
        : idOf(v);
    const LABEL = /^[^\r\n<>]{1,40}$/;
    const limitOk = (n: unknown): n is number => Number.isInteger(n) && (n as number) >= 1 && (n as number) <= 100;

    const visible = (masterId: number, scope: Scope) =>
        scope === null || serverIdsOf(masterId).every(id => scope.includes(id));

    // Живой (не удаляемый) ключ в пределах области; чужой — как несуществующий.
    function masterFor(id: number, scope: Scope): MasterRow {
        const m = q.masterById.get(id) as MasterRow | undefined;
        return m && visible(m.id, scope) ? m : fail(404, "Не найден");
    }

    // Устройство — только вместе со своим мастер-ключом (у внешнего API пути вложенные).
    function deviceFor(id: number, master?: MasterRow): DeviceRow {
        const d = q.deviceById.get(id) as DeviceRow | undefined;
        return d && (!master || d.master_id === master.id) ? d : fail(404, "Не найдено");
    }

    // Набор серверов из тела запроса: непустой, без повторов, только существующие и — для
    // внешнего API — только из области его ключа.
    function parseServers(v: unknown, scope: Scope): number[] {
        if (!Array.isArray(v) || v.length === 0) fail(400, "Нужен хотя бы один сервер");
        const list = v as unknown[];
        if (list.length > MAX_SERVERS) fail(400, `Не больше ${MAX_SERVERS} серверов`);
        const ids = [...new Set(list)];
        if (!ids.every(id => Number.isInteger(id) && servers.exists(id as number))) fail(400, "Такого сервера нет");
        if (scope && !ids.every(id => scope.includes(id as number))) fail(403, "Сервер вне области API-ключа");
        return (ids as number[]).sort((a, b) => a - b);
    }

    // deleting — ключ удалён, но эти серверы ещё не подтвердили, что сняли его пиров.
    function masterOut(m: MasterRow & { devices: number }) {
        return {
            id: m.id, uuid: m.uuid, label: m.label, device_limit: m.device_limit,
            devices: m.devices, servers: serverIdsOf(m.id), created_at: m.created_at,
            ...(m.revoked_at !== null ? { deleting: serverIdsOf(m.id) } : {}),
        };
    }
    const masterOutById = (id: number) => masterOut(q.masterRow.get(id) as MasterRow & { devices: number });

    const listMasters = (scope: Scope) =>
        (q.masters.all() as (MasterRow & { devices: number })[]).filter(m => visible(m.id, scope)).map(masterOut);

    // Серверы по умолчанию: у панели — сервер по умолчанию, у API-ключа — вся его область.
    function createMaster(body: unknown, scope: Scope) {
        const { label, device_limit, servers: wanted } = (body ?? {}) as { label?: unknown; device_limit?: unknown; servers?: unknown };
        if (typeof label !== "string" || !LABEL.test(label)) fail(400, "Метка: до 40 символов, без переводов строк и <>");
        const limit = device_limit === undefined ? DEFAULT_LIMIT : device_limit;
        if (!limitOk(limit)) fail(400, "Лимит устройств: от 1 до 100");
        let ids: number[];
        try { ids = parseServers(wanted ?? scope ?? [servers.defaultId()], scope); }
        catch (e) { throw wanted === undefined && scope === null ? new ApiError(400, "Сначала добавьте сервер") : e; }
        let id = 0;
        uidb.transaction(() => {
            id = Number(q.masterInsert.run(crypto.randomUUID(), label as string, crypto.randomBytes(16), limit as number, ids[0], now()).lastInsertRowid);
            for (const sid of ids) q.serverAdd.run(id, sid);
        })();
        return masterOutById(id);
    }

    async function updateMaster(m: MasterRow, body: unknown, scope: Scope) {
        const { label, device_limit, servers: wanted } = (body ?? {}) as { label?: unknown; device_limit?: unknown; servers?: unknown };
        const nl = label ?? m.label, nd = device_limit ?? m.device_limit;
        if (typeof nl !== "string" || !LABEL.test(nl)) fail(400, "Неверная метка");
        if (!limitOk(nd)) fail(400, "Лимит устройств: от 1 до 100");

        let ids = serverIdsOf(m.id);
        const affected: number[] = [];
        if (wanted !== undefined) {
            const next = parseServers(wanted, scope);
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
        q.masterUpdate.run(nl as string, nd as number, m.id);
        // Лимит стал меньше, чем устройств: лишние отвязываются, начиная с подключившихся последними, —
        // первые, кто занял места, их сохраняют. Как отзыв в панели: пиры снимаются, устройство получит 410.
        const unbound: number[] = [];
        const devices = q.devicesOf.all(m.id) as DeviceRow[];
        if (devices.length > (nd as number)) {
            const newest = [...devices].sort((a, b) => b.created_at - a.created_at || b.id - a.id);
            for (const d of newest.slice(0, devices.length - (nd as number))) {
                affected.push(...forgetDevice(d));
                unbound.push(d.id);
            }
        }
        await reconcileAll(affected);
        return { id: m.id, label: nl as string, device_limit: nd as number, servers: ids, unbound };
    }

    // Новая ссылка; устройства продолжают работать: у них auth_pub, а не secret.
    const rotateMaster = (m: MasterRow) => { q.masterSecret.run(crypto.randomBytes(16), m.id); };
    const rekeyMaster  = (m: MasterRow) => { q.deviceFlag.run(-1, m.id); };

    // Ключ сразу перестаёт работать (ссылка, регистрация, устройства), но строка остаётся
    // отозванной, пока каждый его сервер не подтвердит, что снял пиров, — иначе панель
    // сказала бы «удалён», пока на офлайн-ноде устройства ещё подключаются.
    async function deleteMaster(m: MasterRow) {
        const affected = serverIdsOf(m.id);
        uidb.transaction(() => {
            for (const d of q.devicesOf.all(m.id) as DeviceRow[]) affected.push(...forgetDevice(d));
            q.masterRevoke.run(now(), m.id);
        })();
        await reconcileAll(affected);
        return { success: true, id: m.id, pending: serverIdsOf(m.id) };
    }

    // Выдать ссылку можно, только если подписка настроена и известен адрес панели.
    function linkProblem(req: Request): string | null {
        if (!signKey || !signPub || !SUB_PORT) return "Подписка не настроена на сервере";
        if (!subHost(req)) return "Не удалось определить адрес панели — задайте SUB_HOST";
        return null;
    }

    function linkOf(m: MasterRow, req: Request) {
        const problem = linkProblem(req);
        if (problem) fail(503, problem);
        const link = encodeSenLink({
            tls: tlsPin !== null,
            addrs: [{ host: subHost(req), port: SUB_PORT }],
            secret: m.secret, signPub: signPub as Buffer, tlsPin: tlsPin ?? undefined, name: m.label,
        });
        return { link, tls: tlsPin !== null };
    }

    // Статистика устройства — по всем серверам ключа: онлайн, если есть свежий handshake хоть
    // на одном; трафик суммируется. servers_ok — на скольких серверах уже стоит его текущий ключ.
    async function masterDevices(m: MasterRow) {
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
        return (q.devicesOf.all(m.id) as DeviceRow[]).map(d => {
            const links = q.linksOf.all(d.id) as LinkRow[];
            const s = stats.get(d.pub_key);
            return {
                id: d.id, device_id: d.device_id, device_name: d.device_name, platform: d.platform, version: d.version, created_at: d.created_at,
                last_seen: d.last_seen, rekey_requested: d.rekey_requested === 1,
                online: s?.online ?? false, lastHandshake: s?.lastHandshake ?? 0, rx: s?.rx ?? 0, tx: s?.tx ?? 0,
                servers_total: links.length, servers_ok: links.filter(l => l.peer_pub === d.pub_key && l.ip).length,
            };
        });
    }

    async function deleteDevice(d: DeviceRow) {
        await reconcileAll(forgetDevice(d));
        return { success: true, id: d.id };
    }
    const rekeyDevice = (d: DeviceRow) => { q.deviceFlag.run(d.id, -1); };

    // Новый PSK на каждом сервере; недоступные получат его, когда появятся.
    async function rotatePsk(d: DeviceRow) {
        q.linkPskDue.run(d.id);
        await reconcileAll((q.linksOf.all(d.id) as LinkRow[]).map(l => l.server_id));
        return { id: d.id, pending: (q.linksOf.all(d.id) as LinkRow[]).filter(l => l.psk_pending === 1).length };
    }

    // ── Роуты панели (монтируются в server.ts за JWT) ──────────────────────
    const masterKeys = Router();
    masterKeys.get("/", (_req, res) => {
        res.json({ enabled: signKey !== null, tls: tlsPin !== null, keys: listMasters(null) });
    });
    masterKeys.post("/", aw((req, res) => { res.status(201).json(createMaster(req.body, null)); }));
    masterKeys.patch("/:id", aw(async (req, res) => { res.json(await updateMaster(masterFor(masterIdOf(req.params.id), null), req.body, null)); }));
    masterKeys.post("/:id/rotate", aw((req, res) => { const m = masterFor(masterIdOf(req.params.id), null); rotateMaster(m); res.json({ id: m.id }); }));
    masterKeys.post("/:id/rekey", aw((req, res) => { const m = masterFor(masterIdOf(req.params.id), null); rekeyMaster(m); res.json({ id: m.id }); }));
    masterKeys.delete("/:id", aw(async (req, res) => { res.json(await deleteMaster(masterFor(masterIdOf(req.params.id), null))); }));
    masterKeys.get("/:id/link", aw((req, res) => { res.json(linkOf(masterFor(masterIdOf(req.params.id), null), req)); }));
    masterKeys.get("/:id/devices", aw(async (req, res) => { res.json({ devices: await masterDevices(masterFor(masterIdOf(req.params.id), null)) }); }));

    const devices = Router();
    devices.delete("/:id", aw(async (req, res) => { res.json(await deleteDevice(deviceFor(idOf(req.params.id)))); }));
    devices.post("/:id/rekey", aw((req, res) => { const d = deviceFor(idOf(req.params.id)); rekeyDevice(d); res.json({ id: d.id }); }));
    devices.post("/:id/psk", aw(async (req, res) => { res.json(await rotatePsk(deviceFor(idOf(req.params.id)))); }));

    // ── Внешний API: /api/v1/masterkeys (ключ awgk_, монтируется в server.ts) ─
    // scopeOf — набор серверов API-ключа запроса. Устройства — вложенными путями, чтобы
    // область проверялась через их мастер-ключ.
    function apiRouter(scopeOf: (req: Request) => number[]) {
        const r = Router();
        const master = (req: Request) => masterFor(masterIdOf(req.params.id), scopeOf(req));
        const device = (req: Request) => deviceFor(idOf(req.params.device), master(req));

        r.get("/", aw((req, res) => { res.json({ keys: listMasters(scopeOf(req)) }); }));
        r.post("/", aw((req, res) => {
            const problem = linkProblem(req);           // без ссылки ключ внешней программе бесполезен
            if (problem) fail(503, problem);
            const out = createMaster(req.body, scopeOf(req));
            res.status(201).json({ ...out, ...linkOf(masterFor(out.id, null), req) });
        }));
        r.get("/:id", aw(async (req, res) => {
            const m = master(req);
            // devices — счётчик, как в списке; сами устройства — в device_list.
            res.json({ ...masterOutById(m.id), ...(linkProblem(req) ? { link: null, tls: tlsPin !== null } : linkOf(m, req)), device_list: await masterDevices(m) });
        }));
        r.patch("/:id", aw(async (req, res) => { res.json(await updateMaster(master(req), req.body, scopeOf(req))); }));
        r.post("/:id/rotate", aw((req, res) => {
            const m = master(req);
            rotateMaster(m);
            res.json({ id: m.id, ...linkOf(masterFor(m.id, null), req) });
        }));
        r.post("/:id/rekey", aw((req, res) => { const m = master(req); rekeyMaster(m); res.json({ id: m.id }); }));
        r.delete("/:id", aw(async (req, res) => { res.json(await deleteMaster(master(req))); }));
        r.get("/:id/devices", aw(async (req, res) => { res.json({ devices: await masterDevices(master(req)) }); }));
        r.delete("/:id/devices/:device", aw(async (req, res) => { res.json(await deleteDevice(device(req))); }));
        r.post("/:id/devices/:device/rekey", aw((req, res) => { const d = device(req); rekeyDevice(d); res.json({ id: d.id }); }));
        r.post("/:id/devices/:device/psk", aw(async (req, res) => { res.json(await rotatePsk(device(req))); }));
        return r;
    }

    // ── Листенер и фоновое сведение ────────────────────────────────────────
    servers.onOnline(id => { void reconcile(id); });

    // Сервер удаляют из реестра: пока им пользуется живой мастер-ключ — нельзя. Иначе сперва
    // сводим его (ключи его не используют, значит, список пуст и пиры панели снимутся) — после
    // удаления до него уже не дотянуться. Не вышло (нода не в сети) — удалять только с force;
    // тогда отозванные ключи перестают его ждать уже после удаления (afterDelete).
    servers.beforeDelete(async (id, force) => {
        const live = (q.liveOnServer.all(id) as { label: string }[]).map(r => r.label);
        if (live.length) return { error: `Сервер в мастер-ключах: ${live.join(", ")} — сначала уберите его оттуда`, forcible: false };
        if (servers.isOnline(id)) await reconcile(id);
        const waiting = (q.revokedOnServer.all(id) as { label: string }[]).map(r => r.label);
        const tombs = (q.tombCount.get(id) as { n: number }).n;
        if (!waiting.length && !tombs || force) return null;
        return {
            error: "Сервер не подтвердил, что снял пиров" + (waiting.length ? ` удалённых мастер-ключей (${waiting.join(", ")})` : "") +
                ". Дождитесь, пока он выйдет на связь, или удалите принудительно — тогда пиры могут остаться на нём (снять: awg-ctrl peers drop)",
            forcible: true,
        };
    });
    servers.afterDelete(id => {
        q.serverRelease.run(id);
        q.revokedGone.run();
    });

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

    return { masterKeys, devices, apiRouter, start };
}
