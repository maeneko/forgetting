// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import fs, { readFileSync, writeFileSync, existsSync, mkdirSync } from "fs";
import path  from "path";
import crypto from "crypto";
import { execSync, spawnSync } from "child_process";
import * as zlib from "zlib";
import express, { Request, Response, NextFunction } from "express";
import Database from "better-sqlite3";
import winston from "winston";

const logsDir = path.join(process.cwd(), "logs");
if (!existsSync(logsDir)) mkdirSync(logsDir, { recursive: true });

const logger = winston.createLogger({
    level: "debug",
    transports: [
        new winston.transports.Console({
            format: winston.format.combine(
                winston.format.timestamp(),
                winston.format.colorize(),
                winston.format.printf(({ timestamp, level, message }) =>
                    `${timestamp} ${level}: ${message}`
                ),
            ),
        }),
        new winston.transports.File({
            filename: path.join(logsDir, "app.log"),
            format: winston.format.combine(
                winston.format.timestamp(),
                winston.format.json(),
            ),
        }),
    ],
});

const INTERNAL_AUTH_PUB_FILE = process.env.INTERNAL_AUTH_PUB_FILE
    ?? "/etc/amnezia/amneziawg/internal_auth_public.key";
let internalAuthPubKey: crypto.KeyObject;
try {
    internalAuthPubKey = crypto.createPublicKey(readFileSync(INTERNAL_AUTH_PUB_FILE));
} catch {
    logger.error("FATAL: публичный ключ внутренней авторизации не найден: " + INTERNAL_AUTH_PUB_FILE);
    process.exit(1);
}

const SERVER = {
    port: Number(process.env.PORT) || 3005,
};

const dbPath = path.join("/etc/amnezia/amneziawg", "users.db");
const db = new Database(dbPath);

db.exec(`
    CREATE TABLE IF NOT EXISTS users (
                                         name    TEXT PRIMARY KEY,
                                         ip      TEXT NOT NULL UNIQUE,
                                         pub_key TEXT NOT NULL,
                                         vpn_key TEXT NOT NULL,
                                         psk_key TEXT NOT NULL DEFAULT ''
    )
`);

db.exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ip ON users (ip)");

db.exec(`
    CREATE TABLE IF NOT EXISTS config (
                                          key   TEXT PRIMARY KEY,
                                          value TEXT NOT NULL
    )
`);

const cfgStmts = {
    get: db.prepare<[string], { value: string }>("SELECT value FROM config WHERE key = ?"),
    set: db.prepare<[string, string]>("INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)"),
};

function getCfg(key: string, fallback: string): string {
    return cfgStmts.get.get(key)?.value ?? fallback;
}

function setCfg(key: string, value: string) {
    cfgStmts.set.run(key, value);
}

function getLocalIp(): string {
    const route = spawnSync("ip", ["route", "show", "default"]);
    const iface = route.stdout.toString().match(/dev\s+(\S+)/)?.[1];
    if (!iface) return "";
    const addr = spawnSync("ip", ["addr", "show", iface]);
    return addr.stdout.toString().match(/inet\s+([\d.]+)/)?.[1] ?? "";
}

function initConfig() {
    const serverIp   = getCfg("serverIp",   process.env.SERVER_IP   ?? getLocalIp());
    const serverPort = getCfg("serverPort",  process.env.SERVER_PORT ?? "51820");
    const serverName = getCfg("serverName",  process.env.SERVER_NAME ?? "VPN");

    if (!serverIp) throw new Error("SERVER_IP не задан — передай через env при первом запуске");

    setCfg("serverIp",   serverIp);
    setCfg("serverPort", serverPort);
    setCfg("serverName", serverName);

    return { serverIp, serverPort: Number(serverPort), serverName };
}

interface AwgParams {
    Jc: number; Jmin: number; Jmax: number;
    S1: number; S2: number; S3: number; S4: number;
    H1: string; H2: string; H3: string; H4: string;
    I1: string; I2: string; I3: string; I4: string; I5: string;
}

