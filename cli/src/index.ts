// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { spawn, spawnSync }                     from "child_process";
import { readFileSync, writeFileSync, existsSync,
    mkdirSync, openSync, closeSync,
    unlinkSync }                           from "fs";
import * as readline                            from "readline";
import * as crypto                              from "crypto";
import * as http                                from "http";
import path                                     from "path";

const ROOT = path.resolve(__dirname, "../..");
const LOGS = path.join(ROOT, "logs");

if (!existsSync(LOGS)) mkdirSync(LOGS, { recursive: true });

// Уже заданное окружение не перетираем (??=), поэтому порядок = приоритет:
// реальное окружение → cli.env (машинные настройки, пишет install.sh) →
// .env в корне проекта (имя панели и версия, едет в архиве релиза).
function loadEnvFile(file: string) {
    if (!existsSync(file)) return;
    for (const line of readFileSync(file, "utf8").split("\n")) {
        const t = line.trim();
        if (!t || t.startsWith("#")) continue;
        const idx = t.indexOf("=");
        if (idx === -1) continue;
        process.env[t.slice(0, idx).trim()] ??= t.slice(idx + 1).trim();
    }
}

const envFile = path.join(__dirname, "..", "cli.env");   // машинные настройки
loadEnvFile(envFile);
loadEnvFile(path.join(ROOT, ".env"));                    // имя панели и версия

// Имя панели, канал и версия — из .env (или из cli.env, если оператор задал
// своё имя при установке). В коде только запасные значения на случай, когда
// .env не доехал.
const BRAND   = process.env.BRAND   || "Forgetting";
const CHANNEL = process.env.CHANNEL || "Beta";
const VERSION = process.env.VERSION || "";

const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms));

const C = {
    reset: "\x1b[0m",
    bold:  "\x1b[1m",
    dim:   "\x1b[2m",
    green: "\x1b[32m",
    grey:  "\x1b[90m",
    blue:  "\x1b[34m",
};

const bold  = (s: string) => `${C.bold}${s}${C.reset}`;
const green = (s: string) => `${C.green}${s}${C.reset}`;
const grey  = (s: string) => `${C.grey}${s}${C.reset}`;
const dim   = (s: string) => `${C.dim}${s}${C.reset}`;

