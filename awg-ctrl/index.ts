// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import fs, { readFileSync, writeFileSync, existsSync, mkdirSync } from "fs";
import path  from "path";
import os   from "os";
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

// Миграции users: better-sqlite3 синхронный, ALTER TABLE идемпотентным не бывает,
// поэтому смотрим фактический список колонок.
//   key_gen      — поколение протокола, на параметрах которого выдан vpn_key
//                  ('2' для всего, что заведено до появления 3.1)
//   vpn_key_prev — предыдущий блоб, чтобы перевыпуск можно было откатить
{
    const cols = new Set(
        (db.prepare("PRAGMA table_info(users)").all() as { name: string }[]).map(c => c.name),
    );
    if (!cols.has("key_gen"))
        db.exec("ALTER TABLE users ADD COLUMN key_gen TEXT NOT NULL DEFAULT '2'");
    if (!cols.has("vpn_key_prev"))
        db.exec("ALTER TABLE users ADD COLUMN vpn_key_prev TEXT NOT NULL DEFAULT ''");
}

db.exec(`
    CREATE TABLE IF NOT EXISTS config (
                                          key   TEXT PRIMARY KEY,
                                          value TEXT NOT NULL
    )
`);

// Пиры устройств sen://-подписки. Отдельная таблица, users не трогаем: у этих
// пиров нет ни vpn_key, ни имени — приватный ключ живёт только на устройстве,
// сюда приходит лишь публичный. owner — непрозрачная метка от awg-ui
// («m<masterId>/d<deviceId>»), нужна только для комментария в conf и логов.
db.exec(`
    CREATE TABLE IF NOT EXISTS peers (
        pub_key    TEXT PRIMARY KEY,
        ip         TEXT NOT NULL UNIQUE,
        psk_key    TEXT NOT NULL,
        owner      TEXT NOT NULL DEFAULT '',
        created_at INTEGER NOT NULL
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

// Ключи, появившиеся в AmneziaWG 3.0/3.1. Пустая строка = ключ не задан, тогда
// интерфейс работает в режиме 2.0 и его нет ни в conf, ни в vpn:// ключе.
// Порядок массива = порядок строк в клиентском .conf (см. buildClientConf).
const AWG3_KEYS = [
    "HeaderProtectionKey",
    "ContentPaddingAddition",
    "RekeyAfterTime",
    "RekeyTimeout",
    "RejectAfterTime",
    "KeepaliveTimeout",
    "MaxHandshakeAttempts",
    "RandomTrailers",
    "DisableCookies",
] as const;

type Awg3Key = (typeof AWG3_KEYS)[number];

type AwgParams = {
    Jc: number; Jmin: number; Jmax: number;
    S1: number; S2: number; S3: number; S4: number;
    H1: string; H2: string; H3: string; H4: string;
    I1: string; I2: string; I3: string; I4: string; I5: string;
} & Record<Awg3Key, string>;

const DEFAULT_AWG_PARAMS: AwgParams = {
    Jc: 6, Jmin: 10, Jmax: 50,
    S1: 90, S2: 45, S3: 37, S4: 14,
    H1: "1224800044-2116730834",
    H2: "2122053282-2133204808",
    H3: "2133604274-2140756116",
    H4: "2143656228-2147444225",
    I1: "<r 2><b 0x858000010001000000000669636c6f756403636f6d0000010001c00c000100010000105a00044d583737>",
    I2: "", I3: "", I4: "", I5: "",
    // 3.x по умолчанию выключен: без conf-а сервер остаётся на 2.0.
    HeaderProtectionKey: "", ContentPaddingAddition: "",
    RekeyAfterTime: "", RekeyTimeout: "", RejectAfterTime: "",
    KeepaliveTimeout: "", MaxHandshakeAttempts: "",
    RandomTrailers: "", DisableCookies: "",
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
    const strKeys: (keyof AwgParams)[] = [
        "H1", "H2", "H3", "H4", "I1", "I2", "I3", "I4", "I5", ...AWG3_KEYS,
    ];

    for (const k of numKeys) {
        const m = iface.match(new RegExp(`^\\s*${k}\\s*=\\s*(\\d+)`, "m"));
        if (m) (params[k] as number) = Number(m[1]);
    }
    for (const k of strKeys) {
        const m = iface.match(new RegExp(`^\\s*${k}\\s*=\\s*(.*)$`, "m"));
        if (m) (params[k] as string) = m[1].trim();
    }
    logger.info("awg params loaded from conf", {
        Jc: params.Jc, H1: params.H1, gen: genOf(params),
    });
    return params;
}

// Поколение протокола выводим из самих параметров, отдельного флага нет: conf
// остаётся единственным источником правды. Header protection — та фича, которая
// ломает совместимость с 2.0, поэтому именно она и определяет поколение.
function genOf(p: AwgParams): "2" | "3.1" {
    return p.HeaderProtectionKey ? "3.1" : "2";
}

// Версия модуля/tools для /health — показывает оператору, что реально работает
// в ядре сейчас (см. install.sh: apt может обновить пакет, пока в ядре живёт
// старый загруженный модуль, коммит 6fdacd7). Загруженная версия важнее версии
// на диске ровно по той же причине. Вызывается на старте и при restartAwg() —
// это единственные моменты, когда модуль может реально смениться в течение
// жизни процесса; из обработчика /health не вызывается — этот роут без
// авторизации, спавнить подпроцессы на каждый его опрос нельзя.
function readAwgVersions(): { module: string; tools: string } {
    let module = "";
    try {
        module = readFileSync("/sys/module/amneziawg/version", "utf8").trim();
    } catch {
        try { module = run("modinfo -F version amneziawg"); } catch { /* модуль не загружен */ }
    }
    let tools = "";
    try {
        const out = run("awg --version");
        tools = out.split(/\s+/).find(t => /^v?\d/.test(t)) ?? "";
    } catch { /* awg-tools не найден */ }
    return { module, tools };
}

// PersistentKeepalive в 3.1 задаётся диапазоном (дефолт клиента AmneziaVPN);
// в 2.0 это одно число. Уходит и в серверные [Peer], и в клиентский конфиг.
const KEEPALIVE_BY_GEN: Record<"2" | "3.1", string> = { "2": "25", "3.1": "25-35" };

// Значения 3.1-параметров для перевода уже работающего 2.0-сервера (POST
// /awg/upgrade) — те же, что пишет install.sh (дефолты клиента AmneziaVPN,
// protocolConstants.h), в порядке AWG3_KEYS. RandomTrailers/DisableCookies не
// включаем — как и инсталлятор. HeaderProtectionKey сюда не входит: он
// генерируется на месте (awg genpsk) и фиксируется на весь срок жизни сервера.
const AWG31_DEFAULTS: Record<string, string> = {
    ContentPaddingAddition: "10-100",
    RekeyAfterTime:         "100-120",
    RekeyTimeout:           "3-7",
    RejectAfterTime:        "150-180",
    KeepaliveTimeout:       "5-15",
    MaxHandshakeAttempts:   "15-20",
};

// При заданном HeaderProtectionKey ни один из S1–S4 не может быть меньше 12
// (HEADER_PROTECTION_NONCE_SIZE). Модуль на нарушение отвечает только
// «Invalid argument»; причина видна лишь при
// `echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control`.
const MIN_S_FOR_HEADER_PROTECTION = 12;

const runtimeConfig = initConfig();
const AWG_PARAMS    = readAwgParams();
const AWG_GEN       = genOf(AWG_PARAMS);
const AWG_VERSIONS  = readAwgVersions();
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
    keepalive:  KEEPALIVE_BY_GEN[AWG_GEN],
    awgParams:  AWG_PARAMS,
    gen:        AWG_GEN,
    // Мутируются в restartAwg() — единственном месте, где модуль может
    // реально смениться в течение жизни процесса.
    awgModule:  AWG_VERSIONS.module,
    awgTools:   AWG_VERSIONS.tools,
};

interface UserRow {
    name:         string;
    ip:           string;
    pub_key:      string;
    vpn_key:      string;
    psk_key:      string;
    key_gen:      string;
    vpn_key_prev: string;
}

interface PeerRow {
    pub_key:    string;
    ip:         string;
    psk_key:    string;
    owner:      string;
    created_at: number;
}

const stmts = {
    peersAll:    db.prepare<[], PeerRow>("SELECT * FROM peers ORDER BY created_at, rowid"),
    peerGet:     db.prepare<[string], PeerRow>("SELECT * FROM peers WHERE pub_key = ?"),
    peerInsert:  db.prepare<[string, string, string, string, number]>("INSERT INTO peers (pub_key, ip, psk_key, owner, created_at) VALUES (?, ?, ?, ?, ?)"),
    peerDelete:  db.prepare<[string]>("DELETE FROM peers WHERE pub_key = ?"),
    peerRekey:   db.prepare<[string, string]>("UPDATE peers SET pub_key = ? WHERE pub_key = ?"),
    peerPsk:     db.prepare<[string, string]>("UPDATE peers SET psk_key = ? WHERE pub_key = ?"),
    userPubTaken: db.prepare<[string], { n: number }>("SELECT COUNT(*) AS n FROM users WHERE pub_key = ?"),
    get:    db.prepare<[string], UserRow>("SELECT * FROM users WHERE name = ?"),
    all:    db.prepare<[], UserRow>("SELECT * FROM users"),
    insert: db.prepare<[string, string, string, string, string, string]>("INSERT INTO users (name, ip, pub_key, vpn_key, psk_key, key_gen) VALUES (?, ?, ?, ?, ?, ?)"),
    delete: db.prepare<[string]>("DELETE FROM users WHERE name = ?"),
    ips:    db.prepare<[], { ip: string }>("SELECT ip FROM users UNION SELECT ip FROM peers"),
    // Перевыпуск: старый блоб уезжает в vpn_key_prev, pub_key/psk_key могут
    // смениться, если исходный ключ не удалось разобрать.
    reissue: db.prepare<[string, string, string, string, string]>(
        "UPDATE users SET vpn_key_prev = vpn_key, vpn_key = ?, pub_key = ?, psk_key = ?, key_gen = ? WHERE name = ?",
    ),
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

// Официальный формат .conf: PrivateKey → AWG params (Jc,S,H,I) → 3.x-ключи →
// Address → DNS. Порядок 3.x-блока взят из client/server_scripts/awg/template.conf
// клиента AmneziaVPN; пустые ключи не выводятся вовсе — тогда конфиг остаётся
// ровно тем же 2.0-конфигом, что и до появления поддержки 3.1.
// ВНИМАНИЕ: пустые I2–I5 должны выводиться как «I2 = » с ОДНИМ хвостовым пробелом
// (так в рабочих ключах Amnezia). Пробел даётся через ${" "}, чтобы его не срезали
// ни IDE (strip trailing whitespace), ни инструменты правки. Не «чистить»!
function buildClientConf(
    keys: ReturnType<typeof generateKeys>,
    ip: string,
    serverPub: string,
): string {
    const p = CONFIG.awgParams;
    const awg3 = AWG3_KEYS.filter(k => p[k]).map(k => `${k} = ${p[k]}\n`).join("");
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
${awg3}Address = ${ip}/32
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

    // Ключи в объектах идут в том же ASCII-алфавитном порядке, в каком их
    // сериализует QJsonObject клиента AmneziaVPN. Незаданные 3.x-ключи
    // выбрасываются в dropEmptyAwg3 — на 2.0 объекты остаются прежними байт-в-байт.
    const dropEmptyAwg3 = <T extends Record<string, unknown>>(o: T): T => {
        for (const k of AWG3_KEYS) if (!p[k]) delete o[k];
        return o;
    };

    const lastConfigObj = dropEmptyAwg3({
        ContentPaddingAddition: p.ContentPaddingAddition,
        DisableCookies:         p.DisableCookies,
        H1: p.H1, H2: p.H2, H3: p.H3, H4: p.H4,
        HeaderProtectionKey:    p.HeaderProtectionKey,
        I1: p.I1, I2: "", I3: "", I4: "", I5: "",
        Jc:   String(p.Jc),
        Jmax: String(p.Jmax),
        Jmin: String(p.Jmin),
        KeepaliveTimeout:       p.KeepaliveTimeout,
        MaxHandshakeAttempts:   p.MaxHandshakeAttempts,
        RandomTrailers:         p.RandomTrailers,
        RejectAfterTime:        p.RejectAfterTime,
        RekeyAfterTime:         p.RekeyAfterTime,
        RekeyTimeout:           p.RekeyTimeout,
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
    });

    const json = JSON.stringify({
        containers: [{
            container: "amnezia-awg2",
            awg: dropEmptyAwg3({
                ContentPaddingAddition: p.ContentPaddingAddition,
                DisableCookies:         p.DisableCookies,
                H1: p.H1, H2: p.H2, H3: p.H3, H4: p.H4,
                HeaderProtectionKey:    p.HeaderProtectionKey,
                I1: p.I1, I2: "", I3: "", I4: "", I5: "",
                Jc:   String(p.Jc),
                Jmax: String(p.Jmax),
                Jmin: String(p.Jmin),
                KeepaliveTimeout:       p.KeepaliveTimeout,
                MaxHandshakeAttempts:   p.MaxHandshakeAttempts,
                RandomTrailers:         p.RandomTrailers,
                RejectAfterTime:        p.RejectAfterTime,
                RekeyAfterTime:         p.RekeyAfterTime,
                RekeyTimeout:           p.RekeyTimeout,
                S1: String(p.S1), S2: String(p.S2),
                S3: String(p.S3), S4: String(p.S4),
                last_config:      JSON.stringify(lastConfigObj, null, 2),
                port:             String(CONFIG.serverPort),
                protocol_version: CONFIG.gen,
                subnet_address:   `${CONFIG.subnet}.0.0`,
                transport_proto:  "udp",
            }),
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

// Обратная к encodeVpnKey: vpn:// → base64url → снять 4-байтовый BE-заголовок
// длины → inflate → JSON. Приватный ключ клиента больше нигде не хранится, поэтому
// это единственный способ перевыпустить ключ, не меняя личность пира.
function decodeVpnKey(vpnKey: string): any | null {
    try {
        const buf = Buffer.from(vpnKey.replace(/^vpn:\/\//, ""), "base64url");
        if (buf.length <= 4) return null;
        return JSON.parse(zlib.inflateSync(buf.subarray(4)).toString("utf8"));
    } catch {
        return null;
    }
}

function clientPrivKeyFrom(vpnKey: string): string | null {
    const lastConfig = decodeVpnKey(vpnKey)?.containers?.[0]?.awg?.last_config;
    if (typeof lastConfig !== "string") return null;
    try {
        const priv = JSON.parse(lastConfig).client_priv_key;
        return typeof priv === "string" && priv ? priv : null;
    } catch {
        return null;
    }
}

interface ReissueResult { total: number; reissued: number; regenerated: string[]; backup: string }

// Перевыпуск всех vpn:// ключей на текущих параметрах интерфейса. Нужен после
// смены поколения (2.0 → 3.1): старые ключи собраны на старых параметрах и
// перестают работать. IP, pub_key и psk_key сохраняются — на проводе ничего не
// меняется, клиенту достаточно заново импортировать ключ.
function reissueAll(): ReissueResult {
    const users     = stmts.all.all();
    const serverPub = getServerPublicKey();
    const backup    = `${dbPath}.bak-${new Date().toISOString().replace(/[:.]/g, "-")}`;
    db.prepare("VACUUM INTO ?").run(backup);
    logger.info("reissue: db backed up", { backup, users: users.length });

    const regenerated: string[] = [];
    let reissued = 0;

    for (const u of users) {
        const priv = clientPrivKeyFrom(u.vpn_key);
        let keys: ReturnType<typeof generateKeys>;

        if (priv) {
            keys = { privateKey: priv, publicKey: u.pub_key, presharedKey: u.psk_key };
        } else {
            // Блоб не разобрался — личность пира восстановить неоткуда, выдаём новую.
            logger.warn("reissue: vpn_key не декодируется, генерируем новую пару", { name: u.name });
            keys = generateKeys();
            spawnSync("awg", ["set", CONFIG.interface, "peer", u.pub_key, "remove"]);
            const r = setPeer(keys.publicKey, keys.presharedKey, u.ip);
            if (r.status !== 0) {
                logger.error("reissue: awg set failed", { name: u.name, stderr: r.stderr?.toString() });
                continue;
            }
            regenerated.push(u.name);
        }

        stmts.reissue.run(
            encodeVpnKey(keys, u.ip, serverPub),
            keys.publicKey, keys.presharedKey, CONFIG.gen, u.name,
        );
        reissued++;
    }

    syncPeers();
    logger.info("reissue: done", { total: users.length, reissued, regenerated: regenerated.length });
    return { total: users.length, reissued, regenerated, backup };
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
    const devices  = stmts.peersAll.all();
    const confFile = path.join(CONFIG.confDir, `${CONFIG.interface}.conf`);
    if (!existsSync(confFile)) return;

    const conf  = readFileSync(confFile, "utf8");
    const iface = conf.split(/^\[Peer\]/m)[0].trimEnd();
    // Без записей в peers вывод байт-в-байт прежний.
    const peers = [
        ...users.map(u => ({ label: u.name, ...u })),
        ...devices.map(d => ({ label: d.owner || "device", ...d })),
    ].map(u =>
        `\n# ${u.label}\n[Peer]\nPublicKey = ${u.pub_key}\nPresharedKey = ${u.psk_key}\nAllowedIPs = ${u.ip}/32\nPersistentKeepalive = ${CONFIG.keepalive}`
    ).join("\n");

    writeFileSync(confFile, iface + "\n" + peers + "\n");
    logger.info("conf rebuilt", { peers: users.length + devices.length });
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

