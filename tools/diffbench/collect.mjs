// Состояние после сценария: conf, файлы каталога, строки баз, журнал и состояние заглушек.
import fs from "fs";
import { execSync } from "child_process";
const out = process.argv[2];
const t = JSON.parse(fs.readFileSync(out, "utf8"));
const D = "/etc/amnezia/amneziawg";
const q = sql => JSON.parse(execSync(`sqlite3 -json ${D}/users.db "${sql}"`).toString() || "[]");
t.state = {
    conf: fs.readFileSync(`${D}/awg1.conf`, "utf8"),
    confMode: (fs.statSync(`${D}/awg1.conf`).mode & 0o777).toString(8),
    files: fs.readdirSync(D).map(f => f.replace(/\d{4}-\d\d-\d\dT[\d-]+Z/, "<ts>")).sort(),
    users: q("select * from users order by rowid"),
    peers: q("select * from peers order by rowid"),
    config: q("select * from config order by key"),
    schema: execSync(`sqlite3 ${D}/users.db .schema`).toString(),
    stubLog: fs.readFileSync("/var/lib/stub/log.jsonl", "utf8").split("\n").filter(Boolean).map(l => JSON.parse(l)),
    stubState: JSON.parse(fs.readFileSync("/var/lib/stub/state.json", "utf8")),
};
fs.writeFileSync(out, JSON.stringify(t, null, 1));
console.log(`collect: done`);
