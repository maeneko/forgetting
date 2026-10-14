// Сценарий для awg-ui: одинаковая последовательность запросов к TS- и Go-версии панели.
// Запуск: awg-ui/node_modules/.bin/tsx tools/diffbench/scenario-ui.ts <out.json>
// Окружение: UI (http://127.0.0.1:8080), SUB (http://127.0.0.1:8443), AGENT_BIN — бинарник
// awg-control для ноды (join по строке из панели), BENCH_DIR — каталог с ключами стенда.
import crypto from "crypto";
import fs from "fs";
import { spawn, execSync, type ChildProcess } from "child_process";
import { decodeSenLink, signRequest } from "../../awg-ui/sen";

const UI = process.env.UI ?? "http://127.0.0.1:8080";
const SUB = process.env.SUB ?? "http://127.0.0.1:8443";
const out = process.argv[2];
const BENCH = process.env.BENCH_DIR ?? "/var/lib/uibench";

type Step = { name: string; method: string; path: string; status: number; ctype: string; body: unknown; text?: string };
const steps: Step[] = [];
const sleep = (ms: number) => new Promise(r => setTimeout(r, ms));

async function call(name: string, method: string, path: string, body?: unknown,
    o: { token?: string | null; key?: string; raw?: string; headers?: Record<string, string>; base?: string } = {}) {
    const h: Record<string, string> = { ...(o.headers ?? {}) };
    if (o.token) h.authorization = `Bearer ${o.token}`;
    if (o.key) h["x-api-key"] = o.key;
    let data: string | undefined;
    if (o.raw !== undefined) { h["content-type"] = "application/json"; data = o.raw; }
    else if (body !== undefined) { h["content-type"] = "application/json"; data = JSON.stringify(body); }
    const r = await fetch((o.base ?? UI) + path, { method, headers: h, body: data });
    const ctype = (r.headers.get("content-type") ?? "").split(";")[0];
    const text = await r.text();
    let parsed: unknown = text;
    if (ctype === "application/json") { try { parsed = JSON.parse(text); } catch { /* как есть */ } }
    if (ctype === "text/html" && text.length > 2000) parsed = text.slice(0, 200);   // index.html
    steps.push({ name, method, path, status: r.status, ctype, body: parsed, text: ctype === "application/json" ? text : undefined });
    return parsed as any;
}

// ── Подписанные запросы устройства (/sub/v1) ────────────────────────────────
const seedKey = (s: string) => crypto.createPrivateKey({
    key: Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), crypto.createHash("sha256").update(s).digest()]),
    format: "der", type: "pkcs8",
});
const rawPub = (k: crypto.KeyObject) => (crypto.createPublicKey(k).export({ type: "spki", format: "der" }) as Buffer).subarray(12);
const wgPub = (s: string) => crypto.createHash("sha256").update("wg:" + s).digest("base64");

let tsBump = 0;
async function subCall(name: string, method: string, path: string, body: unknown, auth: crypto.KeyObject | null, device?: number, extra: Record<string, string> = {}) {
    const raw = body === undefined ? "" : JSON.stringify(body);
    const ts = Math.floor(Date.now() / 1000) + (tsBump++ % 2);   // разные подписи для одинаковых запросов
    const h: Record<string, string> = { ...extra };
    if (auth) { h["x-sen-ts"] = String(ts); h["x-sen-sig"] = signRequest(method, path, ts, raw, auth); }
    if (device !== undefined) h["x-sen-device"] = String(device);
    return call(name, method, path, undefined, { raw: body === undefined ? undefined : raw, headers: h, base: SUB });
}