// Параметры обфускации и поколение живут в awg1.conf, а не в процессе:
// перечитываем их после любой правки конфига. Иначе awg-ctrl продолжит
// собирать vpn:// ключи на старых параметрах — например, на 2.0 уже после
// перехода интерфейса на 3.1.
function reloadAwgParams(): "2" | "3.1" {
    CONFIG.awgParams = readAwgParams();
    CONFIG.gen       = genOf(CONFIG.awgParams);
    CONFIG.keepalive = KEEPALIVE_BY_GEN[CONFIG.gen];
    return CONFIG.gen;
}

function restartAwg() {
    logger.info("AWG restart: down");
    const down = spawnSync("awg-quick", ["down", CONFIG.interface]);
    if (down.status !== 0) logger.warn("awg-quick down failed", { stderr: down.stderr?.toString() });
    logger.info("AWG restart: up");
    const up = spawnSync("awg-quick", ["up", CONFIG.interface]);
    if (up.status !== 0) throw new Error(`awg-quick up failed: ${up.stderr?.toString()}`);
    // Конфиг мог измениться, пока интерфейс лежал (переход на 3.1, ручная
    // правка awg1.conf) — перечитываем ДО syncPeers: keepalive из CONFIG уходит
    // в [Peer]-блоки, которые rebuildConf() пересобирает прямо сейчас.
    const gen = reloadAwgParams();
    syncPeers();
    const versions = readAwgVersions();
    CONFIG.awgModule = versions.module;
    CONFIG.awgTools  = versions.tools;
    logger.info("AWG restart: done", { ...versions, gen });
}

