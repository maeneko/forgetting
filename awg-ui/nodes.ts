// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Несколько серверов под одной панелью: core (эта awg-ui) и nodes — VPN-серверы,
// которые сами открывают к core исходящее WSS-соединение (awg-agent). Реестр нод
// лежит в ui.db, RPC к ноде идёт по этому соединению, а нода исполняет запрос
// на своём 127.0.0.1-awg-ctrl. awg-ctrl про ноды ничего не знает.
//
// server_id 0 — локальный awg-ctrl этой же машины (как было до нод); при
// LOCAL_NODE=off его нет, core работает как чистая панель. Протокол и формат
// join-строки — docs/node-protocol.md.
import crypto from "crypto";
import fs from "fs";
import https from "https";
import path from "path";
import { spawnSync } from "child_process";
import { Router, type Request, type Response } from "express";
import type Database from "better-sqlite3";
import { WebSocketServer, WebSocket } from "ws";
import { spkiPin, publicKeyFromRaw } from "./sen";

export type Ctrl = (method: string, urlPath: string, body?: unknown) => Promise<{ status: number; data: any }>;

interface NodeRow {
    id: number; name: string; pub: string | null;
    join_hash: Buffer | null; join_expires: number | null;
    created_at: number; last_seen: number | null;
}

interface Health {
    server: string; ip: string; gen: string; peers: number;
    awg_up: boolean; module: string; tools: string;
}

interface Conn {
    ws: WebSocket; seq: number; alive: boolean;
    pending: Map<number, { resolve: (r: { status: number; data: any }) => void; timer: NodeJS.Timeout }>;
}

const JOIN_TTL      = 3600;          // секунд жизни одноразового секрета
const AUTH_TIMEOUT  = 10_000;        // на challenge–response после открытия сокета
const HEALTH_EVERY  = 15_000;
const PING_EVERY    = 20_000;
const RPC_TIMEOUT   = 15_000;
const RPC_SLOW      = 120_000;       // /awg/*: рестарт и upgrade идут долго
const NAME_RE       = /^[^\r\n<>]{1,40}$/;
const HOST_RE       = /^[A-Za-z0-9.:_-]{1,253}$/;

// Сообщение, которое нода подписывает своим ключом. Дублируется в awg-agent.
const authMsg = (nonce: string, nodeId: number) => Buffer.from(`awg-node-v1\n${nonce}\n${nodeId}`);