const SERVICES = {
    awgctrl: {
        label:   "awg-ctrl",
        cwd:     path.join(ROOT, "awg-ctrl"),
        entry:   "index.ts",
        env: {
            PORT:        process.env.AWGCTRL_PORT     ?? "3005",
            SERVER_IP:   process.env.SERVER_IP        ?? "",
            SERVER_PORT: process.env.SERVER_PORT      ?? "47619",
            SERVER_NAME: process.env.SERVER_NAME      ?? "VPN",
            // Публичный ключ внутренней авторизации (awg-ctrl проверяет им токены awg-ui).
            INTERNAL_AUTH_PUB_FILE: process.env.INTERNAL_AUTH_PUB_FILE
                ?? "/etc/amnezia/amneziawg/internal_auth_public.key",
        },
        pidFile: "/tmp/awg-ctrl.pid",
        logFile: path.join(LOGS, "awg-ctrl.log"),
    },
    ui: {
        label:   "awg-ui",
        cwd:     path.join(ROOT, "awg-ui"),
        entry:   "server.ts",
        env: {
            PORT:         process.env.UI_PORT         ?? "8080",
            AWGCTRL_PORT: process.env.AWGCTRL_PORT    ?? "3005",
            // Приватный ключ внутренней авторизации (awg-ui подписывает им токены к awg-ctrl).
            INTERNAL_AUTH_KEY_FILE: process.env.INTERNAL_AUTH_KEY_FILE
                ?? "/etc/amnezia/amneziawg/internal_auth_private.key",
            // Панель показывает их в вордмарке (GET /ui/brand).
            BRAND:        BRAND,
            CHANNEL:      CHANNEL,
            VERSION:      VERSION,
            UI_USER:      process.env.UI_USER         ?? "admin",
            UI_PASS:      process.env.UI_PASS         ?? "",
            JWT_SECRET:   process.env.JWT_SECRET      ?? "",
            // ui.db (ключи внешнего API) в каталоге данных AWG (вне PROJECT) —
            // переживает переустановку.
            UI_DB_FILE:   process.env.UI_DB_FILE      ?? "/etc/amnezia/amneziawg/ui.db",
            // Подписка sen:// (мастер-ключи SenAWG): порт, режим TLS, ключ подписи и
            // сертификат. Без SUB_PORT или ключа подписи awg-ui подписку не поднимает.
            SUB_PORT:          process.env.SUB_PORT          ?? "",
            SUB_TLS:           process.env.SUB_TLS           ?? "off",
            SUB_SIGN_KEY_FILE: process.env.SUB_SIGN_KEY_FILE ?? "/etc/amnezia/amneziawg/sub_sign.key",
            SUB_TLS_KEY_FILE:  process.env.SUB_TLS_KEY_FILE  ?? "/etc/amnezia/amneziawg/sub_tls.key",
            SUB_TLS_CERT_FILE: process.env.SUB_TLS_CERT_FILE ?? "/etc/amnezia/amneziawg/sub_tls.crt",
            // Публичный адрес панели в sen://-ссылках и endpoints. Пусто — адрес локального
            // сервера (у панели без своего VPN — тот, по которому к ней зашли).
            SUB_HOST:          process.env.SUB_HOST          ?? "",
            // Приём нод (core): порт хаба WSS и его самоподписанный сертификат (pin зашит в
            // join-строки, живёт как sub_tls.*). Без NODE_PORT хаб выключен. LOCAL_NODE=off —
            // core без собственного VPN: awg-ctrl рядом не нужен.
            NODE_PORT:          process.env.NODE_PORT          ?? "",
            LOCAL_NODE:         process.env.LOCAL_NODE         ?? "on",
            NODE_TLS_KEY_FILE:  process.env.NODE_TLS_KEY_FILE  ?? "/etc/amnezia/amneziawg/node_tls.key",
            NODE_TLS_CERT_FILE: process.env.NODE_TLS_CERT_FILE ?? "/etc/amnezia/amneziawg/node_tls.crt",
        },
        pidFile: "/tmp/awg-ui.pid",
        logFile: path.join(LOGS, "awg-ui.log"),
    },
    // Агент ноды: сам звонит в core (CORE_HOST/PORT/PIN из join-строки) и исполняет запросы
    // на локальном awg-ctrl. Запускается вместо awg-ui — см. ROLE.
    agent: {
        label:   "awg-agent",
        cwd:     path.join(ROOT, "awg-agent"),
        entry:   "index.ts",
        env: {
            CORE_HOST:     process.env.CORE_HOST     ?? "",
            CORE_PORT:     process.env.CORE_PORT     ?? "",
            CORE_PIN:      process.env.CORE_PIN      ?? "",
            NODE_ID:       process.env.NODE_ID       ?? "",
            NODE_KEY_FILE: process.env.NODE_KEY_FILE ?? "/etc/amnezia/amneziawg/node.key",
            JOIN_SECRET:   process.env.JOIN_SECRET   ?? "",
            CLI_ENV_FILE:  envFile,
            AWGCTRL_PORT:  process.env.AWGCTRL_PORT  ?? "3005",
            INTERNAL_AUTH_KEY_FILE: process.env.INTERNAL_AUTH_KEY_FILE
                ?? "/etc/amnezia/amneziawg/internal_auth_private.key",
        },
        pidFile: "/tmp/awg-agent.pid",
        logFile: path.join(LOGS, "awg-agent.log"),
    },
} as const;

type SvcName = keyof typeof SERVICES;