// ─────────────────────────── переход 2.0 → 3.1 ───────────────────────────
// 3.1 — основное поколение (install.sh ставит его по умолчанию). Сервера,
// установленные раньше, переводятся отсюда, без переустановки: панель дёргает
// POST /awg/upgrade.

// Header protection использует библиотечный chacha-API (chacha_init/
// chacha20_crypt), которого нет до ядра 5.5 — там модуль 3.x просто не
// соберётся (upstream issue #210).
function kernelSupportsHeaderProtection(): boolean {
    const [maj, min] = os.release().split(".").map(n => Number.parseInt(n, 10));
    if (!Number.isFinite(maj) || !Number.isFinite(min)) return false;
    return maj > 5 || (maj === 5 && min >= 5);
}

// Пред-проверка перед тем, как трогать конфиг: решает ЗАГРУЖЕННЫЙ модуль
// (readAwgVersions читает /sys/module/amneziawg/version), а не тот, что лежит
// на диске после apt — обслуживает интерфейс именно загруженный.
function checkGen31Support(): string | null {
    const { module } = readAwgVersions();
    if (!module.startsWith("3."))
        return `В ядре загружен модуль amneziawg ${module || "неизвестной версии"} — для 3.1 нужна 3.x. `
             + "Обнови пакет (apt) и перезагрузи сервер, затем повтори переход.";
    if (!kernelSupportsHeaderProtection())
        return `Ядро ${os.release()} старше 5.5 — header protection в нём не работает (upstream issue #210).`;
    return null;
}

