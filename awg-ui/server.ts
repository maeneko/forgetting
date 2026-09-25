// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import express, { Request, Response, NextFunction } from "express";
import path     from "path";
import fs       from "fs";
import crypto   from "crypto";
import axios    from "axios";
import Database from "better-sqlite3";
import { createSub } from "./sub";

const app  = express();
const PORT = Number(process.env.PORT) || 8080;

const UI_USER    = process.env.UI_USER    ?? "admin";
const UI_PASS    = process.env.UI_PASS    ?? "";
const JWT_SECRET = process.env.JWT_SECRET ?? crypto.randomBytes(32).toString("hex");
const CTRL       = `http://127.0.0.1:${process.env.AWGCTRL_PORT ?? "3005"}`;

// Имя панели, канал и версия. В проде их прокидывает CLI (он читает .env в
// корне проекта и cli.env); при самостоятельном запуске `npm run server`
// читаем тот же .env сами, чтобы дев-режим показывал то же, что прод.
function envFromFile(file: string, key: string): string {
    try {
        const m = fs.readFileSync(file, "utf8").match(new RegExp(`^\\s*${key}\\s*=\\s*(.*)$`, "m"));
        return m ? m[1].trim() : "";
    } catch { return ""; }
}
const ROOT_ENV = path.join(__dirname, "..", ".env");
const BRAND    = process.env.BRAND   || envFromFile(ROOT_ENV, "BRAND")   || "Forgetting";
const CHANNEL  = process.env.CHANNEL || envFromFile(ROOT_ENV, "CHANNEL") || "Beta";
const VERSION  = process.env.VERSION || envFromFile(ROOT_ENV, "VERSION") || "";

const INTERNAL_AUTH_KEY_FILE = process.env.INTERNAL_AUTH_KEY_FILE
    ?? path.join(__dirname, "internal_auth_private.key");
let internalAuthKey: crypto.KeyObject | null = null;
try {
    internalAuthKey = crypto.createPrivateKey(fs.readFileSync(INTERNAL_AUTH_KEY_FILE));
} catch {
    console.warn("ui: приватный ключ внутренней авторизации не найден: " + INTERNAL_AUTH_KEY_FILE
        + " — запросы к awg-ctrl будут отклоняться");
}

function mintInternalToken(): string {
    if (!internalAuthKey) throw new Error("приватный ключ внутренней авторизации не загружен");
    const now = Math.floor(Date.now() / 1000);
    const h = Buffer.from('{"alg":"EdDSA","typ":"JWT"}').toString("base64url");
    const p = Buffer.from(JSON.stringify({ iss: "awg-ui", iat: now, exp: now + 60 })).toString("base64url");
    const sig = crypto.sign(null, Buffer.from(`${h}.${p}`), internalAuthKey).toString("base64url");
    return `${h}.${p}.${sig}`;
}

const revoked = new Set<string>();

// ── Ключи внешнего API (/api/v1) ───────────────────────────────────────────
// Живут в собственной БД awg-ui: awg-ctrl про них ничего не знает. Открытое
// значение ключа не хранится — только SHA-256 и префикс для показа в панели.
interface ApiKeyRow {
    id: number; label: string; key_hash: string; prefix: string;
    server_id: number; created_at: number; last_used: number | null;
}

const UI_DB_FILE = process.env.UI_DB_FILE || path.join(__dirname, "ui.db");
const uidb = new Database(UI_DB_FILE);
uidb.pragma("journal_mode = WAL");
uidb.exec(`
    CREATE TABLE IF NOT EXISTS api_keys (
        id         INTEGER PRIMARY KEY AUTOINCREMENT,
        label      TEXT    NOT NULL,
        key_hash   TEXT    NOT NULL UNIQUE,
        prefix     TEXT    NOT NULL,
        server_id  INTEGER NOT NULL DEFAULT 0,
        created_at INTEGER NOT NULL,
        last_used  INTEGER
    )
`);

const keyStmts = {
    list:   uidb.prepare("SELECT id, label, prefix, server_id, created_at, last_used FROM api_keys ORDER BY id"),
    insert: uidb.prepare("INSERT INTO api_keys (label, key_hash, prefix, server_id, created_at) VALUES (?, ?, ?, ?, ?)"),
    delete: uidb.prepare<[number]>("DELETE FROM api_keys WHERE id = ?"),
    byHash: uidb.prepare<[string]>("SELECT * FROM api_keys WHERE key_hash = ?"),
    touch:  uidb.prepare<[number, number]>("UPDATE api_keys SET last_used = ? WHERE id = ?"),
};

function hashKey(key: string): string {
    return crypto.createHash("sha256").update(key).digest("hex");
}

