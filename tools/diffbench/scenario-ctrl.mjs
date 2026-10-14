// Сценарий для awg-ctrl: одинаковая последовательность запросов к любой реализации.
// Пишет транскрипт (запрос → статус, тип, тело) в JSON; collect.mjs дописывает состояние.
import fs from "fs";
import crypto from "crypto";
import { execSync } from "child_process";

const BASE = "http://127.0.0.1:3005";
const KEY = crypto.createPrivateKey(fs.readFileSync("/etc/amnezia/amneziawg/internal_auth_private.key"));
const out = process.argv[2];

function token(iss = "bench") {
    const h = Buffer.from(JSON.stringify({ alg: "EdDSA", typ: "JWT" })).toString("base64url");
    const t = Math.floor(Date.now() / 1000);
    const p = Buffer.from(JSON.stringify({ iss, iat: t, exp: t + 60 })).toString("base64url");
    return `${h}.${p}.${crypto.sign(null, Buffer.from(`${h}.${p}`), KEY).toString("base64url")}`;
}
const key = s => crypto.createHash("sha256").update(s).digest("base64");
const url64 = k => Buffer.from(k, "base64").toString("base64url");
const P1 = key("dev1"), P2 = key("dev2"), P3 = key("dev3"), GHOST = key("ghost");

const steps = [];
async function call(name, method, path, body, { auth = true, raw, headers = {} } = {}) {
    const h = { ...headers };
    if (auth) h.authorization = `Bearer ${typeof auth === "string" ? auth : token()}`;
    let data;
    if (raw !== undefined) { h["content-type"] = "application/json"; data = raw; }
    else if (body !== undefined) { h["content-type"] = "application/json"; data = JSON.stringify(body); }
    const r = await fetch(BASE + path, { method, headers: h, body: data });
    const ctype = (r.headers.get("content-type") ?? "").split(";")[0];
    const text = await r.text();
    let parsed = text;
    if (ctype === "application/json") { try { parsed = JSON.parse(text); } catch {} }
    steps.push({ name, method, path, status: r.status, ctype, body: parsed, text: ctype === "application/json" ? text : undefined });
    return parsed;
}

const gen = process.env.GEN ?? "2";