const DEFAULT_AWG_PARAMS: AwgParams = {
    Jc: 6, Jmin: 10, Jmax: 50,
    S1: 90, S2: 45, S3: 37, S4: 14,
    H1: "1224800044-2116730834",
    H2: "2122053282-2133204808",
    H3: "2133604274-2140756116",
    H4: "2143656228-2147444225",
    I1: "<r 2><b 0x858000010001000000000669636c6f756403636f6d0000010001c00c000100010000105a00044d583737>",
    I2: "", I3: "", I4: "", I5: "",
};

function readAwgParams(): AwgParams {
    const confFile = path.join("/etc/amnezia/amneziawg", "awg1.conf");
    const params: AwgParams = { ...DEFAULT_AWG_PARAMS };
    if (!existsSync(confFile)) {
        logger.warn("awg1.conf не найден — параметры обфускации по умолчанию");
        return params;
    }
    const iface = readFileSync(confFile, "utf8").split(/^\[Peer\]/m)[0];

    const numKeys: (keyof AwgParams)[] = ["Jc", "Jmin", "Jmax", "S1", "S2", "S3", "S4"];
    const strKeys: (keyof AwgParams)[] = ["H1", "H2", "H3", "H4", "I1", "I2", "I3", "I4", "I5"];

    for (const k of numKeys) {
        const m = iface.match(new RegExp(`^\\s*${k}\\s*=\\s*(\\d+)`, "m"));
        if (m) (params[k] as number) = Number(m[1]);
    }
    for (const k of strKeys) {
        const m = iface.match(new RegExp(`^\\s*${k}\\s*=\\s*(.*)$`, "m"));
        if (m) (params[k] as string) = m[1].trim();
    }
    logger.info("awg params loaded from conf", { Jc: params.Jc, H1: params.H1 });
    return params;
}

const runtimeConfig = initConfig();
const CONFIG = {
    interface:  "awg1",
    confDir:    "/etc/amnezia/amneziawg",
    subnet:     "10.9",
    serverIp:   runtimeConfig.serverIp,
    serverPort: runtimeConfig.serverPort,
    serverName: runtimeConfig.serverName,
    dns1:       "1.1.1.1",
    dns2:       "1.0.0.1",
    mtu:        1376,
    keepalive:  25,
    awgParams:  readAwgParams(),
};

interface UserRow {
    name:    string;
    ip:      string;
    pub_key: string;
    vpn_key: string;
    psk_key: string;
}

const stmts = {
    get:    db.prepare<[string], UserRow>("SELECT * FROM users WHERE name = ?"),
    insert: db.prepare<[string, string, string, string, string]>("INSERT INTO users (name, ip, pub_key, vpn_key, psk_key) VALUES (?, ?, ?, ?, ?)"),
    delete: db.prepare<[string]>("DELETE FROM users WHERE name = ?"),
    ips:    db.prepare<[], { ip: string }>("SELECT ip FROM users"),
};

function run(cmd: string): string {
    return execSync(cmd, { encoding: "utf8" }).trim();
}

function generateKeys() {
    const privateKey = run("umask 077 && awg genkey");
    const r = spawnSync("awg", ["pubkey"], { input: privateKey, encoding: "utf8" });
    if (r.status !== 0) throw new Error("awg pubkey завершился с ошибкой");
    const publicKey    = (r.stdout as string).trim();
    const presharedKey = run("awg genpsk");
    return { privateKey, publicKey, presharedKey };
}

function getServerPublicKey(): string {
    const f = path.join(CONFIG.confDir, "server_public.key");
    if (!existsSync(f)) throw new Error("server_public.key не найден");
    return readFileSync(f, "utf8").trim();
}

function nextIp(): string {
    const usedIps = new Set(stmts.ips.all().map((r: { ip: string }) => r.ip));
    for (let c = 0; c <= 255; c++)
        for (let d = 2; d <= 254; d++) {
            const ip = `${CONFIG.subnet}.${c}.${d}`;
            if (!usedIps.has(ip)) return ip;
        }
    throw new Error("Подсеть заполнена");
}

