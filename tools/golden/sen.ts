// Эталон для internal/ui (sen.go): ссылки sen:// и подписи протокола, как их делает awg-ui/sen.ts.
// Запуск: awg-ui/node_modules/.bin/tsx tools/golden/sen.ts > testdata/golden/sen.json
import crypto from "crypto";
import {
    encodeSenLink, decodeSenLink, crc32, requestSigString, signResponse, signRequest, rawPublicKey,
    type SenLink,
} from "../../awg-ui/sen";

const seed = (s: string) => crypto.createHash("sha256").update(s).digest();
// Ed25519 из фиксированного seed: PKCS#8 = префикс + 32 байта seed.
const keyFromSeed = (s: Buffer) => crypto.createPrivateKey({
    key: Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), s]), format: "der", type: "pkcs8",
});
const signKey = keyFromSeed(seed("sign"));
const signPub = rawPublicKey(crypto.createPublicKey(signKey));
const hex = (b?: Buffer) => b?.toString("hex");

const links: SenLink[] = [
    { tls: false, addrs: [{ host: "203.0.113.7", port: 8443 }], secret: seed("s1").subarray(0, 16), signPub, name: "Семья" },
    { tls: true, addrs: [{ host: "vpn.example.com", port: 443 }], secret: seed("s2").subarray(0, 16), signPub, tlsPin: seed("pin"), name: "" },
    { tls: false, addrs: [{ host: "2001:db8::1", port: 1 }, { host: "::", port: 65535 }, { host: "fe80:0:0:1:0:0:0:1", port: 80 }],
      secret: seed("s3").subarray(0, 16), signPub, name: "x".repeat(64) },
    { tls: false, addrs: [{ host: "1:2:3:4:5:6:7:8", port: 2 }], secret: seed("s4").subarray(0, 16), signPub, name: "emoji 😀 & <>" },
];
const badLinks = [
    { tls: false, addrs: [], secret: seed("a").subarray(0, 16), signPub, name: "" },
    { tls: false, addrs: [{ host: "256.1.1.1", port: 1 }], secret: seed("a").subarray(0, 16), signPub, name: "" },
    { tls: false, addrs: [{ host: "1::2::3", port: 1 }], secret: seed("a").subarray(0, 16), signPub, name: "" },
    { tls: false, addrs: [{ host: "bad_host", port: 1 }], secret: seed("a").subarray(0, 16), signPub, name: "" },
    { tls: false, addrs: [{ host: "a.b", port: 0 }], secret: seed("a").subarray(0, 16), signPub, name: "" },
    { tls: false, addrs: [{ host: "a.b", port: 1 }], secret: seed("a").subarray(0, 16), signPub, name: "я".repeat(33) },
    { tls: true, addrs: [{ host: "a.b", port: 1 }], secret: seed("a").subarray(0, 16), signPub, name: "" },
];
const good = links.map(encodeSenLink);
const decodeCases = [
    ...good, "  " + good[0].toUpperCase().slice(0, 6) + good[0].slice(6) + " \n", good[1].slice(0, 20) + "\n" + good[1].slice(20),
    "http://x", "sen://", "sen://AAAA", "sen://a+b", good[0].slice(0, -2), good[0].slice(0, -1) + (good[0].endsWith("A") ? "B" : "A"),
];
const err = (f: () => unknown) => { try { return { ok: f() }; } catch (e) { return { error: (e as Error).message }; } };

process.stdout.write(JSON.stringify({
    sign_seed: hex(seed("sign")),
    sign_pub: hex(signPub),
    crc32: ["", "a", "hello", "кириллица"].map(s => ({ in: s, out: crc32(Buffer.from(s)) })),
    encode: links.map((l, i) => ({
        tls: l.tls, addrs: l.addrs, secret: hex(l.secret), tlsPin: hex(l.tlsPin), name: l.name, out: good[i],
    })),
    encode_errors: badLinks.map(l => ({
        tls: l.tls, addrs: l.addrs, secret: hex(l.secret), name: l.name, error: err(() => encodeSenLink(l as SenLink)).error,
    })),
    decode: decodeCases.map(s => {
        const r = err(() => decodeSenLink(s));
        if ("error" in r) return { in: s, error: r.error };
        const d = r.ok as SenLink;
        return { in: s, tls: d.tls, addrs: d.addrs, secret: hex(d.secret), signPub: hex(d.signPub), tlsPin: hex(d.tlsPin) ?? null, name: d.name };
    }),
    sig_string: [
        { method: "post", path: "/sub/v1/register", ts: 1700000000, body: '{"a":1}' },
        { method: "GET", path: "/sub/v1/config", ts: 1, body: "" },
    ].map(c => ({ ...c, out: requestSigString(c.method, c.path, c.ts, c.body),
        sig: signRequest(c.method, c.path, c.ts, c.body, signKey) })),
    sign_response: [{ config: { rev: "ab", servers: [] }, device: 3, ts: 5 }, { error: "revoked" }].map(p => ({ in: p, out: signResponse(p, signKey) })),
}, null, 1));
