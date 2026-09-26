// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Агент ноды. Сам открывает исходящее WSS-соединение к core (awg-ui), доказывает
// свою личность подписью Ed25519 и исполняет присланные запросы на локальном
// awg-ctrl (127.0.0.1). Входящих портов у ноды нет, awg-ctrl остаётся закрытым от сети.
// Протокол и формат join-строки — docs/node-protocol.md.
import crypto from "crypto";
import fs from "fs";
import path from "path";
import type { TLSSocket } from "tls";
import axios from "axios";
import WebSocket from "ws";

const CORE_HOST = process.env.CORE_HOST ?? "";
const CORE_PORT = Number(process.env.CORE_PORT) || 0;
const CORE_PIN  = process.env.CORE_PIN  ?? "";                 // SHA-256 SPKI сертификата core, base64url
const NODE_ID   = Number(process.env.NODE_ID) || 0;
const CTRL      = `http://127.0.0.1:${process.env.AWGCTRL_PORT ?? "3005"}`;
const NODE_KEY_FILE = process.env.NODE_KEY_FILE ?? path.join(__dirname, "node.key");
const INTERNAL_AUTH_KEY_FILE = process.env.INTERNAL_AUTH_KEY_FILE ?? "/etc/amnezia/amneziawg/internal_auth_private.key";
const CLI_ENV_FILE = process.env.CLI_ENV_FILE ?? "";
let joinSecret = process.env.JOIN_SECRET ?? "";                // одноразовый, живёт до первого успешного подключения

if (!CORE_HOST || !CORE_PORT || !CORE_PIN || !NODE_ID) {
    console.error("agent: не заданы CORE_HOST / CORE_PORT / CORE_PIN / NODE_ID — выполните `awg-ctrl join <ссылка>`");
    process.exit(1);
}

// ── Ключи ──────────────────────────────────────────────────────────────────
// Личность ноды (Ed25519): создаётся один раз, приватная часть с ноды не уходит.
function loadNodeKey(): crypto.KeyObject {
    if (!fs.existsSync(NODE_KEY_FILE)) {
        const { privateKey } = crypto.generateKeyPairSync("ed25519");
        fs.writeFileSync(NODE_KEY_FILE, privateKey.export({ type: "pkcs8", format: "pem" }), { mode: 0o600 });
        console.log("agent: создан ключ ноды " + NODE_KEY_FILE);
    }
    return crypto.createPrivateKey(fs.readFileSync(NODE_KEY_FILE));
}
const nodeKey = loadNodeKey();
const nodePubRaw = (() => {
    const der = crypto.createPublicKey(nodeKey).export({ type: "spki", format: "der" }) as Buffer;
    return der.subarray(der.length - 32).toString("base64url");
})();

let internalKey: crypto.KeyObject | null = null;
try { internalKey = crypto.createPrivateKey(fs.readFileSync(INTERNAL_AUTH_KEY_FILE)); }
catch { console.warn("agent: приватный ключ внутренней авторизации не найден: " + INTERNAL_AUTH_KEY_FILE); }

// Тот же короткоживущий токен, что awg-ui шлёт своему awg-ctrl.
function mintInternalToken(): string {
    if (!internalKey) throw new Error("no internal key");
    const t = Math.floor(Date.now() / 1000);
    const h = Buffer.from('{"alg":"EdDSA","typ":"JWT"}').toString("base64url");
    const p = Buffer.from(JSON.stringify({ iss: "awg-agent", iat: t, exp: t + 60 })).toString("base64url");
    return `${h}.${p}.${crypto.sign(null, Buffer.from(`${h}.${p}`), internalKey).toString("base64url")}`;
}

// Дублируется в awg-ui/nodes.ts.
const authMsg = (nonce: string) => Buffer.from(`awg-node-v1\n${nonce}\n${NODE_ID}`);

// ── Запросы core → awg-ctrl ────────────────────────────────────────────────
const METHODS = new Set(["GET", "POST", "PUT", "DELETE"]);
// Агент — не открытый прокси: только API самого awg-ctrl.
function pathAllowed(p: unknown): p is string {
    if (typeof p !== "string" || p.length > 2048 || !/^\/[\x21-\x7e]*$/.test(p)) return false;
    try {
        const u = new URL(p, "http://x");
        if (u.origin !== "http://x" || u.pathname.split("/").some(s => s === ".." || s === ".")) return false;
        return u.pathname === "/health" || u.pathname.startsWith("/api/") || u.pathname.startsWith("/awg/");
    } catch { return false; }
}