const AWG3_KEY_LINE = new RegExp(`^\\s*(${AWG3_KEYS.join("|")})\\s*=`);

// Готовит текст awg1.conf для 3.1: выкидывает старые 3.x-строки (конфиг могли
// править руками), поднимает S1–S4 до допустимых при header protection и
// дописывает 3.x-блок в конец [Interface]. [Peer]-блоки не трогаются —
// личности пиров переход не меняет.
function confWithGen31(conf: string, headerKey: string): { conf: string; bumpedS: string[] } {
    const peerAt = conf.search(/^\[Peer\]/m);
    let   rest   = peerAt >= 0 ? conf.slice(peerAt) : "";
    const head   = (peerAt >= 0 ? conf.slice(0, peerAt) : conf)
        .split("\n")
        .filter(l => !AWG3_KEY_LINE.test(l));

    // Пустые строки и «# имя» в хвосте [Interface] относятся уже к первому
    // [Peer] (их пишет rebuildConf) — отделяем, чтобы 3.x-строки встали внутрь
    // [Interface], а не между комментарием и его пиром.
    const tail: string[] = [];
    while (head.length && (head[head.length - 1].trim() === "" || head[head.length - 1].trim().startsWith("#")))
        tail.unshift(head.pop()!);
    rest = tail.join("\n").trim() ? `${tail.join("\n").trim()}\n${rest}` : rest;

    let iface = head.join("\n");

    // S ниже 12 (или вовсе отсутствующий) ядро с header protection не примет.
    // Правка безопасна ровно потому, что следом идёт перевыпуск ключей: обе
    // стороны получают новые значения одновременно.
    const bumpedS: string[] = [];
    for (const k of ["S1", "S2", "S3", "S4"] as const) {
        const re  = new RegExp(`^\\s*${k}\\s*=\\s*(\\d+)\\s*$`, "m");
        const cur = Number(iface.match(re)?.[1]);
        if (Number.isFinite(cur) && cur >= MIN_S_FOR_HEADER_PROTECTION) continue;
        const val = Math.max(DEFAULT_AWG_PARAMS[k], MIN_S_FOR_HEADER_PROTECTION);
        iface = re.test(iface)
            ? iface.replace(re, `${k} = ${val}`)
            : `${iface.trimEnd()}\n${k} = ${val}`;
        bumpedS.push(`${k}: ${Number.isFinite(cur) ? cur : "нет"} → ${val}`);
    }

    const awg3 = [
        `HeaderProtectionKey = ${headerKey}`,
        ...Object.entries(AWG31_DEFAULTS).map(([k, v]) => `${k} = ${v}`),
    ].join("\n");

    return { conf: `${iface.trimEnd()}\n${awg3}\n${rest ? `\n${rest}` : ""}`, bumpedS };
}