// Официальный формат .conf: PrivateKey → AWG params (Jc,S,H,I) → Address → DNS
// ВНИМАНИЕ: пустые I2–I5 должны выводиться как «I2 = » с ОДНИМ хвостовым пробелом
// (так в рабочих ключах Amnezia). Пробел даётся через ${" "}, чтобы его не срезали
// ни IDE (strip trailing whitespace), ни инструменты правки. Не «чистить»!
function buildClientConf(
    keys: ReturnType<typeof generateKeys>,
    ip: string,
    serverPub: string,
): string {
    const p = CONFIG.awgParams;
    return `[Interface]
PrivateKey = ${keys.privateKey}
Jc = ${p.Jc}
Jmin = ${p.Jmin}
Jmax = ${p.Jmax}
S1 = ${p.S1}
S2 = ${p.S2}
S3 = ${p.S3}
S4 = ${p.S4}
H1 = ${p.H1}
H2 = ${p.H2}
H3 = ${p.H3}
H4 = ${p.H4}
I1 = ${p.I1}
I2 =${" "}
I3 =${" "}
I4 =${" "}
I5 =${" "}
Address = ${ip}/32
DNS = ${CONFIG.dns1}, ${CONFIG.dns2}

[Peer]
PublicKey = ${serverPub}
PresharedKey = ${keys.presharedKey}
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = ${CONFIG.serverIp}:${CONFIG.serverPort}
PersistentKeepalive = ${CONFIG.keepalive}
`;
}

function encodeVpnKey(
    keys: ReturnType<typeof generateKeys>,
    ip: string,
    serverPub: string,
): string {
    const p = CONFIG.awgParams;
    const clientConf = buildClientConf(keys, ip, serverPub);

    const lastConfigObj = {
        H1: p.H1, H2: p.H2, H3: p.H3, H4: p.H4,
        I1: p.I1, I2: "", I3: "", I4: "", I5: "",
        Jc:   String(p.Jc),
        Jmax: String(p.Jmax),
        Jmin: String(p.Jmin),
        S1: String(p.S1), S2: String(p.S2), S3: String(p.S3), S4: String(p.S4),
        allowed_ips:           ["0.0.0.0/0", "::/0"],
        clientId:              keys.publicKey,
        client_ip:             ip,
        client_priv_key:       keys.privateKey,
        client_pub_key:        keys.publicKey,
        config:                clientConf,
        hostName:              CONFIG.serverIp,
        mtu:                   String(CONFIG.mtu),
        persistent_keep_alive: String(CONFIG.keepalive),
        port:                  CONFIG.serverPort,
        psk_key:               keys.presharedKey,
        server_pub_key:        serverPub,
    };

    const json = JSON.stringify({
        containers: [{
            container: "amnezia-awg2",
            awg: {
                H1: p.H1, H2: p.H2, H3: p.H3, H4: p.H4,
                I1: p.I1, I2: "", I3: "", I4: "", I5: "",
                Jc:   String(p.Jc),
                Jmax: String(p.Jmax),
                Jmin: String(p.Jmin),
                S1: String(p.S1), S2: String(p.S2),
                S3: String(p.S3), S4: String(p.S4),
                last_config:      JSON.stringify(lastConfigObj, null, 2),
                port:             String(CONFIG.serverPort),
                protocol_version: "2",
                subnet_address:   `${CONFIG.subnet}.0.0`,
                transport_proto:  "udp",
            },
        }],
        defaultContainer: "amnezia-awg2",
        description:      CONFIG.serverName,
        dns1:             CONFIG.dns1,
        dns2:             CONFIG.dns2,
        hostName:         CONFIG.serverIp,
        nameOverriddenByUser: true,
    });

    const jsonBuf    = Buffer.from(json, "utf8");
    const compressed = zlib.deflateSync(jsonBuf);
    const header     = Buffer.alloc(4);
    header.writeUInt32BE(jsonBuf.length, 0);
    return "vpn://" + Buffer.concat([header, compressed])
        .toString("base64url")
        .replace(/=+$/, "");
}