async function execute(m: { method?: unknown; path?: unknown; body?: unknown }): Promise<{ status: number; data: unknown }> {
    const method = typeof m.method === "string" ? m.method.toUpperCase() : "";
    if (!METHODS.has(method) || !pathAllowed(m.path)) return { status: 400, data: { error: "Запрос не разрешён агентом" } };
    try {
        const r = await axios({
            method, url: CTRL + m.path,
            headers: { Authorization: `Bearer ${mintInternalToken()}`, "Content-Type": "application/json" },
            data: m.body, validateStatus: () => true, timeout: 110_000,
        });
        return { status: r.status, data: r.data };
    } catch {
        return { status: 502, data: { error: "awg-ctrl недоступен" } };
    }
}

// ── Соединение с core ──────────────────────────────────────────────────────
function pinOf(sock: TLSSocket): string | null {
    const raw = sock.getPeerCertificate(true)?.raw;
    if (!raw) return null;
    const spki = new crypto.X509Certificate(raw).publicKey.export({ type: "spki", format: "der" }) as Buffer;
    return crypto.createHash("sha256").update(spki).digest("base64url");
}

// После первого успешного входа секрет больше не нужен — убираем его с диска.
function forgetJoinSecret() {
    joinSecret = "";
    delete process.env.JOIN_SECRET;
    if (!CLI_ENV_FILE) return;
    try {
        const kept = fs.readFileSync(CLI_ENV_FILE, "utf8").split("\n").filter(l => !l.startsWith("JOIN_SECRET="));
        fs.writeFileSync(CLI_ENV_FILE, kept.join("\n"));
    } catch { /* не критично: секрет одноразовый и с истекающим сроком */ }
}

let backoff = 1000;
const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms));

function connectOnce(): Promise<{ authFailed: boolean }> {
    return new Promise(resolve => {
        // Сертификат самоподписанный — цепочку не проверяем, доверие даёт pin (см. 'upgrade').
        const ws = new WebSocket(`wss://${CORE_HOST}:${CORE_PORT}/node/v1`, {
            rejectUnauthorized: false, handshakeTimeout: 10_000, maxPayload: 4 * 1024 * 1024,
        });
        let pinned = false, ready = false, deadTimer: NodeJS.Timeout | undefined;
        const armDead = () => { clearTimeout(deadTimer); deadTimer = setTimeout(() => ws.terminate(), 60_000); };

        // До этого момента ничего не отправляем и не читаем: секрет и подпись уходят только
        // тому, чей сертификат совпал с pin из join-строки.
        ws.on("upgrade", res => {
            const got = pinOf(res.socket as TLSSocket);
            if (got === CORE_PIN) { pinned = true; return; }
            console.error("agent: сертификат core не совпал с pin — соединение отклонено");
            ws.terminate();
        });

        ws.on("ping", armDead);
        ws.on("message", async raw => {
            if (!pinned) return;
            let m: any;
            try { m = JSON.parse(raw.toString()); } catch { ws.terminate(); return; }
            if (m.t === "hello" && typeof m.nonce === "string") {
                const auth: Record<string, unknown> = {
                    t: "auth", node_id: NODE_ID,
                    sig: crypto.sign(null, authMsg(m.nonce), nodeKey).toString("base64url"),
                };
                if (joinSecret) auth.join = { secret: joinSecret, pub: nodePubRaw };
                ws.send(JSON.stringify(auth));
            } else if (m.t === "ready") {
                ready = true; backoff = 1000; armDead();
                if (joinSecret) { forgetJoinSecret(); console.log("agent: нода привязана к core"); }
                console.log("agent: подключено к core");
            } else if (m.t === "req" && ready && Number.isInteger(m.id)) {
                const r = await execute(m);
                if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ t: "res", id: m.id, ...r }));
            }
        });
        ws.on("error", e => { if (!pinned || !ready) console.error("agent: " + (e as Error).message); });
        ws.on("close", code => {
            clearTimeout(deadTimer);
            if (code === 4003) console.error("agent: core отклонил ноду (неверный ключ, просроченная или использованная ссылка)");
            resolve({ authFailed: code === 4003 });
        });
    });
}

async function main() {
    console.log(`agent: нода #${NODE_ID} → wss://${CORE_HOST}:${CORE_PORT}`);
    for (;;) {
        const { authFailed } = await connectOnce();
        const wait = authFailed ? 60_000 : backoff;
        backoff = Math.min(backoff * 2, 60_000);
        await sleep(wait);
    }
}

main();