interface UpgradeResult { gen: "2" | "3.1"; backup: string; bumpedS: string[]; reissue: ReissueResult }

// Перевод интерфейса на 3.1. Единственная настоящая проверка, что ядро приняло
// параметры, — подъём интерфейса, поэтому шаги такие: бэкап конфига → запись
// 3.1 → restartAwg() (он же перечитает параметры) → перевыпуск ключей. Если
// ядро отказало, возвращаем прежний конфиг и поднимаем интерфейс обратно:
// операция либо применяется целиком, либо не оставляет следов.
function upgradeToGen31(): UpgradeResult {
    const confFile = path.join(CONFIG.confDir, `${CONFIG.interface}.conf`);
    const before   = readFileSync(confFile, "utf8");
    const backup   = `${confFile}.bak-${new Date().toISOString().replace(/[:.]/g, "-")}`;
    writeFileSync(backup, before, { mode: 0o600 });

    const { conf, bumpedS } = confWithGen31(before, run("awg genpsk"));
    writeFileSync(confFile, conf, { mode: 0o600 });
    logger.info("upgrade: conf rewritten for 3.1", { backup, bumpedS });

    try {
        restartAwg();
        if (CONFIG.gen !== "3.1") throw new Error("интерфейс поднялся, но конфиг всё ещё 2.0");
    } catch (e) {
        logger.error("upgrade: kernel rejected 3.1, rolling back", { error: e });
        writeFileSync(confFile, before, { mode: 0o600 });
        try { restartAwg(); } catch (e2) { logger.error("upgrade: rollback restart failed", { error: e2 }); }
        throw new Error(
            `Ядро не приняло конфиг 3.1 — вернули прежний (копия: ${backup}). `
            + 'Причина видна в dmesg после `echo "module amneziawg +p" > /sys/kernel/debug/dynamic_debug/control`.',
        );
    }

    // Ключи, выданные на 2.0-параметрах, после смены поколения не подключатся,
    // поэтому перевыпускаем сразу — сервер не должен оставаться в состоянии
    // «3.1 поднят, у всех нерабочие ключи». Личности пиров сохраняются.
    const reissue = reissueAll();
    logger.info("upgrade: done", { gen: CONFIG.gen, reissued: reissue.reissued, bumpedS });
    return { gen: CONFIG.gen, backup, bumpedS, reissue };
}