function getPeersData(): Record<string, { online: boolean; lastHandshake: number; rx: number; tx: number }> {
    try {
        const output = run(`awg show ${CONFIG.interface} dump`);
        const result: Record<string, { online: boolean; lastHandshake: number; rx: number; tx: number }> = {};
        const now   = Math.floor(Date.now() / 1000);
        const lines = output.split("\n");
        for (let i = 1; i < lines.length; i++) {
            const parts         = lines[i].split("\t");
            const pubKey        = parts[0];
            if (!pubKey) continue;
            const lastHandshake = Number(parts[4]);
            const rx            = Number(parts[5]);
            const tx            = Number(parts[6]);
            result[pubKey] = {
                online: lastHandshake > 0 && (now - lastHandshake) < 180,
                lastHandshake, rx, tx,
            };
        }
        return result;
    } catch (e) { logger.warn("getPeersData failed", { error: e }); return {}; }
}

function rebuildConf() {
    const users    = db.prepare("SELECT name, ip, pub_key, psk_key FROM users").all() as UserRow[];
    const confFile = path.join(CONFIG.confDir, `${CONFIG.interface}.conf`);
    if (!existsSync(confFile)) return;

    const conf  = readFileSync(confFile, "utf8");
    const iface = conf.split(/^\[Peer\]/m)[0].trimEnd();
    const peers = (users as any[]).map(u =>
        `\n# ${u.name}\n[Peer]\nPublicKey = ${u.pub_key}\nPresharedKey = ${u.psk_key}\nAllowedIPs = ${u.ip}/32\nPersistentKeepalive = ${CONFIG.keepalive}`
    ).join("\n");

    writeFileSync(confFile, iface + "\n" + peers + "\n");
    logger.info("conf rebuilt", { peers: users.length });
}

const SERVER_PRIV_KEY_FILE = "/etc/amnezia/server_private.key";

const AWGQUICK_ONLY_KEY = /^\s*(Address|DNS|MTU|Table|PreUp|PostUp|PreDown|PostDown|SaveConfig)\s*=/i;

// Готовит «stripped»-конфиг для `awg syncconf`: берёт awg1.conf, выкидывает
// awg-quick-ключи и оставляет [Interface] (PrivateKey + Jc/S/H + ListenPort) и
// [Peer]-блоки.
//   🛑 КРИТИЧНО: [Interface] с PrivateKey ОБЯЗАН попасть в этот конфиг. Раньше
//   syncPeers отдавал в syncconf только [Peer]-блоки — и AmneziaWG обнулял
//   приватный ключ интерфейса и параметры обфускации, после чего сервер
//   поднимался с public-key=(none) и ВСЕ клиенты отваливались.
// PrivateKey подставляем из server_private.key — это та же идентичность, что в
// server_public.key (его зашивают в vpn:// ключи клиентов) и в PostUp.
function buildSyncConf(): string {
    const confFile = path.join(CONFIG.confDir, `${CONFIG.interface}.conf`);
    const priv     = readFileSync(SERVER_PRIV_KEY_FILE, "utf8").trim();
    const out: string[] = [];
    let privReplaced = false;
    for (const line of readFileSync(confFile, "utf8").split("\n")) {
        if (AWGQUICK_ONLY_KEY.test(line)) continue;
        if (/^\s*PrivateKey\s*=/.test(line)) {
            out.push(`PrivateKey = ${priv}`);
            privReplaced = true;
            continue;
        }
        out.push(line);
    }
    if (!privReplaced) {
        const idx = out.findIndex(l => /^\s*\[Interface\]/.test(l));
        if (idx >= 0) out.splice(idx + 1, 0, `PrivateKey = ${priv}`);
    }
    return out.join("\n");
}

