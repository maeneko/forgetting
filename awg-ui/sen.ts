// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// sen:// — ссылка-подписка для SenAWG и подписи протокола /sub/v1.
// Байтовая раскладка и схема подписей описаны в docs/sen-link.md; клиент
// реализуется по ней, поэтому любое изменение здесь — изменение протокола.
import crypto from "crypto";

export const SEN_PREFIX  = "sen://";
export const SEN_VERSION = 1;
export const FLAG_TLS    = 0x01;

const ADDR_V4     = 0x04;
const ADDR_V6     = 0x06;
const ADDR_DOMAIN = 0x44;

export class SenLinkError extends Error {}

export interface SenAddr { host: string; port: number }

export interface SenLink {
    tls: boolean;
    addrs: SenAddr[];
    secret: Buffer;   // 16 байт
    signPub: Buffer;  // 32 байта, Ed25519
    tlsPin?: Buffer;  // 32 байта, только при tls
    name: string;
}

// ── CRC32 (IEEE) ───────────────────────────────────────────────────────────
// zlib.crc32 появился только в Node 20.15, а целимся в 20.x целиком.
const CRC_TABLE = (() => {
    const t = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
        let c = n;
        for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
        t[n] = c >>> 0;
    }
    return t;
})();

export function crc32(buf: Uint8Array): number {
    let c = 0xffffffff;
    for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
}

// ── Адреса ─────────────────────────────────────────────────────────────────
function ipv6ToBytes(host: string): Buffer | null {
    const parts = host.split("::");
    if (parts.length > 2) return null;
    const head = parts[0] ? parts[0].split(":") : [];
    const tail = parts.length === 2 && parts[1] ? parts[1].split(":") : [];
    const missing = 8 - head.length - tail.length;
    if (parts.length === 1 ? head.length !== 8 : missing < 1) return null;
    const groups = [...head, ...Array(parts.length === 2 ? missing : 0).fill("0"), ...tail];
    if (groups.length !== 8) return null;
    const out = Buffer.alloc(16);
    for (let i = 0; i < 8; i++) {
        if (!/^[0-9a-fA-F]{1,4}$/.test(groups[i])) return null;
        out.writeUInt16BE(parseInt(groups[i], 16), i * 2);
    }
    return out;
}

function bytesToIpv6(b: Buffer): string {
    const g: string[] = [];
    for (let i = 0; i < 16; i += 2) g.push(b.readUInt16BE(i).toString(16));
    // Самый длинный run нулей сворачиваем в «::» — как принято в текстовой записи.
    let bestStart = -1, bestLen = 0;
    for (let i = 0; i < 8;) {
        if (g[i] !== "0") { i++; continue; }
        let j = i;
        while (j < 8 && g[j] === "0") j++;
        if (j - i > bestLen) { bestStart = i; bestLen = j - i; }
        i = j;
    }
    if (bestLen < 2) return g.join(":");
    return g.slice(0, bestStart).join(":") + "::" + g.slice(bestStart + bestLen).join(":");
}

function encodeAddr({ host, port }: SenAddr): Buffer {
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new SenLinkError(`Неверный порт: ${port}`);
    const p = Buffer.alloc(2);
    p.writeUInt16BE(port);
    if (/^\d{1,3}(\.\d{1,3}){3}$/.test(host)) {
        const o = host.split(".").map(Number);
        if (o.some(n => n > 255)) throw new SenLinkError(`Неверный IPv4: ${host}`);
        return Buffer.concat([Buffer.from([ADDR_V4, ...o]), p]);
    }
    if (host.includes(":")) {
        const b = ipv6ToBytes(host);
        if (!b) throw new SenLinkError(`Неверный IPv6: ${host}`);
        return Buffer.concat([Buffer.from([ADDR_V6]), b, p]);
    }
    const h = Buffer.from(host, "utf8");
    if (h.length < 1 || h.length > 253 || !/^[A-Za-z0-9.-]+$/.test(host))
        throw new SenLinkError(`Неверный домен: ${host}`);
    return Buffer.concat([Buffer.from([ADDR_DOMAIN, h.length]), h, p]);
}

// ── Ссылка ─────────────────────────────────────────────────────────────────
export function encodeSenLink(l: SenLink): string {
    if (l.addrs.length < 1 || l.addrs.length > 3) throw new SenLinkError("Адресов должно быть от 1 до 3");
    if (l.secret.length !== 16) throw new SenLinkError("secret: 16 байт");
    if (l.signPub.length !== 32) throw new SenLinkError("signPub: 32 байта");
    if (l.tls && l.tlsPin?.length !== 32) throw new SenLinkError("tlsPin: 32 байта при TLS");
    const name = Buffer.from(l.name, "utf8");
    if (name.length > 64) throw new SenLinkError("Имя: до 64 байт");

    const body = Buffer.concat([
        Buffer.from([SEN_VERSION, l.tls ? FLAG_TLS : 0, l.addrs.length]),
        ...l.addrs.map(encodeAddr),
        l.secret,
        l.signPub,
        ...(l.tls ? [l.tlsPin as Buffer] : []),
        Buffer.from([name.length]),
        name,
    ]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body));
    return SEN_PREFIX + Buffer.concat([body, crc]).toString("base64url");
}

