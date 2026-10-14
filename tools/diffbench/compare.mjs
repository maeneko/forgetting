// compare.mjs <ts.json> <go.json>: расхождения двух прогонов стенда. vpn:// раскрываются
// (сжатие у Node и Go разное), время маскируется.
import fs from "fs"; import zlib from "zlib";
const [a, b] = process.argv.slice(2).map(f => JSON.parse(fs.readFileSync(f, "utf8")));
const dec = s => { try { const buf = Buffer.from(s.slice(6), "base64url"); return "vpn<" + zlib.inflateSync(buf.subarray(4)).toString() + ">"; } catch (e) { return "vpn<undecodable " + e.message + ">"; } };
const norm = v => {
  if (typeof v === "string") {
    let s = v.replace(/vpn:\/\/[A-Za-z0-9_-]+/g, dec);
    s = s.replace(/\d{4}-\d\d-\d\dT[\d:.-]+Z/g, "<ts>").replace(/"(created_at|lastHandshake|ts|time|uptime|generated_at)":\d+/g, '"$1":<n>');
    return s;
  }
  if (Array.isArray(v)) return v.map(norm);
  if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, /^(created_at|lastHandshake|ts|uptime)$/.test(k) && typeof x === "number" ? "<n>" : norm(x)]));
  return v;
};
// Известный шум: живые цифры хоста (порядок ключей сохраняем), стек Express в dev-режиме,
// порядок вызовов из параллельных фоновых задач, отступы в CREATE TABLE.
const host = h => h && { cpu: Object.keys(h.cpu), cores: h.cores, load: h.load?.length, mem: Object.keys(h.mem) };
for (const r of [a, b]) {
  for (const s of r.steps) {
    if (s.body?.host) { s.body.host = host(s.body.host); s.text = JSON.stringify(s.body); }
    if (s.ctype === "text/html") s.body = s.body.replace(/<pre>([A-Za-z]+Error)[^<]*(<br>[\s\S]*)?<\/pre>/, "<pre>$1</pre>");
  }
  r.state.stubLog = r.state.stubLog.map(e => JSON.stringify(e).replace(/awg_(sync|psk)_\d+/g, "awg_$1_N")).sort();
  r.state.schema = r.state.schema.replace(/\s+/g, " ");
}
let diffs = 0;
const show = (label, x, y) => { const X = JSON.stringify(norm(x)), Y = JSON.stringify(norm(y)); if (X !== Y) { diffs++; let i = 0; while (X[i] === Y[i]) i++; const from = Math.max(0, i - 150); console.log(`### ${label} @${i}\n TS: …${X.slice(from, i + 350)}\n GO: …${Y.slice(from, i + 350)}`); } };
a.steps.forEach((s, i) => { const t = b.steps[i]; show(`${i} ${s.name}`, { st: s.status, ct: s.ctype, body: s.body, text: s.text }, { st: t.status, ct: t.ctype, body: t.body, text: t.text }); });
for (const k of Object.keys(a.state)) show("state." + k, a.state[k], b.state[k]);
console.log(diffs ? `${diffs} differences` : "IDENTICAL");