// Роль установки. node — VPN-сервер без панели: awg-ctrl + агент, который сам
// подключается к core (есть CORE_HOST, его пишет `join`). core с LOCAL_NODE=off —
// панель без собственного VPN (только awg-ui). Иначе — обычная установка.
const IS_NODE = !!process.env.CORE_HOST;
const NO_LOCAL = (process.env.LOCAL_NODE ?? "on").toLowerCase() === "off";
// Нода со своей панелью: работающий сервер «панель + VPN» подключили к главной панели, не
// выключая его собственную (join без --no-panel). Обе панели ходят в один awg-ctrl — он и
// есть источник правды, поэтому друг другу они не мешают. У ноды, поставленной install.sh,
// панели нет (UI_PASS не задан).
const NODE_PANEL = IS_NODE && !!process.env.UI_PASS && (process.env.NODE_PANEL ?? "on").toLowerCase() !== "off";
const ALL: SvcName[] = IS_NODE ? (NODE_PANEL ? ["awgctrl", "ui", "agent"] : ["awgctrl", "agent"])
    : NO_LOCAL ? ["ui"] : ["awgctrl", "ui"];

function readPid(name: SvcName): number | null {
    const { pidFile } = SERVICES[name];
    if (!existsSync(pidFile)) return null;
    const n = parseInt(readFileSync(pidFile, "utf8").trim());
    return isNaN(n) ? null : n;
}

function isAlive(pid: number): boolean {
    // EPERM → процесс существует, просто не наш (трактуем как «жив»).
    try { process.kill(pid, 0); return true; }
    catch (e: any) { return e?.code === "EPERM"; }
}

// Защита от переиспользования PID: на Linux сверяем cmdline процесса
// с entry-файлом сервиса. Если procfs недоступен — проверить нечем.
function pidMatchesService(name: SvcName, pid: number): boolean {
    const cmdlinePath = `/proc/${pid}/cmdline`;
    if (!existsSync(cmdlinePath)) return true;
    try {
        const cmdline = readFileSync(cmdlinePath, "utf8");
        return cmdline.includes(SERVICES[name].entry);
    } catch { return true; }
}




function isRunning(name: SvcName): boolean {
    const pid = readPid(name);
    return pid !== null && isAlive(pid) && pidMatchesService(name, pid);
}

async function start(name: SvcName) {
    const svc = SERVICES[name];
    if (isRunning(name)) {
        console.log(`  ${svc.label}: already running  pid=${readPid(name)}`);
        return;
    }
    if (name === "awgctrl" && "INTERNAL_AUTH_PUB_FILE" in svc.env && !existsSync(svc.env.INTERNAL_AUTH_PUB_FILE)) {
        console.error("  awgctrl: публичный ключ внутренней авторизации не найден: " + svc.env.INTERNAL_AUTH_PUB_FILE);
        process.exit(1);
    }

    // npx завершается сразу после передачи управления node → его PID мёртв.
    // Запускаем tsx напрямую из node_modules/.bin/, чтобы child.pid был
    // реальным PID сервера. Ищем бинарник и в самом сервисе, и в корне
    // (на случай hoisting зависимостей в общий node_modules).
    const candidates = [
        path.join(svc.cwd, "node_modules/.bin/tsx"),
        path.join(ROOT,    "node_modules/.bin/tsx"),
    ];
    const tsxBin = candidates.find(existsSync);
    const bin    = tsxBin ?? "npx";
    const args   = tsxBin ? [svc.entry] : ["tsx", svc.entry];

    const logFd = openSync(svc.logFile, "a");
    const child = spawn(bin, args, {
        cwd:      svc.cwd,
        env:      { ...process.env, ...svc.env },
        detached: true,
        stdio:    ["ignore", logFd, logFd],
    });

    let spawnError: Error | null = null;
    child.on("error", e => { spawnError = e; });

    // Даём spawn шанс упасть (ENOENT и т.п.) до записи pid-файла.
    await sleep(50);
    closeSync(logFd);            // fd унаследован ребёнком — родителю не нужен

    if (spawnError || !child.pid) {
        console.error(`  ${svc.label}: не удалось запустить — ` +
            `${spawnError ? (spawnError as Error).message : "нет PID"}`);
        try { unlinkSync(svc.pidFile); } catch {}
        return;
    }

    child.unref();
    writeFileSync(svc.pidFile, String(child.pid));
    console.log(`  ${green("▶")} ${svc.label}: started  pid=${child.pid}`);
}

