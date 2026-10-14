// Заглушки системных команд для дифференциального стенда: awg, awg-quick, ip, tc, nft, modinfo.
// Детерминированные ключи (счётчик), состояние интерфейса в JSON, журнал каждого вызова.
// Одинаково ведут себя для TS- и Go-версии — сравнивается то, что версии с ними делают.
import fs from "fs";
import crypto from "crypto";

const DIR = "/var/lib/stub";
const STATE = `${DIR}/state.json`;
const LOG = `${DIR}/log.jsonl`;
const [, , cmd, ...args] = process.argv;

const load = () => { try { return JSON.parse(fs.readFileSync(STATE, "utf8")); } catch { return { up: false, n: 0, peers: {} }; } };
const save = s => fs.writeFileSync(STATE, JSON.stringify(s, null, 1));
const b64 = s => crypto.createHash("sha256").update(s).digest("base64");
const pubOf = priv => b64("pub:" + priv.trim());
const readStdin = () => { try { return fs.readFileSync(0, "utf8"); } catch { return ""; } };

let stdin = "";
const needsStdin = (cmd === "awg" && args[0] === "pubkey") || (cmd === "nft" && args.includes("-"));
if (needsStdin) stdin = readStdin();

const entry = { cmd, args };
if (stdin) entry.stdin = stdin;
// файлы, которые передают по пути (PSK, syncconf), пишем в журнал содержимым: имя у них со временем
const fileArgs = [];
for (let i = 0; i < args.length; i++) {
    if (["preshared-key", "private-key"].includes(args[i]) || (cmd === "awg" && args[0] === "syncconf" && i === 2)) {
        try { fileArgs.push(fs.readFileSync(args[i + (args[0] === "syncconf" ? 0 : 1)], "utf8")); } catch { fileArgs.push("<unreadable>"); }
    }
}
if (fileArgs.length) entry.files = fileArgs;
fs.appendFileSync(LOG, JSON.stringify(entry) + "\n");

const out = s => process.stdout.write(s);
const st = load();

function parsePeers(text) {
    const peers = {};
    for (const block of text.split(/^\[Peer\]/m).slice(1)) {
        const g = k => block.match(new RegExp(`^\\s*${k}\\s*=\\s*(.*)$`, "m"))?.[1]?.trim();
        const pub = g("PublicKey");
        if (pub) peers[pub] = { psk: g("PresharedKey") ?? "", allowed: g("AllowedIPs") ?? "", ka: g("PersistentKeepalive") ?? "" };
    }
    return peers;
}

function serverPub() {
    try { return pubOf(fs.readFileSync("/etc/amnezia/server_private.key", "utf8")); } catch { return "(none)"; }
}

