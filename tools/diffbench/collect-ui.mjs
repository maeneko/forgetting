// Состояние после сценария awg-ui: строки ui.db и пиры/пользователи awg-ctrl.
import fs from "fs";
import { execSync } from "child_process";
const out = process.argv[2];
const t = JSON.parse(fs.readFileSync(out, "utf8"));
const q = (db, sql) => JSON.parse(execSync(`sqlite3 -json ${db} "${sql}"`).toString() || "[]");
const UI = "/run/bench/ui.db", CT = "/etc/amnezia/amneziawg/users.db";
const tables = ["api_keys", "api_key_servers", "nodes", "master_keys", "master_servers", "devices", "device_peers",
  "peer_tombstones", "server_profiles", "sub_meta", "metrics_token"];
t.state = {
  ui: Object.fromEntries(tables.map(x => [x, q(UI, `select * from ${x} order by rowid`)])),
  uiSchema: execSync(`sqlite3 ${UI} .schema`).toString(),
  ctrlUsers: q(CT, "select name, ip, key_gen from users order by rowid"),
  ctrlPeers: q(CT, "select pub_key, ip, owner from peers order by rowid"),
};
fs.writeFileSync(out, JSON.stringify(t, null, 1));
console.log("collect-ui: done");