function syncPeers() {
    rebuildConf();
    const tmpFile = `/tmp/awg_sync_${Date.now()}.conf`;
    try {
        writeFileSync(tmpFile, buildSyncConf(), { mode: 0o600 });
        const r = spawnSync("awg", ["syncconf", CONFIG.interface, tmpFile]);
        if (r.status !== 0) logger.warn("syncPeers syncconf failed", { stderr: r.stderr?.toString() });
    } finally {
        try { fs.unlinkSync(tmpFile); } catch {}
    }
    const n = (db.prepare("SELECT COUNT(*) AS n FROM users").get() as { n: number }).n;
    logger.info("peers synced", { count: n });
}

function getInterfaceStatus(): { up: boolean; peers: number; publicKey: string | null } {
    const r = spawnSync("awg", ["show", CONFIG.interface]);
    if (r.status !== 0) return { up: false, peers: 0, publicKey: null };
    const output    = r.stdout.toString();
    const peers     = (output.match(/^peer:/gm) ?? []).length;
    const publicKey = output.match(/public key:\s*(.+)/)?.[1]?.trim() ?? null;
    return { up: true, peers, publicKey };
}

function ensureInterfaceUp() {
    const { up } = getInterfaceStatus();
    if (!up) throw new Error(`Interface ${CONFIG.interface} is not up. Run: awg-quick up ${CONFIG.interface}`);
}

function restartAwg() {
    logger.info("AWG restart: down");
    const down = spawnSync("awg-quick", ["down", CONFIG.interface]);
    if (down.status !== 0) logger.warn("awg-quick down failed", { stderr: down.stderr?.toString() });
    logger.info("AWG restart: up");
    const up = spawnSync("awg-quick", ["up", CONFIG.interface]);
    if (up.status !== 0) throw new Error(`awg-quick up failed: ${up.stderr?.toString()}`);
    syncPeers();
    logger.info("AWG restart: done");
}

function startInterface() {
    const status = getInterfaceStatus();
    if (status.up) { logger.info("AWG already up", { peers: status.peers }); return status; }
    const r = spawnSync("awg-quick", ["up", CONFIG.interface]);
    if (r.status !== 0) throw new Error(`awg-quick up failed: ${r.stderr?.toString()}`);
    syncPeers();
    return getInterfaceStatus();
}

function addUser(username: string): UserRow {
    const keys      = generateKeys();
    const serverPub = getServerPublicKey();

    const ip = db.transaction(() => {
        const ip = nextIp();
        stmts.insert.run(username, ip, keys.publicKey, "", keys.presharedKey);
        return ip;
    })();

    const vpn_key = encodeVpnKey(keys, ip, serverPub);
    db.prepare("UPDATE users SET vpn_key = ? WHERE name = ?").run(vpn_key, username);

    const tmpPsk = `/tmp/awg_psk_${Date.now()}.tmp`;
    writeFileSync(tmpPsk, keys.presharedKey);
    const r = spawnSync("awg", [
        "set", CONFIG.interface, "peer", keys.publicKey,
        "preshared-key", tmpPsk,
        "allowed-ips", `${ip}/32`,
        "persistent-keepalive", String(CONFIG.keepalive),
    ]);
    try { fs.unlinkSync(tmpPsk); } catch {}
    if (r.status !== 0) {
        stmts.delete.run(username);
        throw new Error(`awg set failed: ${r.stderr?.toString()}`);
    }

    rebuildConf();
    logger.info("user created", { name: username, ip });
    return { name: username, ip, pub_key: keys.publicKey, vpn_key, psk_key: keys.presharedKey };
}

function removeUser(username: string) {
    const user = stmts.get.get(username);
    if (!user) throw new Error("Пользователь не найден");

    stmts.delete.run(username);
    spawnSync("awg", ["set", CONFIG.interface, "peer", user.pub_key, "remove"]);
    rebuildConf();

    for (const ext of [".conf", ".key"]) {
        const f = path.join(CONFIG.confDir, "clients", username + ext);
        if (existsSync(f)) fs.unlinkSync(f);
    }
    logger.info("user removed", { name: username });
}

const app = express();
app.use(express.json({ limit: "1kb" }));

