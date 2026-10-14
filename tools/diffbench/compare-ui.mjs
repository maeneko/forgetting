// compare-ui.mjs <ts.json> <go.json>: расхождения двух прогонов scenario-ui. Случайное
// маскируется (токены, ключи, uuid, ссылки, подписи, время, id панели); подписанные ответы
// /sub/v1 раскрываются: сравнивается body, а подпись — только что она есть.
import fs from "fs";
const [a, b] = process.argv.slice(2).map(f => JSON.parse(fs.readFileSync(f, "utf8")));
const panelOf = r => r.state.ui.sub_meta.find(x => x.key === "panel_id")?.value ?? "";
const TIME_KEYS = /^(created_at|last_used|last_seen|at|ts|updated_at|join_expires|revoked_at|expires_in_at)$/;
function mask(s, panel) {
  if (panel) s = s.split(panel).join("<panel>");
  return s
    .replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "<jwt>")
    .replace(/awg[km]_[A-Za-z0-9_-]{8}(…|[A-Za-z0-9_-]{24})/g, "<token>")
    .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, "<uuid>")
    .replace(/sen:\/\/[A-Za-z0-9_-]+/g, "<sen>")
    .replace(/awgjoin:\/\/[A-Za-z0-9_-]+/g, "<join>")
    .replace(/awg_scrape_duration_seconds [0-9.e-]+/g, "awg_scrape_duration_seconds <d>")
    .replace(/(awg_host_[a-z_]+\{[^}]*\}) [0-9.e+-]+/g, "$1 <n>");
}
function norm(v, panel, key = "") {
  if (typeof v === "string") {
    if (key === "sig") return "<sig>";
    if (key === "body" && v.startsWith("{")) { try { return norm(JSON.parse(v), panel); } catch {} }
    return mask(v, panel);
  }
  if (typeof v === "number" && TIME_KEYS.test(key)) return "<t>";
  if (Array.isArray(v)) return v.map(x => norm(x, panel));
  if (v && typeof v === "object") {
    const o = {};
    for (const [k, x] of Object.entries(v)) {
      if (["key_hash", "secret", "join_hash", "token_hash", "pub", "prefix"].includes(k) && x !== null) { o[k] = "<rnd>"; continue; }
      if (k === "value" && typeof x === "string" && /^[[{]/.test(x)) { o[k] = norm(JSON.parse(x), panel); continue; }
      o[k] = norm(x, panel, k);
    }
    return o;
  }
  return v;
}
const pa = panelOf(a), pb = panelOf(b);
let diffs = 0;
const show = (label, x, y) => {
  const X = JSON.stringify(norm(x, pa)), Y = JSON.stringify(norm(y, pb));
  if (X === Y) return;
  diffs++;
  let i = 0; while (X[i] === Y[i]) i++;
  const from = Math.max(0, i - 120);
  console.log(`### ${label} @${i}\n TS: …${X.slice(from, i + 300)}\n GO: …${Y.slice(from, i + 300)}`);
};
if (a.error || b.error) console.log("scenario error:", a.error, "|", b.error);
const n = Math.max(a.steps.length, b.steps.length);
for (let i = 0; i < n; i++) {
  const s = a.steps[i] ?? {}, t = b.steps[i] ?? {};
  // text не сравниваем: порядок ключей виден и в разобранном body (Object.entries его хранит).
  // Стек Express в dev-режиме (NODE_ENV не задан) у Go не воспроизводится — сравниваем тип ошибки.
  const html = x => typeof x === "string" ? x.replace(/<pre>([A-Za-z]+Error)[^<]*(<br>[\s\S]*)?<\/pre>/, "<pre>$1</pre>") : x;
  show(`${i} ${s.name ?? t.name} ${s.method} ${s.path}`, { st: s.status, ct: s.ctype, body: html(s.body) }, { st: t.status, ct: t.ctype, body: html(t.body) });
}
for (const k of Object.keys(a.state.ui)) show("ui." + k, a.state.ui[k], b.state.ui[k]);
show("uiSchema", a.state.uiSchema.replace(/\s+/g, " "), b.state.uiSchema.replace(/\s+/g, " "));
show("ctrlUsers", a.state.ctrlUsers, b.state.ctrlUsers);
show("ctrlPeers", a.state.ctrlPeers, b.state.ctrlPeers);
console.log(diffs ? `${diffs} differences` : "IDENTICAL");