switch (cmd) {
    case "awg": {
        const [sub, iface, ...rest] = args;
        if (sub === "--version") { out("awg-tools v1.0.20210914 - https://amnezia.org\n"); break; }
        if (sub === "genkey") { st.n++; save(st); out(b64("priv" + st.n) + "\n"); break; }
        if (sub === "genpsk") { st.n++; save(st); out(b64("psk" + st.n) + "\n"); break; }
        if (sub === "pubkey") { out(pubOf(stdin) + "\n"); break; }
        if (sub === "show") {
            if (!st.up) { process.stderr.write(`Unable to access interface: No such device\n`); process.exit(1); }
            const pubs = Object.keys(st.peers);
            if (rest[0] === "dump") {
                out(`(hidden)\t${serverPub()}\t47619\toff\n`);
                for (const p of pubs) out(`${p}\t(hidden)\t(none)\t${st.peers[p].allowed}\t0\t0\t0\t${st.peers[p].ka || "off"}\n`);
            } else if (rest[0] === "peers") {
                for (const p of pubs) out(p + "\n");
            } else {
                out(`interface: ${iface}\n  public key: ${serverPub()}\n  private key: (hidden)\n  listening port: 47619\n`);
                for (const p of pubs) out(`\npeer: ${p}\n  preshared key: (hidden)\n  allowed ips: ${st.peers[p].allowed}\n`);
            }
            break;
        }
        if (sub === "set") {
            if (!st.up) { process.stderr.write("Unable to modify interface: No such device\n"); process.exit(1); }
            const i = rest.indexOf("peer");
            if (i >= 0) {
                const pub = rest[i + 1];
                if (rest.includes("remove")) { delete st.peers[pub]; }
                else {
                    const p = st.peers[pub] ?? { psk: "", allowed: "", ka: "" };
                    const k = rest.indexOf("allowed-ips"); if (k >= 0) p.allowed = rest[k + 1];
                    const a = rest.indexOf("persistent-keepalive"); if (a >= 0) p.ka = rest[a + 1];
                    const s = rest.indexOf("preshared-key"); if (s >= 0) { try { p.psk = fs.readFileSync(rest[s + 1], "utf8").trim(); } catch {} }
                    st.peers[pub] = p;
                }
            }
            save(st);
            break;
        }
        if (sub === "syncconf") {
            if (!st.up) { process.stderr.write("Unable to modify interface: No such device\n"); process.exit(1); }
            st.peers = parsePeers(fs.readFileSync(rest[0], "utf8"));
            save(st);
            break;
        }
        process.stderr.write(`stub awg: unsupported ${args.join(" ")}\n`); process.exit(1);
    }
    case "awg-quick": {
        const [sub] = args;
        if (sub === "up") {
            if (st.up) { process.stderr.write("awg-quick: `awg1' already exists\n"); process.exit(1); }
            if (process.env.STUB_UP_FAIL === "1") { process.stderr.write("RTNETLINK answers: Invalid argument\n"); process.exit(1); }
            st.up = true;
            try { st.peers = parsePeers(fs.readFileSync("/etc/amnezia/amneziawg/awg1.conf", "utf8")); } catch { st.peers = {}; }
        } else if (sub === "down") {
            if (!st.up) { process.stderr.write("awg-quick: `awg1' is not a WireGuard interface\n"); process.exit(1); }
            st.up = false; st.peers = {};
        }
        save(st);
        break;
    }
    case "ip": {
        const a = args.join(" ");
        if (a === "route show default") out("default via 203.0.113.1 dev eth0 proto static\n");
        else if (a.startsWith("addr show")) out("2: eth0: <BROADCAST,MULTICAST,UP> mtu 1500\n    inet 203.0.113.7/24 brd 203.0.113.255 scope global eth0\n");
        else if (a === "link show ifb-awg") { if (!st.ifb) process.exit(1); out("5: ifb-awg: <BROADCAST,NOARP,UP> mtu 1500\n"); }
        else if (a.startsWith("link add ifb-awg")) { st.ifb = true; save(st); }
        else if (a.startsWith("link del ifb-awg")) { if (!st.ifb) process.exit(1); st.ifb = false; save(st); }
        break;
    }
    case "tc": {
        const a = args.join(" ");
        st.tc ??= {};
        if (a.startsWith("qdisc replace dev")) { st.tc[args[3] + ":root"] = a; save(st); }
        else if (a.startsWith("qdisc add dev") && a.includes("ingress")) { st.tc[args[3] + ":ingress"] = a; save(st); }
        else if (a.startsWith("filter add dev")) { st.tc[args[3] + ":filter"] = a; save(st); }
        else if (a.startsWith("qdisc del dev")) {
            const key = args[3] + ":" + (args[4] === "root" ? "root" : "ingress");
            if (!st.tc[key]) { process.stderr.write("Error: Cannot delete qdisc with handle of zero.\n"); process.exit(2); }
            delete st.tc[key]; if (args[4] === "ingress") delete st.tc[args[3] + ":filter"]; save(st);
        }
        else if (a.startsWith("filter show dev")) { if (st.tc[args[3] + ":filter"]) out("filter parent ffff: protocol all pref 1 matchall\n\taction order 1: mirred (Egress Redirect to device ifb-awg) stolen\n"); }
        else if (args[0] === "-s" && args[1] === "-j") { const dev = args[5]; out(st.tc[dev + ":root"] ? `[{"kind":"cake","handle":"8001:","root":true,"bytes":1000,"packets":10,"drops":2,"overlimits":0,"requeues":0,"backlog":0,"qlen":0}]\n` : "[]\n"); }
        else if (a.startsWith("qdisc show dev")) { if (st.tc[args[3] + ":root"]) out(`qdisc cake 8001: root refcnt 2 bandwidth 285Mbit\n`); }
        break;
    }
    case "nft": {
        if (args[0] === "delete") { if (!st.nft) { process.stderr.write("Error: No such file or directory\n"); process.exit(1); } st.nft = false; save(st); }
        else if (args.includes("-")) { st.nft = true; save(st); }
        break;
    }
    case "modinfo": out((process.env.STUB_MODULE ?? "1.0.0") + "\n"); break;
    default: process.stderr.write(`stub: unknown ${cmd}\n`); process.exit(127);
}