function verifyInternalToken(token: string): boolean {
    try {
        const [h, p, s] = token.split(".");
        if (!h || !p || !s) return false;
        const ok = crypto.verify(null, Buffer.from(`${h}.${p}`), internalAuthPubKey, Buffer.from(s, "base64url"));
        if (!ok) return false;
        const { exp } = JSON.parse(Buffer.from(p, "base64url").toString()) as { exp: number };
        return exp > Math.floor(Date.now() / 1000);
    } catch { return false; }
}

function auth(req: Request, res: Response, next: NextFunction) {
    const header = (req.headers["authorization"] ?? "") as string;
    const token  = header.startsWith("Bearer ") ? header.slice(7) : "";
    if (!token || !verifyInternalToken(token)) {
        res.status(401).json({ error: "Неверная авторизация" }); return;
    }
    next();
}

function validateName(req: Request, res: Response, next: NextFunction) {
    const name = req.params.name ?? (req.body as { name?: string }).name;
    if (!name || !/^[a-zA-Z0-9_-]{1,32}$/.test(name)) {
        res.status(400).json({ error: "Имя: буквы, цифры, _ и -, до 32 символов" }); return;
    }
    next();
}

function handler(fn: (req: Request, res: Response) => void | Promise<void>) {
    return async (req: Request, res: Response) => {
        try { await fn(req, res); }
        catch (e) { logger.error("handler error", { error: e }); res.status(500).json({ error: "Internal server error" }); }
    };
}

app.get("/health", (_req, res) => {
    const { up, peers } = getInterfaceStatus();
    res.status(up ? 200 : 503).json({
        status: up ? "ok" : "degraded",
        server: CONFIG.serverName,
        ip:     CONFIG.serverIp,
        awg:    { status: up ? "ok" : "down", peers },
    });
});

app.post("/api/users", auth, validateName, handler((req, res) => {
    const { name } = req.body as { name: string };
    if (stmts.get.get(name)) {
        res.status(409).json({ error: "Пользователь уже существует" }); return;
    }
    res.status(201).json(addUser(name));
}));

app.get("/api/users", auth, handler((_req, res) => {
    const users = db.prepare("SELECT name, ip, pub_key, vpn_key FROM users WHERE vpn_key != ''").all() as UserRow[];
    const peers = getPeersData();
    res.json({
        users: users.map(u => ({
            ...u,
            online:        peers[u.pub_key]?.online        ?? false,
            lastHandshake: peers[u.pub_key]?.lastHandshake ?? 0,
        })),
    });
}));

app.get("/api/users/stats", auth, handler((_req, res) => {
    const users = db.prepare("SELECT name, ip, pub_key FROM users WHERE vpn_key != ''").all() as UserRow[];
    const peers = getPeersData();
    res.json({
        users: users.map(u => ({
            name:          u.name,
            ip:            u.ip,
            online:        peers[u.pub_key]?.online        ?? false,
            lastHandshake: peers[u.pub_key]?.lastHandshake ?? 0,
            rx:            peers[u.pub_key]?.rx            ?? 0,
            tx:            peers[u.pub_key]?.tx            ?? 0,
        })),
    });
}));

app.post("/api/users/:name", auth, validateName, handler((req, res) => {
    const user = stmts.get.get(req.params.name);
    if (!user) { res.status(404).json({ error: "Пользователь не найден" }); return; }
    res.json(user);
}));

app.delete("/api/users/:name", auth, validateName, handler((req, res) => {
    removeUser(req.params.name);
    res.json({ success: true, name: req.params.name });
}));

app.post("/awg/restart", auth, handler((_req, res) => {
    restartAwg();
    res.json({ success: true });
}));

app.get("/awg/status", auth, handler((_req, res) => {
    res.json(getInterfaceStatus());
}));

app.post("/awg/start", auth, handler((_req, res) => {
    res.json(startInterface());
}));

app.listen(SERVER.port, "127.0.0.1", () => {
    logger.info("Server started", { port: SERVER.port, host: "127.0.0.1", serverName: CONFIG.serverName, serverIp: CONFIG.serverIp });
});
ensureInterfaceUp();
syncPeers();
