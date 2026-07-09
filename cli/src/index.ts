// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { spawn }                                from "child_process";
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

const envFile = path.join(__dirname, "..", "cli.env");
if (existsSync(envFile)) {
    for (const line of readFileSync(envFile, "utf8").split("\n")) {
        const t = line.trim();
        if (!t || t.startsWith("#")) continue;
        const idx = t.indexOf("=");
        if (idx === -1) continue;
        const k = t.slice(0, idx).trim();
        const v = t.slice(idx + 1).trim();
        process.env[k] ??= v;
    }
}

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
            UI_USER:      process.env.UI_USER         ?? "admin",
            UI_PASS:      process.env.UI_PASS         ?? "",
            JWT_SECRET:   process.env.JWT_SECRET      ?? "",
            // ui.db в каталоге данных AWG (вне PROJECT) — переживает переустановку.
            UI_DB_FILE:   process.env.UI_DB_FILE      ?? "/etc/amnezia/amneziawg/ui.db",
        },
        pidFile: "/tmp/awg-ui.pid",
        logFile: path.join(LOGS, "awg-ui.log"),
    },
} as const;

type SvcName = keyof typeof SERVICES;
const ALL: SvcName[] = ["awgctrl", "ui"];

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

function status() {
    console.log("");
    for (const name of ALL) {
        const running = isRunning(name);
        const pid     = readPid(name);
        const dot     = running ? green("●") : grey("○");
        const info    = running ? green("running") + `  pid ${pid}` : grey("stopped");
        console.log(`  ${dot} ${SERVICES[name].label.padEnd(12)} ${info}`);
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
async function waitForHealth(name: SvcName, attempts = 10): Promise<boolean> {
    const url = `http://127.0.0.1:${SERVICES[name].env.PORT}/health`;
    for (let i = 0; i < attempts; i++) {
        if (!isRunning(name)) return false;
        if (await probe(url)) return true;
        await sleep(300);
    }
    return false;
}

async function startAll() {
    await start("awgctrl");
    if (!isRunning("awgctrl") || !(await waitForHealth("awgctrl"))) {
        console.error(`  ${grey("■")} awg-ctrl не отвечает — awg-ui не запущен (см. ${SERVICES.awgctrl.logFile})`);
        return;
    }
    await start("ui");
}

async function stopAll()    { for (const n of ALL) await stop(n); }
async function restartAll() { await stopAll(); await startAll(); }

function updateEnvVar(content: string, key: string, value: string): string {
    const re = new RegExp(`^${key}=.*$`, "m");
    return re.test(content)
        ? content.replace(re, `${key}=${value}`)
        : content.trimEnd() + `\n${key}=${value}\n`;
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
        console.log(`\n${bold(`${C.blue}── Forgetting Alpha 0.1.3.1 ──${C.reset}`)}\n`);

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
            case "4": status();                     break;
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

    const USAGE = "Usage: cli <start|stop|restart|status|credentials [user|pass]>";

    const [,,, sub] = process.argv;

    switch (cmd) {
        case "start":       await startAll();                                  break;
        case "stop":        await stopAll();                                   break;
        case "restart":     await restartAll();                                break;
        case "status":      status();                                          break;
        case "credentials":
            if (sub === "user") await setCredentials("user");
            else if (sub === "pass") await setCredentials("pass");
            else await setCredentials();
            break;
        default: console.log(USAGE); process.exit(1);
    }
}

main().catch(e => { console.error(e); process.exit(1); });