// ── Сценарий ────────────────────────────────────────────────────────────────
async function main() {
let agent: ChildProcess | null = null;
try {
    await call("brand", "GET", "/ui/brand");
    await call("login bad", "POST", "/login", { user: "admin", pass: "nope" });
    await call("login no body", "POST", "/login");
    await call("login broken json", "POST", "/login", undefined, { raw: "{" });
    const { token } = await call("login", "POST", "/login", { user: "admin", pass: "benchpass" });
    const T = { token };

    await call("apikeys no auth", "GET", "/ui/apikeys");
    await call("apikeys bad token", "GET", "/ui/apikeys", undefined, { token: "a.b.c" });
    await call("nodes no auth", "GET", "/ui/nodes");
    await call("proxy no auth", "GET", "/api/users");

    // прокси к awg-ctrl
    await call("proxy health", "GET", "/health", undefined, T);
    await call("proxy users", "GET", "/api/users", undefined, T);
    await call("proxy add alice", "POST", "/api/users", { name: "alice" }, T);
    await call("proxy add bad", "POST", "/api/users", { name: "a b" }, T);
    await call("proxy unknown", "GET", "/api/nope?x=1", undefined, T);
    await call("proxy server 5", "GET", "/api/users", undefined, { ...T, headers: { "x-server-id": "5" } });
    await call("proxy server junk", "GET", "/api/users/stats", undefined, { ...T, headers: { "x-server-id": "junk" } });

    // ключи API
    await call("apikeys empty", "GET", "/ui/apikeys", undefined, T);
    await call("apikey bad label", "POST", "/ui/apikeys", { label: "bad!" }, T);
    await call("apikey no label", "POST", "/ui/apikeys", {}, T);
    await call("apikey bad servers", "POST", "/ui/apikeys", { label: "k", servers: [9] }, T);
    await call("apikey empty servers", "POST", "/ui/apikeys", { label: "k", servers: [] }, T);
    const k1 = await call("apikey create", "POST", "/ui/apikeys", { label: "main key" }, T);
    const k2 = await call("apikey create sid", "POST", "/ui/apikeys", { label: "second", server_id: 0, servers: [0, 0] }, T);
    await call("apikeys list", "GET", "/ui/apikeys", undefined, T);
    await call("apikey patch bad", "PATCH", "/ui/apikeys/" + k2.id, { servers: [] }, T);
    await call("apikey patch missing", "PATCH", "/ui/apikeys/999", { servers: [0] }, T);
    await call("apikey patch", "PATCH", "/ui/apikeys/" + k2.id, { servers: [0] }, T);
    await call("apikey delete bad", "DELETE", "/ui/apikeys/x", undefined, T);
    await call("apikey delete", "DELETE", "/ui/apikeys/" + k2.id, undefined, T);

    // внешний API: пользователи
    const K = { key: k1.key as string };
    await call("v1 no key", "GET", "/api/v1/users");
    await call("v1 bad key", "GET", "/api/v1/users", undefined, { key: "awgk_nope" });
    await call("v1 users", "GET", "/api/v1/users", undefined, K);
    await call("v1 add bob", "POST", "/api/v1/users", { name: "bob" }, K);
    await call("v1 add bob again", "POST", "/api/v1/users", { name: "bob" }, K);
    await call("v1 add no name", "POST", "/api/v1/users", {}, K);
    await call("v1 get bob", "GET", "/api/v1/users/bob", undefined, K);
    await call("v1 get nobody", "GET", "/api/v1/users/nobody", undefined, K);
    await call("v1 get escaped", "GET", "/api/v1/users/b%20b", undefined, K);
    await call("v1 server outside", "GET", "/api/v1/users", undefined, { ...K, headers: { "x-server-id": "3" } });
    await call("v1 unknown with key", "GET", "/api/v1/zzz", undefined, K);
    await call("v1 delete bob", "DELETE", "/api/v1/users/bob", undefined, K);

    // ноды: заводим и подключаем агента к тому же awg-ctrl
    await call("nodes list", "GET", "/ui/nodes", undefined, T);
    await call("node bad name", "POST", "/ui/nodes", { name: "" }, T);
    const n1 = await call("node create", "POST", "/ui/nodes", { name: "edge-1", host: "127.0.0.1" }, T);
    await call("node rename bad", "PATCH", "/ui/nodes/" + n1.id, { name: "a<b" }, T);
    await call("node rename", "PATCH", "/ui/nodes/" + n1.id, { name: "edge-one" }, T);
    await call("node rejoin", "POST", `/ui/nodes/${n1.id}/join`, { host: "127.0.0.1" }, T).then(async r => {
        const j = JSON.parse(Buffer.from(String(r.join).slice("awgjoin://".length), "base64url").toString());
        fs.rmSync(`${BENCH}/agent-node.key`, { force: true });
        agent = spawn(process.env.AGENT_BIN!, ["agent"], {
            env: { ...process.env, CORE_HOST: j.h, CORE_PORT: String(j.p), CORE_PIN: j.pin, NODE_ID: String(j.n), JOIN_SECRET: j.s,
                AWGCTRL_PORT: "3005", NODE_KEY_FILE: `${BENCH}/agent-node.key`, VERSION: "0.9.9",
                INTERNAL_AUTH_KEY_FILE: "/etc/amnezia/amneziawg/internal_auth_private.key", CLI_ENV_FILE: "" },
            stdio: "ignore",
        });
    });
    for (let i = 0; i < 100; i++) {
        const r = await fetch(UI + "/ui/nodes", { headers: { authorization: `Bearer ${token}` } }).then(x => x.json()) as any;
        if (r.nodes.some((n: any) => n.id === n1.id && n.online && n.health)) break;
        await sleep(100);
    }
    await sleep(500);
    await call("nodes online", "GET", "/ui/nodes", undefined, T);
    await call("node join bound", "POST", `/ui/nodes/${n1.id}/join`, {}, T);
    await call("proxy via node", "GET", "/api/users", undefined, { ...T, headers: { "x-server-id": String(n1.id) } });
    await call("proxy via node awg", "GET", "/awg/status", undefined, { ...T, headers: { "x-server-id": String(n1.id) } });

    // мастер-ключи
    await call("mk list empty", "GET", "/ui/masterkeys", undefined, T);
    await call("mk bad label", "POST", "/ui/masterkeys", { label: "a\nb" }, T);
    await call("mk bad limit", "POST", "/ui/masterkeys", { label: "x", device_limit: 0 }, T);
    await call("mk bad servers", "POST", "/ui/masterkeys", { label: "x", servers: [42] }, T);
    const m1 = await call("mk create", "POST", "/ui/masterkeys", { label: "Семья", device_limit: 2 }, T);
    const m2 = await call("mk create multi", "POST", "/ui/masterkeys", { label: "Multi", servers: [n1.id, 0] }, T);
    await call("mk list", "GET", "/ui/masterkeys", undefined, T);
    const link = await call("mk link", "GET", `/ui/masterkeys/${m1.id}/link`, undefined, T);
    await call("mk link by uuid", "GET", `/ui/masterkeys/${m1.uuid.toUpperCase()}/link`, undefined, T);
    await call("mk link missing", "GET", "/ui/masterkeys/999/link", undefined, T);
    const sen = decodeSenLink(link.link);
    const secret = sen.secret.toString("base64url");

    // /sub/v1: регистрация и опрос устройства
    const d1 = seedKey("dev1"), d2 = seedKey("dev2"), d3 = seedKey("dev3");
    const reg = (dev: string, k: crypto.KeyObject, sec: string, extra: object = {}) => ({
        sub: sec, device_id: dev, device_name: "Phone " + dev, platform: "android", pub_key: wgPub(dev),
        auth_pub: rawPub(k).toString("base64url"), version: "1.2.3", ...extra,
    });
    await subCall("sub unknown path", "GET", "/sub/v1/nope", undefined, null);
    await subCall("sub broken json", "POST", "/sub/v1/peek", undefined, null).catch(() => {});
    await call("sub broken json raw", "POST", "/sub/v1/peek", undefined, { raw: "{", base: SUB });
    await subCall("sub peek bad", "POST", "/sub/v1/peek", { sub: "x" }, null);
    await subCall("sub peek missing", "POST", "/sub/v1/peek", { sub: "A".repeat(22) }, null);
    await subCall("sub peek", "POST", "/sub/v1/peek", { sub: secret }, null);
    await subCall("sub register bad", "POST", "/sub/v1/register", { sub: secret }, d1);
    await subCall("sub register unsigned", "POST", "/sub/v1/register", reg("d1", d1, secret), null);
    const r1 = await subCall("sub register d1", "POST", "/sub/v1/register", reg("d1", d1, secret), d1, { "x-sen-device": "77" });
    const dev1 = JSON.parse(r1.body).device;
    const r2 = await subCall("sub register d2", "POST", "/sub/v1/register", reg("d2", d2, secret), d2);
    const dev2 = JSON.parse(r2.body).device;
    await subCall("sub register over limit", "POST", "/sub/v1/register", reg("d3", d3, secret), d3);
    await subCall("sub config", "GET", "/sub/v1/config", undefined, d1, dev1, { "x-sen-version": "1.2.4" });
    await subCall("sub config wrong key", "GET", "/sub/v1/config", undefined, d2, dev1);
    await subCall("sub config no sig", "GET", "/sub/v1/config", undefined, null, dev1);
    await subCall("sub config gone", "GET", "/sub/v1/config", undefined, d1, 9999);
    await subCall("sub devices", "GET", "/sub/v1/devices", undefined, d2, dev2);
    await subCall("sub rekey bad", "POST", "/sub/v1/rekey", { pub_key: "x" }, d1, dev1);
    await subCall("sub rekey same", "POST", "/sub/v1/rekey", { pub_key: wgPub("d1") }, d1, dev1);
    await subCall("sub rekey taken", "POST", "/sub/v1/rekey", { pub_key: wgPub("d2") }, d1, dev1);
    await subCall("sub rekey", "POST", "/sub/v1/rekey", { pub_key: wgPub("d1-new") }, d1, dev1);
    // повтор той же подписи — replay
    {
        const ts = Math.floor(Date.now() / 1000);
        const sig = signRequest("GET", "/sub/v1/config", ts, "", d2);
        const h = { "x-sen-ts": String(ts), "x-sen-sig": sig, "x-sen-device": String(dev2) };
        await call("sub config sig once", "GET", "/sub/v1/config", undefined, { headers: h, base: SUB });
        await call("sub config replay", "GET", "/sub/v1/config", undefined, { headers: h, base: SUB });
    }
    // устройство на ключе с двумя серверами (нода — тот же awg-ctrl)
    const link2 = await call("mk2 link", "GET", `/ui/masterkeys/${m2.id}/link`, undefined, T);
    const secret2 = decodeSenLink(link2.link).secret.toString("base64url");
    const r3 = await subCall("sub register multi", "POST", "/sub/v1/register", reg("d3", d3, secret2), d3);
    const dev3 = JSON.parse(r3.body).device;
    await subCall("sub config multi", "GET", "/sub/v1/config", undefined, d3, dev3);

    // панель: устройства и мастер-ключи
    await call("mk devices", "GET", `/ui/masterkeys/${m1.id}/devices`, undefined, T);
    await call("mk2 devices", "GET", `/ui/masterkeys/${m2.id}/devices`, undefined, T);
    await call("dev rekey", "POST", `/ui/devices/${dev2}/rekey`, undefined, T);
    await call("dev psk", "POST", `/ui/devices/${dev2}/psk`, undefined, T);
    await call("dev missing", "POST", "/ui/devices/999/psk", undefined, T);
    await call("mk rekey", "POST", `/ui/masterkeys/${m1.id}/rekey`, undefined, T);
    await subCall("sub config after rekey flag", "GET", "/sub/v1/config", undefined, d2, dev2);
    await call("mk patch bad", "PATCH", `/ui/masterkeys/${m1.id}`, { device_limit: 101 }, T);
    await call("mk patch limit", "PATCH", `/ui/masterkeys/${m1.id}`, { label: "Семья 2", device_limit: 1 }, T);
    await subCall("sub config unbound", "GET", "/sub/v1/config", undefined, d2, dev2);
    await call("mk2 patch servers", "PATCH", `/ui/masterkeys/${m2.id}`, { servers: [0] }, T);
    await call("mk rotate", "POST", `/ui/masterkeys/${m1.id}/rotate`, undefined, T);
    await subCall("sub peek old secret", "POST", "/sub/v1/peek", { sub: secret }, null);
    await call("dev delete", "DELETE", `/ui/devices/${dev1}`, undefined, T);
    await subCall("sub config deleted", "GET", "/sub/v1/config", undefined, d1, dev1);
    await subCall("sub delete self", "DELETE", "/sub/v1/device", undefined, d3, dev3);

    // внешний API: мастер-ключи
    await call("v1 mk list", "GET", "/api/v1/masterkeys", undefined, K);
    await call("v1 mk create outside", "POST", "/api/v1/masterkeys", { label: "ext", servers: [n1.id] }, K);
    const em = await call("v1 mk create", "POST", "/api/v1/masterkeys", { label: "ext" }, K);
    await call("v1 mk get", "GET", `/api/v1/masterkeys/${em.id}`, undefined, K);
    await call("v1 mk get by uuid", "GET", `/api/v1/masterkeys/${em.uuid}`, undefined, K);
    await call("v1 mk get invisible", "GET", `/api/v1/masterkeys/${m2.id}`, undefined, K);
    await call("v1 mk patch", "PATCH", `/api/v1/masterkeys/${em.id}`, { device_limit: 5 }, K);
    await call("v1 mk rotate", "POST", `/api/v1/masterkeys/${em.id}/rotate`, undefined, K);
    await call("v1 mk rekey", "POST", `/api/v1/masterkeys/${em.id}/rekey`, undefined, K);
    await call("v1 mk devices", "GET", `/api/v1/masterkeys/${em.id}/devices`, undefined, K);
    await call("v1 mk device missing", "DELETE", `/api/v1/masterkeys/${em.id}/devices/${dev2}`, undefined, K);
    await call("v1 mk delete", "DELETE", `/api/v1/masterkeys/${em.id}`, undefined, K);

    // аудит
    await call("audit get", "GET", "/ui/masterkeys/audit", undefined, T);
    await call("audit settings bad", "PUT", "/ui/masterkeys/audit/settings", { every_h: 0 }, T);
    await call("audit settings", "PUT", "/ui/masterkeys/audit/settings", { clean: false, every_h: 2, max: 5 }, T);
    await call("audit run", "POST", "/ui/masterkeys/audit", undefined, T);
    await call("audit clean", "POST", "/ui/masterkeys/audit/clean", undefined, T);

    // метрики
    await call("metrics no token", "GET", "/metrics");
    await call("metrics token none", "GET", "/ui/metrics-token", undefined, T);
    const mt = await call("metrics token create", "POST", "/ui/metrics-token", undefined, T);
    await call("metrics", "GET", "/metrics", undefined, { token: mt.token });
    await call("metrics token view", "GET", "/ui/metrics-token", undefined, T);
    await call("metrics token delete", "DELETE", "/ui/metrics-token", undefined, T);
    await call("metrics after delete", "GET", "/metrics", undefined, { token: mt.token });

    // монитор (Prometheus не запущен)
    await call("monitor", "GET", "/ui/monitor", undefined, T);
    await call("monitor overview bad", "GET", "/ui/monitor/overview?range=2h", undefined, T);
    await call("monitor overview", "GET", "/ui/monitor/overview?range=6h", undefined, T);

    // аудит с «мёртвыми»: лишний пир панели у awg-ctrl и пир в ядре без записи
    {
        const panel = fs.readFileSync(__dirname + "/../../awg-ui/panel.id", "utf8").trim();
        await call("orphan add", "POST", "/api/peers", { pub_key: wgPub("orphan"), owner: `${panel}/m1/d77` }, T);
        await call("foreign add", "POST", "/api/peers", { pub_key: wgPub("foreign"), owner: "ffffffffffff/m1/d1" }, T);
        execSync(`awg set awg1 peer ${wgPub("ghost")} allowed-ips 10.9.9.9/32`);
        await call("audit run 1", "POST", "/ui/masterkeys/audit", undefined, T);
        await call("audit clean unconfirmed", "POST", "/ui/masterkeys/audit/clean", undefined, T);
        await call("audit run 2", "POST", "/ui/masterkeys/audit", undefined, T);
        await call("audit clean confirmed", "POST", "/ui/masterkeys/audit/clean", undefined, T);
        await call("audit after clean", "GET", "/ui/masterkeys/audit", undefined, T);
    }

    // ключ API на ноде мешает её удалить; потом нода уходит офлайн
    const k3 = await call("apikey on node", "POST", "/ui/apikeys", { label: "node only", servers: [n1.id] }, T);
    await call("node delete key veto", "DELETE", `/ui/nodes/${n1.id}`, undefined, T);
    await call("apikey to both", "PATCH", "/ui/apikeys/" + k3.id, { servers: [0, n1.id] }, T);
    await call("v1 users on node", "GET", "/api/v1/users", undefined, { key: k3.key, headers: { "x-server-id": String(n1.id) } });
    if (agent) { (agent as ChildProcess).kill(); agent = null; }
    for (let i = 0; i < 50; i++) {
        const r = await fetch(UI + "/ui/nodes", { headers: { authorization: `Bearer ${token}` } }).then(x => x.json()) as any;
        if (r.nodes.some((n: any) => n.id === n1.id && !n.online)) break;
        await sleep(100);
    }
    await call("nodes offline", "GET", "/ui/nodes", undefined, T);
    await call("proxy via offline node", "GET", "/api/users", undefined, { ...T, headers: { "x-server-id": String(n1.id) } });
    await call("mk2 back to node", "PATCH", `/ui/masterkeys/${m2.id}`, { servers: [0, n1.id] }, T);
    await call("mk2 devices offline", "GET", `/ui/masterkeys/${m2.id}/devices`, undefined, T);

    // удаление ноды
    await call("node delete in use", "DELETE", `/ui/nodes/${n1.id}`, undefined, T);
    await call("mk2 delete", "DELETE", `/ui/masterkeys/${m2.id}`, undefined, T);
    await call("node delete pending", "DELETE", `/ui/nodes/${n1.id}`, undefined, T);
    await call("node delete", "DELETE", `/ui/nodes/${n1.id}?force=1`, undefined, T);
    await call("apikeys after node delete", "GET", "/ui/apikeys", undefined, T);
    await call("node delete again", "DELETE", `/ui/nodes/${n1.id}`, undefined, T);
    await call("mk list final", "GET", "/ui/masterkeys", undefined, T);

    // статика и общие ответы
    await call("static index", "GET", "/");
    await call("static spa", "GET", "/some/route");
    await call("static ui fallback", "GET", "/ui/unknown", undefined, T);
    await call("post unknown", "POST", "/nope");
    await call("wrong method", "PUT", "/ui/apikeys", undefined, T);
    await call("logout", "POST", "/logout", undefined, T);
    await call("after logout", "GET", "/ui/apikeys", undefined, T);
} finally {
    if (agent) (agent as ChildProcess).kill();
}

fs.writeFileSync(out, JSON.stringify({ steps }, null, 1));
console.log(`scenario-ui: ${steps.length} steps → ${out}`);
}

main().catch(e => { console.error(e); fs.writeFileSync(out, JSON.stringify({ steps, error: String(e) }, null, 1)); process.exit(1); });