export function decodeSenLink(link: string): SenLink {
    const t = link.trim();
    if (!t.toLowerCase().startsWith(SEN_PREFIX)) throw new SenLinkError("Ссылка должна начинаться с sen://");
    const payload = t.slice(SEN_PREFIX.length).replace(/\s+/g, "");
    if (!/^[A-Za-z0-9_-]+$/.test(payload)) throw new SenLinkError("Ссылка повреждена: недопустимые символы");
    const raw = Buffer.from(payload, "base64url");
    if (raw.length < 3 + 4) throw new SenLinkError("Ссылка слишком короткая");

    const body = raw.subarray(0, raw.length - 4);
    if (raw.readUInt32BE(raw.length - 4) !== crc32(body)) throw new SenLinkError("Ссылка повреждена: не сошлась контрольная сумма");

    let off = 0;
    const need = (n: number) => { if (off + n > body.length) throw new SenLinkError("Ссылка обрезана"); };
    const take = (n: number) => { need(n); const s = body.subarray(off, off + n); off += n; return s; };

    const version = take(1)[0];
    if (version !== SEN_VERSION) throw new SenLinkError(`Неизвестная версия ссылки: ${version}`);
    const flags = take(1)[0];
    if (flags & ~FLAG_TLS) throw new SenLinkError("Неизвестные флаги ссылки");
    const n = take(1)[0];
    if (n < 1 || n > 3) throw new SenLinkError("Неверное число адресов");

    const addrs: SenAddr[] = [];
    for (let i = 0; i < n; i++) {
        const type = take(1)[0];
        let host: string;
        if (type === ADDR_V4)      host = [...take(4)].join(".");
        else if (type === ADDR_V6) host = bytesToIpv6(Buffer.from(take(16)));
        else if (type === ADDR_DOMAIN) {
            const len = take(1)[0];
            if (len < 1) throw new SenLinkError("Пустой домен");
            host = Buffer.from(take(len)).toString("utf8");
            if (!/^[A-Za-z0-9.-]+$/.test(host)) throw new SenLinkError("Неверный домен");
        } else throw new SenLinkError(`Неизвестный тип адреса: ${type}`);
        const port = Buffer.from(take(2)).readUInt16BE(0);
        if (port === 0) throw new SenLinkError("Порт 0");
        addrs.push({ host, port });
    }

    const secret  = Buffer.from(take(16));
    const signPub = Buffer.from(take(32));
    const tls     = (flags & FLAG_TLS) !== 0;
    const tlsPin  = tls ? Buffer.from(take(32)) : undefined;
    const nameLen = take(1)[0];
    if (nameLen > 64) throw new SenLinkError("Имя длиннее 64 байт");
    const name = Buffer.from(take(nameLen)).toString("utf8");
    if (off !== body.length) throw new SenLinkError("Лишние данные в конце ссылки");

    return { tls, addrs, secret, signPub, tlsPin, name };
}

// ── Ключи Ed25519 в «сыром» виде (32 байта) ────────────────────────────────
const SPKI_ED25519_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

export function rawPublicKey(key: crypto.KeyObject): Buffer {
    const der = key.export({ type: "spki", format: "der" }) as Buffer;
    return der.subarray(der.length - 32);
}

export function publicKeyFromRaw(raw: Buffer): crypto.KeyObject {
    if (raw.length !== 32) throw new SenLinkError("Публичный ключ Ed25519: 32 байта");
    return crypto.createPublicKey({ key: Buffer.concat([SPKI_ED25519_PREFIX, raw]), format: "der", type: "spki" });
}

/** SHA-256 от SPKI DER сертификата — то, что кладётся в ссылку как pin. */
export function spkiPin(certPem: string): Buffer {
    const spki = new crypto.X509Certificate(certPem).publicKey.export({ type: "spki", format: "der" }) as Buffer;
    return crypto.createHash("sha256").update(spki).digest();
}

// ── Подписи протокола ──────────────────────────────────────────────────────
// Ответ: {"body": "<JSON-строка>", "sig": "<base64url>"}; sig — Ed25519 над
// UTF-8 байтами строки body. body — именно строка, а не вложенный объект,
// чтобы клиенту не нужна была каноническая сериализация JSON.
export function signResponse(payload: unknown, signKey: crypto.KeyObject): { body: string; sig: string } {
    const body = JSON.stringify(payload);
    return { body, sig: crypto.sign(null, Buffer.from(body, "utf8"), signKey).toString("base64url") };
}

export function verifyResponse(res: { body: string; sig: string }, signPub: Buffer): boolean {
    try {
        return crypto.verify(null, Buffer.from(res.body, "utf8"), publicKeyFromRaw(signPub), Buffer.from(res.sig, "base64url"));
    } catch { return false; }
}

// Запрос: подпись ключом устройства (auth_pub) над строкой
//   METHOD \n /path \n ts \n hex(sha256(тело запроса))
// Тело пустое → sha256 пустой строки. path — без query.
export function requestSigString(method: string, urlPath: string, ts: number, body: Buffer | string): string {
    const h = crypto.createHash("sha256").update(body).digest("hex");
    return `${method.toUpperCase()}\n${urlPath}\n${ts}\n${h}`;
}

export function signRequest(
    method: string, urlPath: string, ts: number, body: Buffer | string, authKey: crypto.KeyObject,
): string {
    return crypto.sign(null, Buffer.from(requestSigString(method, urlPath, ts, body), "utf8"), authKey).toString("base64url");
}

export function verifyRequest(
    method: string, urlPath: string, ts: number, body: Buffer | string, sig: string, authPub: Buffer,
): boolean {
    try {
        return crypto.verify(
            null, Buffer.from(requestSigString(method, urlPath, ts, body), "utf8"),
            publicKeyFromRaw(authPub), Buffer.from(sig, "base64url"),
        );
    } catch { return false; }
}
