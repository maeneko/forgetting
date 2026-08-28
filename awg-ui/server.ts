// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import express, { Request, Response, NextFunction } from "express";
import path     from "path";
import fs       from "fs";
import crypto   from "crypto";
import axios    from "axios";
import { ma7 } from "./ma7";

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

async function ctrl(method: string, urlPath: string, body?: unknown) {
    return axios({
        method,
        url:     CTRL + urlPath,
        headers: { Authorization: `Bearer ${mintInternalToken()}`, "Content-Type": "application/json" },
        data:    body,
        validateStatus: () => true,
    });
}

// ── Интеграция с MA7 (вкладка «Профиль») ───────────────────────────────────
// Сервер персональный — панель показывает баланс своего единственного
// владельца, не список абонентов. Логин владельца (MA7_LOGIN) — часть
// конфигурации сервера, задаётся один раз при установке (cli.env), а НЕ
// вводится через UI: бот-токен MA7_JWT_SECRET умеет искать любого абонента
// MA7, и если бы логин можно было менять прямо в панели, любой, кто знает
// пароль от панели, мог бы подсматривать баланс чужих абонентов — панель
// защищена одним общим UI_USER/UI_PASS, отдельного «клиентского» входа нет.
const MA7_LOGIN = process.env.MA7_LOGIN ?? "";

app.get("/ui/ma7/profile", requireAuth, async (_req: Request, res: Response) => {
    if (!/^[a-zA-Z0-9_-]{1,32}$/.test(MA7_LOGIN)) {
        res.status(500).json({ error: "MA7_LOGIN не задан — укажи его в cli.env" }); return;
    }
    try {
        const r = await ma7.get("/api/admin/users", { params: { q: MA7_LOGIN, limit: 1 } });
        if (r.status >= 400) {
            const msg = r.status === 401 ? "MA7: неверный MA7_JWT_SECRET"
                : r.status === 403 ? "MA7: не та роль в токене"
                : (r.data as { error?: string })?.error ?? "Ошибка MA7";
            res.status(r.status === 401 || r.status === 403 ? 502 : r.status).json({ error: msg });
            return;
        }
        const user = (r.data.users as { login: string; balance: number }[] ?? [])
            .find(u => u.login === MA7_LOGIN);
        if (!user) { res.status(404).json({ error: "MA7_LOGIN не найден в MA7" }); return; }
        res.json({ login: user.login, balance: user.balance });
    } catch {
        res.status(502).json({ error: "MA7 недоступен" });
    }
});

app.use(["/api", "/health", "/awg"], requireAuth, proxy);

app.use(express.static(path.join(__dirname, "dist")));

app.get("*", (_req: Request, res: Response) => {
    res.sendFile(path.join(__dirname, "dist", "index.html"));
});

app.listen(PORT, () => { console.log(`ui: listening on :${PORT}`); });