function startInterface() {
    const status = getInterfaceStatus();
    if (status.up) { logger.info("AWG already up", { peers: status.peers }); return status; }
    const r = spawnSync("awg-quick", ["up", CONFIG.interface]);
    if (r.status !== 0) throw new Error(`awg-quick up failed: ${r.stderr?.toString()}`);
    syncPeers();
    return getInterfaceStatus();
}

// PSK уходит во временный файл, а не в аргументы: иначе он виден в /proc/<pid>/cmdline.
function setPeer(pubKey: string, psk: string, ip: string) {
    const tmpPsk = `/tmp/awg_psk_${Date.now()}.tmp`;
    writeFileSync(tmpPsk, psk, { mode: 0o600 });
    try {
        return spawnSync("awg", [
            "set", CONFIG.interface, "peer", pubKey,
            "preshared-key", tmpPsk,
            "allowed-ips", `${ip}/32`,
            "persistent-keepalive", CONFIG.keepalive,
        ]);
    } finally {
        try { fs.unlinkSync(tmpPsk); } catch {}
    }
}

function addUser(username: string): UserRow {
    const keys      = generateKeys();
    const serverPub = getServerPublicKey();

    const ip = db.transaction(() => {
        const ip = nextIp();
        stmts.insert.run(username, ip, keys.publicKey, "", keys.presharedKey, CONFIG.gen);
        return ip;
    })();

    const vpn_key = encodeVpnKey(keys, ip, serverPub);
    db.prepare("UPDATE users SET vpn_key = ? WHERE name = ?").run(vpn_key, username);

    const r = setPeer(keys.publicKey, keys.presharedKey, ip);
    if (r.status !== 0) {
        stmts.delete.run(username);
        throw new Error(`awg set failed: ${r.stderr?.toString()}`);
    }

    rebuildConf();
    logger.info("user created", { name: username, ip, gen: CONFIG.gen });
    return {
        name: username, ip, pub_key: keys.publicKey, vpn_key,
        psk_key: keys.presharedKey, key_gen: CONFIG.gen, vpn_key_prev: "",
    };
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

// ── Пиры устройств (sen://) ────────────────────────────────────────────────
// awg-ctrl про мастер-ключи ничего не знает: ему приходит «добавь пира с этим
// публичным ключом». Приватного ключа устройства здесь нет и быть не может.
class HttpError extends Error {
    constructor(public status: number, message: string) { super(message); }
}

// Публичный ключ WireGuard: base64 от 32 байт (44 символа, последний «=»).
function checkPubKey(k: unknown): string {
    if (typeof k !== "string" || !/^[A-Za-z0-9+/]{43}=$/.test(k) || Buffer.from(k, "base64").length !== 32)
        throw new HttpError(400, "Неверный публичный ключ");
    return k;
}

// В пути base64url: «/» и «+» обычного base64 в URL ломают маршрут.
function pubKeyFromParam(p: string): string {
    return checkPubKey(Buffer.from(p, "base64url").toString("base64"));
}

function assertPubKeyFree(pub: string) {
    if (stmts.peerGet.get(pub) || stmts.userPubTaken.get(pub)!.n > 0 || pub === getServerPublicKey())
        throw new HttpError(409, "Публичный ключ уже используется");
}

function addPeer(pubKey: string, owner: string): { ip: string; psk_key: string } {
    checkPubKey(pubKey);
    const psk = run("awg genpsk");
    const ip = db.transaction(() => {
        assertPubKeyFree(pubKey);
        const ip = nextIp();
        stmts.peerInsert.run(pubKey, ip, psk, owner, Math.floor(Date.now() / 1000));
        return ip;
    })();

    const r = setPeer(pubKey, psk, ip);
    if (r.status !== 0) {
        stmts.peerDelete.run(pubKey);
        throw new Error(`awg set failed: ${r.stderr?.toString()}`);
    }
    rebuildConf();
    logger.info("peer added", { owner, ip });
    return { ip, psk_key: psk };
}

// Смена ключа устройства: IP и PSK остаются, меняется только pub_key.
function replacePeer(oldPub: string, newPub: string): { ip: string; psk_key: string } {
    checkPubKey(newPub);
    const old = stmts.peerGet.get(oldPub);
    if (!old) throw new HttpError(404, "Пир не найден");
    db.transaction(() => { assertPubKeyFree(newPub); stmts.peerRekey.run(newPub, oldPub); })();

    const r = setPeer(newPub, old.psk_key, old.ip);
    if (r.status !== 0) {
        stmts.peerRekey.run(oldPub, newPub);
        throw new Error(`awg set failed: ${r.stderr?.toString()}`);
    }
    spawnSync("awg", ["set", CONFIG.interface, "peer", oldPub, "remove"]);
    rebuildConf();
    logger.info("peer rekeyed", { owner: old.owner, ip: old.ip });
    return { ip: old.ip, psk_key: old.psk_key };
}

function rotatePeerPsk(pub: string): { ip: string; psk_key: string } {
    const peer = stmts.peerGet.get(pub);
    if (!peer) throw new HttpError(404, "Пир не найден");
    const psk = run("awg genpsk");
    const r = setPeer(pub, psk, peer.ip);
    if (r.status !== 0) throw new Error(`awg set failed: ${r.stderr?.toString()}`);
    stmts.peerPsk.run(psk, pub);
    rebuildConf();
    logger.info("peer psk rotated", { owner: peer.owner });
    return { ip: peer.ip, psk_key: psk };
}

function removePeer(pub: string) {
    const peer = stmts.peerGet.get(pub);
    if (!peer) throw new HttpError(404, "Пир не найден");
    stmts.peerDelete.run(pub);
    spawnSync("awg", ["set", CONFIG.interface, "peer", pub, "remove"]);
    rebuildConf();
    logger.info("peer removed", { owner: peer.owner });
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
        catch (e) {
            if (e instanceof HttpError) { res.status(e.status).json({ error: e.message }); return; }
            logger.error("handler error", { error: e }); res.status(500).json({ error: "Internal server error" });
        }
    };
}