async function stop(name: SvcName) {
    const svc = SERVICES[name];
    const pid = readPid(name);

    if (pid === null || !isAlive(pid) || !pidMatchesService(name, pid)) {
        console.log(`  ${svc.label}: not running`);
        try { unlinkSync(svc.pidFile); } catch {}
        return;
    }

    try { process.kill(pid, "SIGTERM"); } catch {}

    // Ждём мягкого завершения до 5 с, иначе добиваем SIGKILL.
    const deadline = Date.now() + 5000;
    while (isAlive(pid) && Date.now() < deadline) await sleep(100);
    if (isAlive(pid)) {
        try { process.kill(pid, "SIGKILL"); } catch {}
        await sleep(200);
    }

    try { unlinkSync(svc.pidFile); } catch {}
    console.log(`  ${grey("■")} ${svc.label}: stopped  pid=${pid}`);
}

async function restart(name: SvcName) {
    await stop(name);
    await start(name);
}

// GET /health и разбор JSON-тела. Любая ошибка (не запущен, таймаут, не JSON)
// молча даёт null — health-строка в status() тогда просто не печатается.
interface HealthBody {
    gen?: string;
    awg?: { module?: string; tools?: string };
}
function fetchHealth(url: string, timeoutMs = 1000): Promise<HealthBody | null> {
    return new Promise(resolve => {
        const req = http.get(url, res => {
            let body = "";
            res.on("data", (c: Buffer) => { body += c; });
            res.on("end", () => {
                try { resolve(JSON.parse(body) as HealthBody); } catch { resolve(null); }
            });
        });
        req.on("error", () => resolve(null));
        req.setTimeout(timeoutMs, () => { req.destroy(); resolve(null); });
    });
}

async function status() {
    console.log("");
    for (const name of ALL) {
        const running = isRunning(name);
        const pid     = readPid(name);
        const dot     = running ? green("●") : grey("○");
        const info    = running ? green("running") + `  pid ${pid}` : grey("stopped");
        console.log(`  ${dot} ${SERVICES[name].label.padEnd(12)} ${info}`);
    }
    if (isRunning("awgctrl")) {
        const h = await fetchHealth(`http://127.0.0.1:${SERVICES.awgctrl.env.PORT}/health`);
        if (h?.gen) {
            const gen = h.gen === "3.1" ? "3.1" : "2.0";
            console.log(`  ${dim(`AmneziaWG ${gen} · модуль ${h.awg?.module || "?"} · tools ${h.awg?.tools || "?"}`)}`);
        }
    }
    console.log("");
}

// HTTP-проба: любой ответ (даже 401/503) означает, что порт слушается.
function probe(url: string, timeoutMs = 1000): Promise<boolean> {
    return new Promise(resolve => {
        const req = http.get(url, res => { res.resume(); resolve(true); });
        req.on("error", () => resolve(false));
        req.setTimeout(timeoutMs, () => { req.destroy(); resolve(false); });
    });
}

// Ждём, пока awg-ctrl начнёт отвечать. Если процесс умер (например,
// ensureInterfaceUp() бросил исключение) — сразу выходим.
async function waitForHealth(name: "awgctrl", attempts = 10): Promise<boolean> {
    const url = `http://127.0.0.1:${SERVICES[name].env.PORT}/health`;
    for (let i = 0; i < attempts; i++) {
        if (!isRunning(name)) return false;
        if (await probe(url)) return true;
        await sleep(300);
    }
    return false;
}

async function startAll() {
    // Панель без своего VPN: awg-ctrl рядом нет, health-gate не нужен.
    if (!ALL.includes("awgctrl")) { for (const n of ALL) await start(n); return; }

    await start("awgctrl");
    const rest = ALL.filter(n => n !== "awgctrl");
    if (!isRunning("awgctrl") || !(await waitForHealth("awgctrl"))) {
        console.error(`  ${grey("■")} awg-ctrl не отвечает — ${rest.map(n => SERVICES[n].label).join(", ")} не запущен (см. ${SERVICES.awgctrl.logFile})`);
        return;
    }
    for (const n of rest) await start(n);
}

async function stopAll()    { for (const n of ALL) await stop(n); }
async function restartAll() { await stopAll(); await startAll(); }

function updateEnvVar(content: string, key: string, value: string): string {
    const re = new RegExp(`^${key}=.*$`, "m");
    return re.test(content)
        ? content.replace(re, `${key}=${value}`)
        : content.trimEnd() + `\n${key}=${value}\n`;
}