await call("health", "GET", "/health", undefined, { auth: false });
await call("no token", "GET", "/api/users", undefined, { auth: false });
await call("bad token", "GET", "/api/users", undefined, { auth: "a.b.c" });
await call("expired token", "GET", "/api/users", undefined, { auth: (() => {
    const h = Buffer.from(JSON.stringify({ alg: "EdDSA", typ: "JWT" })).toString("base64url");
    const p = Buffer.from(JSON.stringify({ iss: "x", iat: 1, exp: 2 })).toString("base64url");
    return `${h}.${p}.${crypto.sign(null, Buffer.from(`${h}.${p}`), KEY).toString("base64url")}`;
})() });
await call("users empty", "GET", "/api/users");
await call("add alice", "POST", "/api/users", { name: "alice" });
await call("add bob", "POST", "/api/users", { name: "bob" });
await call("add alice again", "POST", "/api/users", { name: "alice" });
await call("add bad name", "POST", "/api/users", { name: "a b" });
await call("add long name", "POST", "/api/users", { name: "x".repeat(33) });
await call("add no body", "POST", "/api/users");
await call("add too big", "POST", "/api/users", undefined, { raw: JSON.stringify({ name: "y", pad: "z".repeat(2000) }) });
await call("add broken json", "POST", "/api/users", undefined, { raw: "{\"name\":" });
await call("users", "GET", "/api/users");
await call("stats", "GET", "/api/users/stats");
await call("fetch alice", "POST", "/api/users/alice");
await call("fetch nobody", "POST", "/api/users/nobody");
await call("fetch bad name", "POST", "/api/users/a%20b");
await call("add peer", "POST", "/api/peers", { pub_key: P1, owner: "abcdef123456/m1/d1" });
await call("add peer dup", "POST", "/api/peers", { pub_key: P1, owner: "abcdef123456/m1/d1" });
await call("add peer bad owner", "POST", "/api/peers", { pub_key: P2, owner: "bad owner!" });
await call("add peer bad key", "POST", "/api/peers", { pub_key: "nope", owner: "abcdef123456/m1/d2" });
await call("add peer legacy", "POST", "/api/peers", { pub_key: P3, owner: "m7/d9" });
await call("peers", "GET", "/api/peers");
await call("rekey peer", "PUT", `/api/peers/${url64(P1)}`, { pub_key: P2 });
await call("rekey missing", "PUT", `/api/peers/${url64(key("missing"))}`, { pub_key: key("other") });
await call("rekey to taken", "PUT", `/api/peers/${url64(P2)}`, { pub_key: P3 });
await call("rotate psk", "POST", `/api/peers/${url64(P2)}/psk`);
await call("rotate psk missing", "POST", `/api/peers/${url64(key("missing"))}/psk`);
await call("profile", "GET", "/api/profile");
await call("add peer 4", "POST", "/api/peers", { pub_key: key("dev4"), owner: "abcdef123456/m1/d4" });
await call("sync bad owner", "PUT", "/api/peers/sync", { owner: "BAD", keep: [] });
await call("sync bad keep", "PUT", "/api/peers/sync", { owner: "abcdef123456", keep: "x" });
await call("sync keep P2 + legacy P3", "PUT", "/api/peers/sync", { owner: "abcdef123456", keep: [P2, P3] });
await call("peers after sync", "GET", "/api/peers");
execSync(`awg set awg1 peer ${GHOST} allowed-ips 10.9.9.9/32`);
await call("audit", "GET", "/api/audit");
await call("audit clean bad", "POST", "/api/audit/clean", { pub_keys: "x" });
await call("audit clean", "POST", "/api/audit/clean", { pub_keys: [GHOST, P2] });
await call("delete peer", "DELETE", `/api/peers/${url64(P2)}`);
await call("delete peer missing", "DELETE", `/api/peers/${url64(P2)}`);
await call("delete bob", "DELETE", "/api/users/bob");
await call("delete nobody", "DELETE", "/api/users/nobody");
await call("shaping", "GET", "/api/shaping");
await call("shaping bad", "PUT", "/api/shaping", { enabled: "yes", auto: true });
await call("shaping bad mbit", "PUT", "/api/shaping", { enabled: true, auto: true, downMbit: 1.5, upMbit: 10 });
await call("shaping missing width", "PUT", "/api/shaping", { enabled: true, auto: true, downMbit: 0, upMbit: 10 });
await call("shaping on", "PUT", "/api/shaping", { enabled: true, auto: false, downMbit: 285, upMbit: 280 });
await call("shaping get on", "GET", "/api/shaping");
await call("stats priority", "GET", "/api/users/stats");
await call("metrics", "GET", "/api/metrics");
await call("shaping off", "PUT", "/api/shaping", { enabled: false, auto: true, downMbit: 0, upMbit: 0 });
await call("monitor job", "GET", "/api/monitor/job");
await call("monitor install (no cli)", "POST", "/api/monitor/install");
await call("awg status", "GET", "/awg/status");
await call("awg start (already up)", "POST", "/awg/start");
await call("reissue", "POST", "/api/users/reissue");
await call("users after reissue", "GET", "/api/users");
await call("awg restart", "POST", "/awg/restart");
await call("health after restart", "GET", "/health", undefined, { auth: false });
await call("upgrade", "POST", "/awg/upgrade");
await call("profile after upgrade", "GET", "/api/profile");
await call("users after upgrade", "GET", "/api/users");
await call("fetch alice after upgrade", "POST", "/api/users/alice");
await call("health final", "GET", "/health", undefined, { auth: false });
await call("unknown route", "GET", "/api/nope");
await call("wrong method", "PATCH", "/api/users");

fs.writeFileSync(out, JSON.stringify({ gen, steps }, null, 1));
console.log(`scenario: ${steps.length} steps → ${out}`);