export function createNodes(deps: { uidb: Database.Database; ctrlLocal: Ctrl; baseDir: string }) {
    const { uidb, ctrlLocal } = deps;

    const NODE_PORT = Number(process.env.NODE_PORT) || 0;
    const LOCAL     = (process.env.LOCAL_NODE ?? "on").toLowerCase() !== "off";
    const file = (env: string | undefined, name: string) => env || path.join(deps.baseDir, name);

    // ── TLS: самоподписанный сертификат, pin которого зашит в join-строки ────
    // Живёт столько же, сколько core: пересоздать = переподключить все ноды.
    let tls: { key: Buffer; cert: Buffer } | null = null;
    let pin: Buffer | null = null;
    if (NODE_PORT) {
        const keyF = file(process.env.NODE_TLS_KEY_FILE,  "node_tls.key");
        const crtF = file(process.env.NODE_TLS_CERT_FILE, "node_tls.crt");
        try {
            if (!fs.existsSync(keyF) && !fs.existsSync(crtF)) {
                const r = spawnSync("openssl", [
                    "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
                    "-keyout", keyF, "-out", crtF, "-days", "3650", "-subj", "/CN=awg-node",
                ], { stdio: "ignore" });
                if (r.status !== 0) throw new Error("openssl");
                fs.chmodSync(keyF, 0o600); fs.chmodSync(crtF, 0o600);
                console.log("nodes: создан самоподписанный сертификат " + crtF);
            }
            tls = { key: fs.readFileSync(keyF), cert: fs.readFileSync(crtF) };
            pin = spkiPin(tls.cert.toString());
        } catch {
            console.warn("nodes: сертификат хаба недоступен (" + crtF + ") — приём нод отключён");
            tls = null;
        }
    }

    // ── Схема ──────────────────────────────────────────────────────────────
    uidb.exec(`
        CREATE TABLE IF NOT EXISTS nodes (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            name         TEXT    NOT NULL,
            pub          TEXT,
            join_hash    BLOB,
            join_expires INTEGER,
            created_at   INTEGER NOT NULL,
            last_seen    INTEGER
        );
    `);
    const q = {
        all:    uidb.prepare("SELECT * FROM nodes ORDER BY id"),
        byId:   uidb.prepare<[number]>("SELECT * FROM nodes WHERE id = ?"),
        insert: uidb.prepare<[string, Buffer, number, number]>("INSERT INTO nodes (name, join_hash, join_expires, created_at) VALUES (?, ?, ?, ?)"),
        rename: uidb.prepare<[string, number]>("UPDATE nodes SET name = ? WHERE id = ?"),
        rejoin: uidb.prepare<[Buffer, number, number]>("UPDATE nodes SET join_hash = ?, join_expires = ? WHERE id = ? AND pub IS NULL"),
        bind:   uidb.prepare<[string, number]>("UPDATE nodes SET pub = ?, join_hash = NULL, join_expires = NULL WHERE id = ?"),
        seen:   uidb.prepare<[number, number]>("UPDATE nodes SET last_seen = ? WHERE id = ?"),
        delete: uidb.prepare<[number]>("DELETE FROM nodes WHERE id = ?"),
    };
    const now = () => Math.floor(Date.now() / 1000);
    const sha = (b: Buffer) => crypto.createHash("sha256").update(b).digest();

    // ── RPC к нодам ────────────────────────────────────────────────────────
    const conns  = new Map<number, Conn>();
    const health = new Map<number, Health>();
    const OFFLINE = { status: 502, data: { error: "Сервер недоступен" } };

    function rpc(id: number, method: string, urlPath: string, body?: unknown): Promise<{ status: number; data: any }> {
        const c = conns.get(id);
        if (!c || c.ws.readyState !== WebSocket.OPEN) return Promise.resolve(OFFLINE);
        return new Promise(resolve => {
            const rid = ++c.seq;
            const timer = setTimeout(() => {
                c.pending.delete(rid);
                resolve({ status: 504, data: { error: "Сервер не ответил" } });
            }, urlPath.startsWith("/awg/") ? RPC_SLOW : RPC_TIMEOUT);
            c.pending.set(rid, { resolve, timer });
            c.ws.send(JSON.stringify({ t: "req", id: rid, method, path: urlPath, body }));
        });
    }

    /** Контроллер нужного сервера. 0 — локальный awg-ctrl, остальное — нода по WSS. */
    function ctrlFor(serverId: number): Ctrl {
        if (serverId === 0) {
            if (!LOCAL) return async () => ({ status: 404, data: { error: "Локальный сервер не настроен" } });
            return ctrlLocal;
        }
        if (!q.byId.get(serverId)) return async () => ({ status: 404, data: { error: "Сервер не найден" } });
        return (m, p, b) => rpc(serverId, m, p, b);
    }

    const exists = (id: number) => (id === 0 ? LOCAL : q.byId.get(id) !== undefined);

    /** Сервер по умолчанию: локальный, а без него — первая нода. */
    function defaultId(): number {
        if (LOCAL) return 0;
        return (q.all.all() as NodeRow[])[0]?.id ?? 0;
    }

    async function refreshHealth(id: number) {
        const r = await (id === 0 ? ctrlLocal("GET", "/health") : rpc(id, "GET", "/health")).catch(() => null);
        const d = r?.data;
        if (d && typeof d === "object" && typeof d.server === "string") {
            health.set(id, {
                server: d.server, ip: d.ip ?? "", gen: d.gen ?? "2", peers: d.awg?.peers ?? 0,
                awg_up: d.awg?.status === "ok", module: d.awg?.module ?? "", tools: d.awg?.tools ?? "",
            });
        } else health.delete(id);
    }

    // ── Хаб: приём соединений от нод ───────────────────────────────────────
    const failHits = new Map<string, { n: number; reset: number }>();
    const failed = (ip: string) => {
        const t = Date.now(), e = failHits.get(ip);
        if (!e || e.reset < t) failHits.set(ip, { n: 1, reset: t + 15 * 60_000 });
        else e.n++;
    };
    const blocked = (ip: string) => {
        const e = failHits.get(ip);
        return !!e && e.reset > Date.now() && e.n >= 20;
    };

    function dropConn(id: number, c: Conn) {
        for (const p of c.pending.values()) { clearTimeout(p.timer); p.resolve(OFFLINE); }
        c.pending.clear();
        if (conns.get(id) === c) { conns.delete(id); health.delete(id); }
    }

    function onConnection(ws: WebSocket, req: { socket: { remoteAddress?: string } }) {
        const ip = req.socket.remoteAddress ?? "unknown";
        if (blocked(ip)) { ws.close(4029, "rate_limited"); return; }

        const nonce = crypto.randomBytes(16).toString("base64url");
        let conn: Conn | null = null;
        let nodeId = 0;
        const authTimer = setTimeout(() => ws.close(4001, "auth_timeout"), AUTH_TIMEOUT);
        const deny = () => { failed(ip); ws.close(4003, "unauthorized"); };

        function authenticate(m: any) {
            if (m?.t !== "auth" || !Number.isInteger(m.node_id) || typeof m.sig !== "string") return deny();
            const row = q.byId.get(m.node_id) as NodeRow | undefined;
            if (!row) return deny();

            let pubB64: string | null = row.pub;
            const joining = pubB64 === null;
            if (joining) {
                const j = m.join;
                if (!row.join_hash || !row.join_expires || row.join_expires < now()
                    || typeof j?.secret !== "string" || typeof j?.pub !== "string") return deny();
                const h = sha(Buffer.from(j.secret, "base64url"));
                if (h.length !== row.join_hash.length || !crypto.timingSafeEqual(h, row.join_hash)) return deny();
                pubB64 = j.pub;
            }
            try {
                const key = publicKeyFromRaw(Buffer.from(pubB64 as string, "base64url"));
                if (!crypto.verify(null, authMsg(nonce, row.id), key, Buffer.from(m.sig, "base64url"))) return deny();
            } catch { return deny(); }

            clearTimeout(authTimer);
            if (joining) q.bind.run(pubB64 as string, row.id);
            q.seen.run(now(), row.id);

            const old = conns.get(row.id);
            conn = { ws, seq: 0, alive: true, pending: new Map() };
            conns.set(row.id, conn);
            nodeId = row.id;
            if (old) { dropConn(row.id, old); conns.set(row.id, conn); old.ws.close(4000, "replaced"); }

            ws.send(JSON.stringify({ t: "ready" }));
            console.log(`nodes: «${row.name}» (#${row.id}) подключена${joining ? " (первое подключение)" : ""}`);
            void refreshHealth(row.id);
        }

        ws.on("message", raw => {
            let m: any;
            try { m = JSON.parse(raw.toString()); } catch { ws.close(4002, "bad_frame"); return; }
            if (!conn) { authenticate(m); return; }
            if (m?.t === "res" && Number.isInteger(m.id)) {
                const p = conn.pending.get(m.id);
                if (!p) return;
                conn.pending.delete(m.id);
                clearTimeout(p.timer);
                p.resolve({ status: Number(m.status) || 502, data: m.data });
            }
        });
        ws.on("pong", () => { if (conn) conn.alive = true; });
        ws.on("close", () => {
            clearTimeout(authTimer);
            if (conn) { dropConn(nodeId, conn); console.log(`nodes: #${nodeId} отключена`); }
        });
        ws.on("error", () => { /* close придёт следом */ });

        ws.send(JSON.stringify({ t: "hello", nonce }));
    }

    function start() {
        if (LOCAL) void refreshHealth(0);
        if (!NODE_PORT || !tls) return;
        const server = https.createServer(tls);
        const wss = new WebSocketServer({ server, path: "/node/v1", maxPayload: 4 * 1024 * 1024 });
        wss.on("connection", onConnection);
        server.listen(NODE_PORT, () => console.log(`nodes: hub on :${NODE_PORT} (wss)`));

        setInterval(() => {
            for (const [id, c] of conns) {
                if (!c.alive) { c.ws.terminate(); dropConn(id, c); continue; }
                c.alive = false;
                try { c.ws.ping(); } catch { /* закроется само */ }
            }
        }, PING_EVERY).unref();
        setInterval(() => { for (const id of conns.keys()) void refreshHealth(id); }, HEALTH_EVERY).unref();
    }

    // ── Роуты панели (монтируются в server.ts за JWT) ──────────────────────
    const joinString = (host: string, id: number, secret: Buffer) => "awgjoin://" + Buffer.from(JSON.stringify({
        v: 1, h: host, p: NODE_PORT, pin: (pin as Buffer).toString("base64url"), n: id, s: secret.toString("base64url"),
    })).toString("base64url");

    const hostOf = (req: Request): string => {
        const given = (req.body as { host?: unknown } | undefined)?.host;
        if (typeof given === "string" && HOST_RE.test(given)) return given;
        return (req.headers.host ?? "").replace(/:\d+$/, "").replace(/^\[|\]$/g, "");
    };
    const idOf = (req: Request) => Number(req.params.id);
    const validId = (n: number) => Number.isInteger(n) && n >= 1;
    const hubReady = () => NODE_PORT > 0 && tls !== null && pin !== null;

    const router = Router();

    router.get("/", async (_req: Request, res: Response) => {
        if (LOCAL) await refreshHealth(0);
        const nodes: unknown[] = [];
        if (LOCAL) {
            const h = health.get(0) ?? null;
            nodes.push({ id: 0, name: h?.server ?? "VPN", local: true, online: h !== null, pending: false, last_seen: null, health: h });
        }
        for (const n of q.all.all() as NodeRow[]) {
            nodes.push({
                id: n.id, name: n.name, local: false, online: conns.has(n.id),
                pending: n.pub === null, last_seen: n.last_seen, health: health.get(n.id) ?? null,
            });
        }
        res.json({ hub: hubReady(), local: LOCAL, nodes });
    });

    router.post("/", (req: Request, res: Response) => {
        if (!hubReady()) { res.status(503).json({ error: "Приём нод не настроен (NODE_PORT / сертификат)" }); return; }
        const name = (req.body as { name?: unknown } | undefined)?.name;
        if (typeof name !== "string" || !NAME_RE.test(name)) { res.status(400).json({ error: "Имя: до 40 символов, без переводов строк и <>" }); return; }
        const host = hostOf(req);
        if (!HOST_RE.test(host)) { res.status(400).json({ error: "Не удалось определить адрес core" }); return; }
        const secret = crypto.randomBytes(16);
        const id = Number(q.insert.run(name, sha(secret), now() + JOIN_TTL, now()).lastInsertRowid);
        res.status(201).json({ id, name, join: joinString(host, id, secret), expires_in: JOIN_TTL });
    });

    // Новая join-строка для ноды, которая ещё не подключалась (истёк срок / потеряли).
    router.post("/:id/join", (req: Request, res: Response) => {
        if (!hubReady()) { res.status(503).json({ error: "Приём нод не настроен (NODE_PORT / сертификат)" }); return; }
        const id = idOf(req);
        const n = validId(id) ? q.byId.get(id) as NodeRow | undefined : undefined;
        if (!n) { res.status(404).json({ error: "Не найден" }); return; }
        if (n.pub !== null) { res.status(409).json({ error: "Нода уже подключена" }); return; }
        const host = hostOf(req);
        if (!HOST_RE.test(host)) { res.status(400).json({ error: "Не удалось определить адрес core" }); return; }
        const secret = crypto.randomBytes(16);
        q.rejoin.run(sha(secret), now() + JOIN_TTL, id);
        res.json({ id, name: n.name, join: joinString(host, id, secret), expires_in: JOIN_TTL });
    });

    router.patch("/:id", (req: Request, res: Response) => {
        const id = idOf(req);
        const n = validId(id) ? q.byId.get(id) as NodeRow | undefined : undefined;
        if (!n) { res.status(404).json({ error: "Не найден" }); return; }
        const name = (req.body as { name?: unknown } | undefined)?.name;
        if (typeof name !== "string" || !NAME_RE.test(name)) { res.status(400).json({ error: "Неверное имя" }); return; }
        q.rename.run(name, id);
        res.json({ id, name });
    });

    router.delete("/:id", (req: Request, res: Response) => {
        const id = idOf(req);
        const n = validId(id) ? q.byId.get(id) as NodeRow | undefined : undefined;
        if (!n) { res.status(404).json({ error: "Не найден" }); return; }
        // Ключи API и мастер-ключи ссылаются на server_id — не оставляем их висеть в воздухе.
        const used = (["api_keys", "master_keys"] as const).some(t => {
            try { return (uidb.prepare(`SELECT COUNT(*) AS n FROM ${t} WHERE server_id = ?`).get(id) as { n: number }).n > 0; }
            catch { return false; }
        });
        if (used) { res.status(409).json({ error: "К серверу привязаны ключи API или мастер-ключи — сначала удалите их" }); return; }
        const c = conns.get(id);
        if (c) { dropConn(id, c); c.ws.close(4004, "removed"); }
        q.delete.run(id);
        res.json({ success: true, id });
    });

    return { router, ctrlFor, exists, defaultId, start };
}