function removeEnvVar(content: string, key: string): string {
    return content.split("\n").filter(l => !l.startsWith(`${key}=`)).join("\n");
}

// Роль читается из окружения при старте процесса, поэтому после смены cli.env
// перезапуск делаем свежим процессом CLI, а не в этом же.
function restartFresh(): void {
    spawnSync(process.execPath, [...process.execArgv, process.argv[1], "restart"], { stdio: "inherit" });
}

// `join <awgjoin://…> [--no-panel]`: подключить эту установку к главной панели как ноду. Рядом
// запускается агент; awg-ctrl и пользователи остаются как были. Своя панель (awg-ui) продолжает
// работать, если она здесь есть, — с --no-panel она останавливается.
async function joinCore(link?: string, flag?: string) {
    if (!link?.startsWith("awgjoin://") || (flag !== undefined && flag !== "--no-panel")) {
        console.error("  Usage: awg-ctrl join <awgjoin://…> [--no-panel]   (ссылку даёт панель: «Добавить сервер»)");
        process.exit(1);
    }
    let j: { v?: number; h?: string; p?: number; pin?: string; n?: number; s?: string };
    try { j = JSON.parse(Buffer.from(link.slice("awgjoin://".length), "base64url").toString("utf8")); }
    catch { console.error("  Ссылка повреждена"); process.exit(1); }
    const ok = j.v === 1 && typeof j.h === "string" && /^[A-Za-z0-9.:_-]{1,253}$/.test(j.h)
        && Number.isInteger(j.p) && j.p! > 0 && j.p! < 65536
        && typeof j.pin === "string" && /^[A-Za-z0-9_-]{43}$/.test(j.pin)
        && Number.isInteger(j.n) && j.n! > 0 && typeof j.s === "string" && /^[A-Za-z0-9_-]{22}$/.test(j.s);
    if (!ok) { console.error("  Ссылка неверного формата или от другой версии панели"); process.exit(1); }
    if (IS_NODE && !await confirmYes(`  Эта нода уже подключена к ${process.env.CORE_HOST}. Перепривязать?`)) return;

    const keepPanel = flag !== "--no-panel" && !!process.env.UI_PASS;
    let content = existsSync(envFile) ? readFileSync(envFile, "utf8") : "";
    const set: Record<string, string> = {
        CORE_HOST: j.h!, CORE_PORT: String(j.p), CORE_PIN: j.pin!, NODE_ID: String(j.n), JOIN_SECRET: j.s!,
        NODE_PANEL: keepPanel ? "on" : "off",
    };
    for (const [k, v] of Object.entries(set)) content = updateEnvVar(content, k, v);
    writeFileSync(envFile, content, { mode: 0o600 });

    // Панель останавливается, а пиры её устройств остаются на awg-ctrl — управлять ими больше
    // некому. Решает оператор; спрашиваем до остановки, снимаем после (иначе панель вернёт их).
    const dropPeers = !keepPanel && process.env.UI_PASS ? await askDropPanelPeers() : false;
    if (!keepPanel) await stop("ui");
    if (dropPeers) await dropArea(localPanelId()!);
    console.log(`\n  ${green("✓")} Настроено: core ${j.h}:${j.p}, нода #${j.n}` +
        (keepPanel ? " · своя панель продолжает работать" : process.env.UI_PASS ? " · своя панель остановлена" : ""));
    restartFresh();
}

// ── Пиры подписки (sen://) на локальном awg-ctrl ──────────────────────────────
// Пира ставит панель, owner = «<id панели>/m<ключ>/d<устройство>»: id — область панели.
// У ноды со своей панелью на одном awg-ctrl две области (своя и core). Пиры без области
// («m1/d2») выданы до областей. Снимает их awg-ctrl по полному списку от панели
// (PUT /api/peers/sync); пустой список = снять всю область. Здесь — ручной рычаг для
// оператора: панель выключена или core пропал насовсем. По времени ничего не удаляется.
const PANEL_ID_FILE = path.join(ROOT, "awg-ui", "panel.id");
const AREA_RE       = /^[a-z0-9]{8,16}$/;
const LEGACY_OWNER  = /^m\d+\/d\d+$/;