function genApiKey(): { key: string; hash: string; prefix: string } {
    const key = "awgk_" + crypto.randomBytes(24).toString("base64url");
    return { key, hash: hashKey(key), prefix: key.slice(0, 13) + "…" };
}

function tokenSig(token: string): string { return token.split(".")[2] ?? token; }

function sign(): string {
    const h = Buffer.from('{"alg":"HS256","typ":"JWT"}').toString("base64url");
    const p = Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 86400 })).toString("base64url");
    const s = crypto.createHmac("sha256", JWT_SECRET).update(`${h}.${p}`).digest("base64url");
    return `${h}.${p}.${s}`;
}

function verify(token: string): boolean {
    try {
        if (revoked.has(tokenSig(token))) return false;
        const [h, p, s] = token.split(".");
        if (!h || !p || !s) return false;
        const expected = crypto.createHmac("sha256", JWT_SECRET).update(`${h}.${p}`).digest();
        const actual   = Buffer.from(s, "base64url");
        if (actual.length !== expected.length) return false;
        if (!crypto.timingSafeEqual(actual, expected)) return false;
        const { exp } = JSON.parse(Buffer.from(p, "base64url").toString()) as { exp: number };
        return exp > Math.floor(Date.now() / 1000);
    } catch { return false; }
}

function requireAuth(req: Request, res: Response, next: NextFunction) {
    const auth  = (req.headers["authorization"] ?? "") as string;
    const token = auth.startsWith("Bearer ") ? auth.slice(7) : "";
    if (!token || !verify(token)) { res.status(401).json({ error: "Unauthorized" }); return; }
    next();
}

const RATE_LIMIT  = 5;
const RATE_WINDOW = 15 * 60 * 1000;

interface RateEntry { count: number; resetAt: number; }
const loginAttempts = new Map<string, RateEntry>();

function checkRate(ip: string): { ok: boolean; retryAfter?: number } {
    const now   = Date.now();
    const entry = loginAttempts.get(ip);
    if (!entry || entry.resetAt < now) {
        loginAttempts.set(ip, { count: 1, resetAt: now + RATE_WINDOW });
        return { ok: true };
    }
    if (entry.count >= RATE_LIMIT) {
        return { ok: false, retryAfter: Math.ceil((entry.resetAt - now) / 1000) };
    }
    entry.count++;
    return { ok: true };
}

app.use(express.json());

app.post("/login", (req: Request, res: Response) => {
    const ip = req.socket.remoteAddress ?? "unknown";
    const { ok, retryAfter } = checkRate(ip);
    if (!ok) {
        res.status(429).json({ error: `Слишком много попыток. Повтори через ${retryAfter} сек.` }); return;
    }
    const { user, pass } = req.body as { user?: string; pass?: string };
    if (!UI_PASS || user !== UI_USER || pass !== UI_PASS) {
        res.status(401).json({ error: "Неверный логин или пароль" }); return;
    }
    loginAttempts.delete(ip);
    res.json({ token: sign() });
});

app.post("/logout", requireAuth, (req: Request, res: Response) => {
    const token = (req.headers["authorization"] as string).slice(7);
    revoked.add(tokenSig(token));
    res.json({ ok: true });
});

// Единственный роут без авторизации: вордмарк нужен уже на экране логина,
// до выдачи JWT. Наружу уходит только имя продукта и версия.
app.get("/ui/brand", (_req: Request, res: Response) => {
    res.json({ brand: BRAND, channel: CHANNEL, version: VERSION });
});

// Управление ключами внешнего API — роуты самой awg-ui, в awg-ctrl не идут.
app.get("/ui/apikeys", requireAuth, (_req: Request, res: Response) => {
    res.json({ keys: keyStmts.list.all() });
});

app.post("/ui/apikeys", requireAuth, (req: Request, res: Response) => {
    const { label, server_id } = req.body as { label?: string; server_id?: number };
    if (!label || !/^[\w \-]{1,40}$/.test(label)) {
        res.status(400).json({ error: "Метка: буквы, цифры, пробел, _ и -, до 40 символов" }); return;
    }
    // TODO multi-server: server_id пока всегда 0 (текущий сервер). Когда появится
    // список серверов — валидировать его против реального реестра серверов.
    const { key, hash, prefix } = genApiKey();
    const info = keyStmts.insert.run(label, hash, prefix, Number(server_id) || 0, Math.floor(Date.now() / 1000));
    // Открытое значение отдаётся один раз — дальше в БД только хэш.
    res.status(201).json({ id: info.lastInsertRowid, label, prefix, key });
});

app.delete("/ui/apikeys/:id", requireAuth, (req: Request, res: Response) => {
    const id = Number(req.params.id);
    if (!Number.isInteger(id)) { res.status(400).json({ error: "Неверный id" }); return; }
    keyStmts.delete.run(id);
    res.json({ success: true, id });
});