app.get("/health", (_req, res) => {
    const { up, peers } = getInterfaceStatus();
    res.status(up ? 200 : 503).json({
        status: up ? "ok" : "degraded",
        server: CONFIG.serverName,
        ip:     CONFIG.serverIp,
        gen:    CONFIG.gen,
        awg:    { status: up ? "ok" : "down", peers, module: CONFIG.awgModule, tools: CONFIG.awgTools },
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
    const users = db.prepare("SELECT name, ip, pub_key, vpn_key, key_gen FROM users WHERE vpn_key != ''").all() as UserRow[];
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
    const users = db.prepare("SELECT name, ip, pub_key, key_gen FROM users WHERE vpn_key != ''").all() as UserRow[];
    const peers = getPeersData();
    res.json({
        users: users.map(u => ({
            name:          u.name,
            ip:            u.ip,
            key_gen:       u.key_gen,
            online:        peers[u.pub_key]?.online        ?? false,
            lastHandshake: peers[u.pub_key]?.lastHandshake ?? 0,
            rx:            peers[u.pub_key]?.rx            ?? 0,
            tx:            peers[u.pub_key]?.tx            ?? 0,
        })),
    });
}));

// ⚠️ Должен быть объявлен ДО «/api/users/:name», иначе тот перехватит «reissue»
// как имя пользователя.
app.post("/api/users/reissue", auth, handler((_req, res) => {
    res.json(reissueAll());
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

app.post("/api/peers", auth, handler((req, res) => {
    const { pub_key, owner } = (req.body ?? {}) as { pub_key?: string; owner?: string };
    if (typeof owner !== "string" || !/^[\w/.-]{0,64}$/.test(owner)) throw new HttpError(400, "Неверный owner");
    res.status(201).json(addPeer(checkPubKey(pub_key), owner));
}));

app.get("/api/peers", auth, handler((_req, res) => {
    const stats = getPeersData();
    res.json({
        peers: stmts.peersAll.all().map(p => ({
            pub_key: p.pub_key, ip: p.ip, psk_key: p.psk_key, owner: p.owner,
            online:        stats[p.pub_key]?.online        ?? false,
            lastHandshake: stats[p.pub_key]?.lastHandshake ?? 0,
            rx:            stats[p.pub_key]?.rx            ?? 0,
            tx:            stats[p.pub_key]?.tx            ?? 0,
        })),
    });
}));

app.put("/api/peers/:pub", auth, handler((req, res) => {
    const { pub_key } = (req.body ?? {}) as { pub_key?: string };
    res.json(replacePeer(pubKeyFromParam(req.params.pub), checkPubKey(pub_key)));
}));

app.post("/api/peers/:pub/psk", auth, handler((req, res) => {
    res.json(rotatePeerPsk(pubKeyFromParam(req.params.pub)));
}));

app.delete("/api/peers/:pub", auth, handler((req, res) => {
    removePeer(pubKeyFromParam(req.params.pub));
    res.json({ success: true });
}));

// Общая часть клиентского конфига для подписки. Ключи obfuscation — именами .conf;
// пустые (I2–I5, весь 3.x-блок на 2.0) не выводятся, как и в самом conf.
app.get("/api/profile", auth, handler((_req, res) => {
    const awg: Record<string, string> = {};
    for (const [k, v] of Object.entries(CONFIG.awgParams)) if (String(v) !== "") awg[k] = String(v);
    res.json({
        name:      CONFIG.serverName,
        endpoint:  `${CONFIG.serverIp}:${CONFIG.serverPort}`,
        server_pub: getServerPublicKey(),
        gen:       CONFIG.gen,
        dns:       [CONFIG.dns1, CONFIG.dns2],
        keepalive: CONFIG.keepalive,
        mtu:       CONFIG.mtu,
        awg,
    });
}));

app.post("/awg/restart", auth, handler((_req, res) => {
    restartAwg();
    res.json({ success: true });
}));

// Перевод сервера с 2.0 на 3.1 — включая перевыпуск всех vpn:// ключей.
// Ошибку отдаём текстом: панель показывает её оператору, а причины отказа
// (старый модуль, старое ядро, отказ ядра на подъёме) требуют разных действий.
app.post("/awg/upgrade", auth, handler((_req, res) => {
    if (CONFIG.gen === "3.1") {
        res.status(409).json({ error: "Сервер уже работает на AmneziaWG 3.1" }); return;
    }
    const blocker = checkGen31Support();
    if (blocker) { res.status(409).json({ error: blocker }); return; }
    try {
        res.json(upgradeToGen31());
    } catch (e) {
        res.status(500).json({ error: e instanceof Error ? e.message : "Не удалось перейти на 3.1" });
    }
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