function localPanelId(): string | null {
    try { const id = readFileSync(PANEL_ID_FILE, "utf8").trim(); return AREA_RE.test(id) ? id : null; }
    catch { return null; }
}

// Тот же короткоживущий токен, что awg-ui и awg-agent шлют awg-ctrl (Ed25519, 60 с).
function mintInternalToken(): string {
    const key = crypto.createPrivateKey(readFileSync(SERVICES.ui.env.INTERNAL_AUTH_KEY_FILE));
    const t = Math.floor(Date.now() / 1000);
    const h = Buffer.from('{"alg":"EdDSA","typ":"JWT"}').toString("base64url");
    const p = Buffer.from(JSON.stringify({ iss: "awg-cli", iat: t, exp: t + 60 })).toString("base64url");
    return `${h}.${p}.${crypto.sign(null, Buffer.from(`${h}.${p}`), key).toString("base64url")}`;
}

function ctrlCall(method: string, urlPath: string, body?: unknown): Promise<{ status: number; data: any }> {
    return new Promise((resolve, reject) => {
        const payload = body === undefined ? undefined : JSON.stringify(body);
        const req = http.request({
            host: "127.0.0.1", port: Number(SERVICES.awgctrl.env.PORT), method, path: urlPath, timeout: 30_000,
            headers: {
                Authorization: `Bearer ${mintInternalToken()}`, "Content-Type": "application/json",
                ...(payload ? { "Content-Length": Buffer.byteLength(payload) } : {}),
            },
        }, res => {
            let raw = "";
            res.on("data", c => { raw += c; });
            res.on("end", () => { try { resolve({ status: res.statusCode ?? 0, data: JSON.parse(raw) }); } catch { resolve({ status: res.statusCode ?? 0, data: null }); } });
        });
        req.on("error", reject);
        req.on("timeout", () => req.destroy(new Error("timeout")));
        req.end(payload);
    });
}

interface CtrlPeer { pub_key: string; ip: string; owner: string; online: boolean }

async function listPeers(): Promise<CtrlPeer[]> {
    const r = await ctrlCall("GET", "/api/peers");
    if (r.status !== 200) throw new Error(r.data?.error ?? `awg-ctrl ответил ${r.status}`);
    return r.data.peers as CtrlPeer[];
}

// Область пира: id панели, «legacy» (до областей) или null (чужой формат — не наш).
function areaOf(owner: string): string | null {
    const head = owner.split("/")[0];
    if (owner.includes("/") && AREA_RE.test(head)) return head;
    return LEGACY_OWNER.test(owner) ? "legacy" : null;
}

const peersWord = (n: number) =>
    n % 10 === 1 && n % 100 !== 11 ? "пир" : [2, 3, 4].includes(n % 10) && ![12, 13, 14].includes(n % 100) ? "пира" : "пиров";

async function askDropPanelPeers(): Promise<boolean> {
    const own = localPanelId();
    if (!own) return false;
    let n: number;
    try { n = (await listPeers()).filter(p => areaOf(p.owner) === own).length; }
    catch (e) { console.warn(`  ${dim(`Не удалось посмотреть пиров подписки: ${(e as Error).message}`)}`); return false; }
    if (n === 0) return false;

    const preset = (process.env.NODE_PEERS ?? "").toLowerCase();
    if (preset === "keep" || preset === "drop") return preset === "drop";
    console.log(`\n  На этой панели ${n} ${peersWord(n)} подписки (устройства мастер-ключей). Без панели ими некому управлять:`);
    console.log(`    1) Оставить — продолжат работать, отозвать их можно будет только после unjoin`);
    console.log(`    2) Отключить — снять их с VPN (ключи останутся в ui.db и вернутся после unjoin)`);
    const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
    const a = await new Promise<string>(r => rl.question("  Выбор [1]: ", r));
    rl.close();
    return a.trim() === "2";
}