async function proxy(req: Request, res: Response) {
    try {
        const r = await axios({
            method: req.method,
            url:    CTRL + req.originalUrl,
            headers: { Authorization: `Bearer ${mintInternalToken()}`, "Content-Type": "application/json" },
            data:   req.body,
            validateStatus: () => true,
        });
        res.status(r.status).json(r.data);
    } catch {
        res.status(502).json({ error: "awg-ctrl недоступен" });
    }
}

// TODO multi-server: ключ привязан к server_id (пока всегда 0 = текущий
// сервер). Когда серверов станет несколько — маршрутизировать ctrl() на нужный
// awg-ctrl по (req as ExtRequest).apiKey.server_id.

interface ExtRequest extends Request { apiKey?: ApiKeyRow; }

function requireApiKey(req: Request, res: Response, next: NextFunction) {
    const key = (req.headers["x-api-key"] as string) ?? "";
    if (!key.startsWith("awgk_")) { res.status(401).json({ error: "API key required" }); return; }
    const row = keyStmts.byHash.get(hashKey(key)) as ApiKeyRow | undefined;
    if (!row) { res.status(401).json({ error: "Invalid API key" }); return; }
    keyStmts.touch.run(Math.floor(Date.now() / 1000), row.id);
    (req as ExtRequest).apiKey = row;
    next();
}

async function ctrl(method: string, urlPath: string, body?: unknown) {
    return axios({
        method,
        url:     CTRL + urlPath,
        headers: { Authorization: `Bearer ${mintInternalToken()}`, "Content-Type": "application/json" },
        data:    body,
        validateStatus: () => true,
    });
}

// Внешний контракт: наружу отдаём только name/ip/gen/vpn_key — psk_key и pub_key
// остаются внутри. gen — поколение AmneziaWG, на параметрах которого выдан ключ.
interface ExtUser { name: string; ip: string; vpn_key: string; key_gen: string }

const ext = express.Router();
ext.use(requireApiKey);

// Express 4 не ловит отказы async-обработчиков: незакрытый reject (awg-ctrl не
// отвечает, внутренний ключ не загружен) уронил бы весь процесс панели. Ответ
// тот же, что у proxy() в такой ситуации.
type Handler = (req: Request, res: Response) => Promise<void>;
const wrap = (fn: Handler) => (req: Request, res: Response) => {
    fn(req, res).catch(() => { res.status(502).json({ error: "awg-ctrl недоступен" }); });
};

ext.post("/users", wrap(async (req, res) => {
    const { name } = (req.body ?? {}) as { name?: string };
    const r = await ctrl("POST", "/api/users", { name });
    if (r.status >= 400) { res.status(r.status).json(r.data); return; }
    const u = r.data as ExtUser;
    res.status(201).json({ name: u.name, ip: u.ip, gen: u.key_gen, vpn_key: u.vpn_key });
}));

ext.get("/users", wrap(async (_req, res) => {
    const r = await ctrl("GET", "/api/users/stats");
    res.status(r.status).json(r.data);
}));

ext.get("/users/:name", wrap(async (req, res) => {
    const r = await ctrl("POST", `/api/users/${encodeURIComponent(req.params.name)}`);
    if (r.status >= 400) { res.status(r.status).json(r.data); return; }
    const u = r.data as ExtUser;
    res.json({ name: u.name, ip: u.ip, gen: u.key_gen, vpn_key: u.vpn_key });
}));

ext.delete("/users/:name", wrap(async (req, res) => {
    const r = await ctrl("DELETE", `/api/users/${encodeURIComponent(req.params.name)}`);
    res.status(r.status).json(r.data);
}));

// ВАЖНО: /api/v1 монтируется ДО JWT-прокси на /api — иначе прокси перехватит
// его и потребует JWT вместо ключа.
app.use("/api/v1", ext);

// sen://-подписка: мастер-ключи и устройства (роуты панели, за JWT) и публичный
// листенер /sub/v1 на своём порту SUB_PORT. Публичная часть сюда не монтируется.
const sub = createSub({ uidb, ctrl, baseDir: __dirname });
app.use("/ui/masterkeys", requireAuth, sub.masterKeys);
app.use("/ui/devices",    requireAuth, sub.devices);

app.use(["/api", "/health", "/awg"], requireAuth, proxy);

app.use(express.static(path.join(__dirname, "dist")));

app.get("*", (_req: Request, res: Response) => {
    res.sendFile(path.join(__dirname, "dist", "index.html"));
});

app.listen(PORT, () => { console.log(`ui: listening on :${PORT}`); });
sub.start();