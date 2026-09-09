// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
// Бэкенд админ-панели: логин, JWT-сессия, отдача собранного SPA.
// Никакой предметной логики здесь нет — это каркас, поверх которого
// навешиваются свои роуты под /api (они автоматически за requireAuth).
import express, { Request, Response, NextFunction } from "express";
import path   from "path";
import crypto from "crypto";

const app  = express();
const PORT = Number(process.env.PORT) || 8080;

const ADMIN_USER = process.env.ADMIN_USER ?? "admin";
const ADMIN_PASS = process.env.ADMIN_PASS ?? "";
// Без заданного секрета токены живут только до рестарта процесса — для дева
// это удобно, в проде JWT_SECRET обязателен (иначе сессии рвутся при рестарте).
const JWT_SECRET = process.env.JWT_SECRET ?? crypto.randomBytes(32).toString("hex");

// Каталог со сборкой фронта (vite build → frontend/dist).
const STATIC_DIR = process.env.STATIC_DIR ?? path.join(__dirname, "..", "frontend", "dist");

const TOKEN_TTL = 24 * 60 * 60; // сек

// Отозванные токены (logout). In-memory: рестарт процесса и так рвёт сессии,
// потому что при пустом JWT_SECRET секрет генерится заново.
const revoked = new Set<string>();

function tokenSig(token: string): string { return token.split(".")[2] ?? token; }

function sign(user: string): string {
    const h = Buffer.from('{"alg":"HS256","typ":"JWT"}').toString("base64url");
    const p = Buffer.from(JSON.stringify({
        sub: user,
        exp: Math.floor(Date.now() / 1000) + TOKEN_TTL,
    })).toString("base64url");
    const s = crypto.createHmac("sha256", JWT_SECRET).update(`${h}.${p}`).digest("base64url");
    return `${h}.${p}.${s}`;
}

interface Claims { sub?: string; exp: number; }

function verify(token: string): Claims | null {
    try {
        if (revoked.has(tokenSig(token))) return null;
        const [h, p, s] = token.split(".");
        if (!h || !p || !s) return null;
        const expected = crypto.createHmac("sha256", JWT_SECRET).update(`${h}.${p}`).digest();
        const actual   = Buffer.from(s, "base64url");
        if (actual.length !== expected.length) return null;
        if (!crypto.timingSafeEqual(actual, expected)) return null;
        const claims = JSON.parse(Buffer.from(p, "base64url").toString()) as Claims;
        if (claims.exp <= Math.floor(Date.now() / 1000)) return null;
        return claims;
    } catch { return null; }
}

interface AuthRequest extends Request { claims?: Claims; }

export function requireAuth(req: Request, res: Response, next: NextFunction) {
    const auth  = (req.headers["authorization"] ?? "") as string;
    const token = auth.startsWith("Bearer ") ? auth.slice(7) : "";
    const claims = token ? verify(token) : null;
    if (!claims) { res.status(401).json({ error: "Unauthorized" }); return; }
    (req as AuthRequest).claims = claims;
    next();
}

// Ограничение перебора пароля: 5 неудачных попыток на IP за 15 минут.
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
    if (!ADMIN_PASS || user !== ADMIN_USER || pass !== ADMIN_PASS) {
        res.status(401).json({ error: "Неверный логин или пароль" }); return;
    }
    loginAttempts.delete(ip);
    res.json({ token: sign(ADMIN_USER) });
});

app.post("/logout", requireAuth, (req: Request, res: Response) => {
    const token = (req.headers["authorization"] as string).slice(7);
    revoked.add(tokenSig(token));
    res.json({ ok: true });
});

// Всё под /api закрыто JWT — свои роуты добавляй ниже этой строки.
app.use("/api", requireAuth);

// Проверка живости сессии: фронт зовёт его при загрузке, чтобы понять,
// валиден ли сохранённый токен.
app.get("/api/session", (req: Request, res: Response) => {
    res.json({ user: (req as AuthRequest).claims?.sub ?? null });
});

// Статика SPA + fallback на index.html (роутинг на клиенте).
app.use(express.static(STATIC_DIR));

app.get("*", (_req: Request, res: Response) => {
    res.sendFile(path.join(STATIC_DIR, "index.html"));
});

app.listen(PORT, () => {
    if (!ADMIN_PASS) console.warn("admin: ADMIN_PASS не задан — вход отключён");
    console.log(`admin: listening on :${PORT}`);
});