// Снять всю область: для панели — пустым списком через sync, для legacy — по одному.
async function dropArea(area: string): Promise<void> {
    try {
        if (area === "legacy") {
            const legacy = (await listPeers()).filter(p => areaOf(p.owner) === "legacy");
            for (const p of legacy) await ctrlCall("DELETE", `/api/peers/${Buffer.from(p.pub_key, "base64").toString("base64url")}`);
            console.log(`  ${green("✓")} Снято пиров без области: ${legacy.length}`);
            return;
        }
        const r = await ctrlCall("PUT", "/api/peers/sync", { owner: area, keep: [] });
        if (r.status !== 200) throw new Error(r.data?.error ?? `awg-ctrl ответил ${r.status}`);
        console.log(`  ${green("✓")} Снято пиров области ${area}: ${r.data.removed}`);
    } catch (e) {
        console.error(`  Не удалось снять пиров: ${(e as Error).message}`);
    }
}

async function peersCmd(sub?: string, arg?: string) {
    const own = localPanelId();
    let peers: CtrlPeer[];
    try { peers = await listPeers(); }
    catch (e) { console.error(`  awg-ctrl недоступен: ${(e as Error).message}`); process.exit(1); }
    const areas = new Map<string, CtrlPeer[]>();
    for (const p of peers) { const a = areaOf(p.owner); if (a) areas.set(a, [...(areas.get(a) ?? []), p]); }

    if (sub === undefined) {
        if (!areas.size) { console.log("  Пиров подписки (sen://) на этом сервере нет"); return; }
        console.log("\n  Пиры подписки (sen://) на этом сервере:\n");
        for (const [a, list] of areas) {
            const mark = a === own ? "эта панель" : a === "legacy" ? "без области (выданы до областей)" : "";
            console.log(`    ${bold(a.padEnd(16))} ${String(list.length).padStart(4)} ${peersWord(list.length).padEnd(5)}` +
                `  онлайн ${list.filter(p => p.online).length}  ${dim(mark)}`);
        }
        console.log(`\n  ${dim("Снять область целиком: awg-ctrl peers drop <область>")}`);
        return;
    }
    if (sub !== "drop" || !arg || !(arg === "legacy" || AREA_RE.test(arg))) {
        console.error("  Usage: awg-ctrl peers [drop <область|legacy>]");
        process.exit(1);
    }
    const n = areas.get(arg)?.length ?? 0;
    if (!n) { console.log(`  В области ${arg} пиров нет`); return; }
    // Панель, которая ведёт эту область, вернёт пиров при следующем сведении — снимать бесполезно.
    if (arg === own && isRunning("ui")) {
        console.error("  Эту область ведёт панель на этой машине — удаляйте мастер-ключи или устройства в ней");
        process.exit(1);
    }
    console.log(`  ${dim("Если панель этой области ещё ведёт сервер, она вернёт пиров при следующем сведении —")}`);
    console.log(`  ${dim("сперва удалите сервер в ней (или мастер-ключи).")}`);
    if (!await confirmYes(`  Снять ${n} ${peersWord(n)} области ${arg}? Их устройства потеряют доступ к этому серверу.`)) return;
    await dropArea(arg);
}

async function unjoinCore() {
    if (!IS_NODE) { console.log("  Эта установка не подключена к core"); return; }
    await stop("agent");
    let content = existsSync(envFile) ? readFileSync(envFile, "utf8") : "";
    for (const k of ["CORE_HOST", "CORE_PORT", "CORE_PIN", "NODE_ID", "JOIN_SECRET", "NODE_PANEL"]) content = removeEnvVar(content, k);
    writeFileSync(envFile, content, { mode: 0o600 });
    console.log(`  ${green("✓")} Нода отвязана. Удалите её в панели (сервер) и запустите: awg-ctrl restart`);
    // Нода, поставленная install.sh с ролью «нода», панели не имеет — без пароля в неё не войти.
    if (!process.env.UI_PASS)
        console.log(`  ${dim("Панель на этой машине не настроена: задайте логин и пароль — awg-ctrl credentials")}`);
}

async function confirmYes(q: string): Promise<boolean> {
    const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
    const a = await new Promise<string>(r => rl.question(`${q} [y/N] `, r));
    rl.close();
    return /^y/i.test(a.trim());
}

async function setCredentials(field?: "user" | "pass") {
    const rl  = readline.createInterface({ input: process.stdin, output: process.stdout });
    const ask = (q: string) => new Promise<string>(r => rl.question(q, r));

    let user: string | undefined;
    let pass: string | undefined;

    if (!field || field === "user") {
        const cur = process.env.UI_USER ?? "admin";
        user = ((await ask(`\n  Логин [${cur}]: `)).trim()) || cur;
    }
    if (!field || field === "pass") {
        const generated = crypto.randomBytes(9).toString("base64url").slice(0, 12);
        pass = ((await ask(`  Пароль [${generated}]: `)).trim()) || generated;
    }

    rl.close();

    let content = existsSync(envFile) ? readFileSync(envFile, "utf8") : "";
    if (user !== undefined) { content = updateEnvVar(content, "UI_USER", user); process.env.UI_USER = user; }
    if (pass !== undefined) { content = updateEnvVar(content, "UI_PASS", pass); process.env.UI_PASS = pass; }
    writeFileSync(envFile, content, "utf8");

    if (user !== undefined) console.log(`\n  ${green("✓")} Логин:  ${user}`);
    if (pass !== undefined) console.log(`  ${green("✓")} Пароль: ${pass}`);
    console.log(`\n  ${dim("Перезапусти UI чтобы изменения вступили в силу.")}`);
}

async function interactiveMenu() {
    const rl = readline.createInterface({
        input:  process.stdin,
        output: process.stdout,
    });

    const ask = (prompt: string): Promise<string> =>
        new Promise(resolve => rl.question(prompt, resolve));

    const printMenu = () => {
        console.clear();
        console.log(`\n${bold(`${C.blue}── ${[BRAND, CHANNEL, VERSION].filter(Boolean).join(" ")} ──${C.reset}`)}\n`);

        for (const name of ALL) {
            const running = isRunning(name);
            const pid     = readPid(name);
            const dot     = running ? green("●") : grey("○");
            const info    = running
                ? green("running") + dim(`  pid ${pid}`)
                : grey("stopped");
            console.log(`  ${dot} ${SERVICES[name].label.padEnd(12)} ${info}`);
        }

        console.log(`
  ${bold("1")}  Запустить
  ${bold("2")}  Остановить
  ${bold("3")}  Перезапустить
  ${bold("4")}  Статус
  ${dim("──────────────")}
  ${bold("5")}  Сменить логин UI
  ${bold("6")}  Сменить пароль UI
  ${dim("──────────────")}
  ${bold("0")}  Выход
`);
    };

    while (true) {
        printMenu();
        const choice = (await ask("  › ")).trim();
        console.log("");

        switch (choice) {
            case "1": await startAll();             break;
            case "2": await stopAll();              break;
            case "3": await restartAll();           break;
            case "4": await status();               break;
            case "5": await setCredentials("user");  break;
            case "6": await setCredentials("pass");  break;
            case "0": rl.close(); process.exit(0);
            default:  console.log(grey("  Неверный выбор")); break;
        }

        if (choice !== "0") {
            await ask(`\n  ${dim("Enter для продолжения...")}`);
        }
    }
}

async function main() {
    const [,, cmd] = process.argv;

    if (!cmd) {
        await interactiveMenu();
        return;
    }

    const USAGE = "Usage: cli <start|stop|restart|status|credentials [user|pass]|join <awgjoin://…> [--no-panel]|unjoin|peers [drop <область>]>";

    const [,,, sub, arg] = process.argv;

    switch (cmd) {
        case "start":       await startAll();                                  break;
        case "stop":        await stopAll();                                   break;
        case "restart":     await restartAll();                                break;
        case "status":      await status();                                    break;
        case "credentials":
            if (sub === "user") await setCredentials("user");
            else if (sub === "pass") await setCredentials("pass");
            else await setCredentials();
            break;
        case "join":        await joinCore(sub, arg);                          break;
        case "unjoin":      await unjoinCore();                                break;
        case "peers":       await peersCmd(sub, arg);                          break;
        default: console.log(USAGE); process.exit(1);
    }
}

main().catch(e => { console.error(e); process.exit(1); });