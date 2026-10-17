// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// `awg-ctrl monitor install|status|remove` — встроенный Prometheus для вкладки «Монитор».
// Работает от root (ставит пакет, пишет в /etc): панель сама пакетов не ставит, она только просит
// (через awg-ctrl) или показывает эту команду. Всё слушает 127.0.0.1: наружу Prometheus не торчит,
// а панель читает его по HTTP на той же машине.
import { spawnSync }                                         from "child_process";
import { readFileSync, writeFileSync, existsSync, copyFileSync, unlinkSync } from "fs";
import * as crypto                                           from "crypto";
import * as http                                             from "http";

const AWG_DIR   = "/etc/amnezia/amneziawg";
const PROM_DIR  = "/etc/prometheus";
const CONF_FILE = `${PROM_DIR}/prometheus.yml`;
const TOKEN_FILE = `${PROM_DIR}/forgetting.token`;
// Хэш локального токена для awg-ui: читает его при проверке /metrics (сам токен знает только Prometheus).
export const MONITOR_HASH_FILE = `${AWG_DIR}/monitor_token.sha256`;
const DEFAULTS_FILE = "/etc/default/prometheus";
const MARKER  = "# managed by forgetting";
const LISTEN  = "127.0.0.1:9090";
const RETENTION = "31d";   // с запасом на вид «30 дней» во вкладке «Монитор»

const log  = (s: string) => console.log(`  ${s}`);
function fail(s: string): never { console.error(`\n  ✗ ${s}\n`); process.exit(1); }

function sh(cmd: string, args: string[], opts: { quiet?: boolean } = {}) {
    return spawnSync(cmd, args, {
        encoding: "utf8",
        stdio: opts.quiet ? ["ignore", "pipe", "pipe"] : ["ignore", "inherit", "inherit"],
        env: { ...process.env, DEBIAN_FRONTEND: "noninteractive" },
    });
}

function osFamily(): string {
    try {
        const t = readFileSync("/etc/os-release", "utf8");
        const id   = t.match(/^ID=(.*)$/m)?.[1]?.replace(/"/g, "") ?? "";
        const like = t.match(/^ID_LIKE=(.*)$/m)?.[1]?.replace(/"/g, "") ?? "";
        return `${id} ${like}`;
    } catch { return ""; }
}

function ready(timeoutMs = 1500): Promise<boolean> {
    return new Promise(resolve => {
        const [host, port] = LISTEN.split(":");
        const req = http.get({ host, port: Number(port), path: "/-/ready", timeout: timeoutMs }, res => {
            res.resume(); resolve(res.statusCode === 200);
        });
        req.on("timeout", () => { req.destroy(); resolve(false); });
        req.on("error", () => resolve(false));
    });
}

const installed = () => sh("dpkg", ["-s", "prometheus"], { quiet: true }).status === 0;
const active    = () => sh("systemctl", ["is-active", "--quiet", "prometheus"], { quiet: true }).status === 0;
const managed   = () => existsSync(CONF_FILE) && readFileSync(CONF_FILE, "utf8").includes(MARKER);

// Новые схемы (authorization.credentials_file) есть с 2.26; в 3.x bearer_token_file убран.
function authBlock(): string {
    const v = sh("prometheus", ["--version"], { quiet: true });
    const m = `${v.stdout ?? ""}${v.stderr ?? ""}`.match(/version (\d+)\.(\d+)/);
    const [maj, min] = m ? [Number(m[1]), Number(m[2])] : [2, 0];
    return maj > 2 || (maj === 2 && min >= 26)
        ? `    authorization:\n      credentials_file: ${TOKEN_FILE}\n`
        : `    bearer_token_file: ${TOKEN_FILE}\n`;
}

function writeConfig(uiPort: string) {
    if (existsSync(CONF_FILE)) copyFileSync(CONF_FILE, `${CONF_FILE}.bak-${new Date().toISOString().replace(/[:.]/g, "-")}`);
    writeFileSync(CONF_FILE, `${MARKER}
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: forgetting
    scheme: http
    metrics_path: /metrics
${authBlock()}    static_configs:
      - targets: ['127.0.0.1:${uiPort}']
`, { mode: 0o644 });
}

// Токен для самого Prometheus. Файл читает пользователь prometheus, в панели лежит только хэш.
function writeToken() {
    const token = "awgm_" + crypto.randomBytes(24).toString("base64url");
    writeFileSync(TOKEN_FILE, token, { mode: 0o640 });
    sh("chown", ["root:prometheus", TOKEN_FILE], { quiet: true });
    writeFileSync(MONITOR_HASH_FILE, crypto.createHash("sha256").update(token).digest("hex") + "\n", { mode: 0o644 });
}

function setArgs() {
    const line = `ARGS="--web.listen-address=${LISTEN} --storage.tsdb.retention.time=${RETENTION}"`;
    let text = existsSync(DEFAULTS_FILE) ? readFileSync(DEFAULTS_FILE, "utf8") : "";
    if (text) copyFileSync(DEFAULTS_FILE, `${DEFAULTS_FILE}.bak-${new Date().toISOString().replace(/[:.]/g, "-")}`);
    text = /^ARGS=/m.test(text) ? text.replace(/^ARGS=.*$/m, line) : `${text.replace(/\n?$/, "\n")}${line}\n`;
    writeFileSync(DEFAULTS_FILE, text, { mode: 0o644 });
}

async function install() {
    if (process.getuid?.() !== 0) fail("Нужен root: sudo awg-ctrl monitor install");
    if (!/debian|ubuntu/i.test(osFamily())) fail("Автоустановка поддерживает только Debian и Ubuntu. На другом дистрибутиве поставьте Prometheus сами и подключите его кнопкой в панели (токен метрик).");
    const uiPort = process.env.UI_PORT;
    if (!uiPort) fail("На этой машине нет панели (UI_PORT не задан): Prometheus скрейпит метрики панели.");

    // Чужой Prometheus не трогаем: перезаписать его prometheus.yml значит сломать чужой мониторинг.
    if (installed() && existsSync(CONF_FILE) && !managed()) {
        fail(`Prometheus уже установлен и настроен не нами (${CONF_FILE}). Подключите его кнопкой «Подключить Prometheus» во вкладке «Монитор»: она выдаст токен и готовый конфиг.`);
    }

    log("1/4  установка пакета prometheus");
    if (!installed()) {
        if (sh("apt-get", ["update", "-qq"]).status !== 0) fail("apt-get update завершился ошибкой");
        if (sh("apt-get", ["install", "-y", "--no-install-recommends", "prometheus"]).status !== 0) fail("apt-get install prometheus завершился ошибкой");
    } else log("уже установлен");

    log("2/4  токен и конфиг");
    writeToken();
    writeConfig(uiPort);

    log(`3/4  слушаем только ${LISTEN}, хранение ${RETENTION}`);
    setArgs();

    log("4/4  запуск");
    sh("systemctl", ["enable", "prometheus"], { quiet: true });
    if (sh("systemctl", ["restart", "prometheus"]).status !== 0) fail("prometheus не запустился: journalctl -u prometheus");
    for (let i = 0; i < 30; i++) {
        if (await ready()) { console.log(`\n  ✓ Prometheus работает на ${LISTEN}, метрики панели скрейпятся каждые 15 с.\n    Во вкладке «Монитор» графики появятся через минуту-две.\n`); return; }
        await new Promise(r => setTimeout(r, 1000));
    }
    fail("Prometheus не ответил за 30 с: journalctl -u prometheus");
}

async function status() {
    const ok = await ready();
    console.log(`  пакет:     ${installed() ? "установлен" : "нет"}`);
    console.log(`  сервис:    ${active() ? "работает" : "остановлен"}`);
    console.log(`  наш конфиг: ${managed() ? "да" : "нет"}`);
    console.log(`  готов:     ${ok ? `да, ${LISTEN}` : "нет"}`);
}

function remove() {
    if (process.getuid?.() !== 0) fail("Нужен root: sudo awg-ctrl monitor remove");
    // Чужой Prometheus не удаляем: у него могут быть свои данные и задачи, которых мы не ставили.
    if (installed() && existsSync(CONF_FILE) && !managed()) {
        fail(`Prometheus настроен не нами (${CONF_FILE}), поэтому панель его не удаляет. Если он не нужен: sudo apt purge prometheus`);
    }

    log("1/3  остановка сервиса");
    sh("systemctl", ["disable", "--now", "prometheus"], { quiet: true });

    log("2/3  токен");
    for (const f of [TOKEN_FILE, MONITOR_HASH_FILE]) { try { unlinkSync(f); } catch { /* уже нет */ } }

    log("3/3  удаление пакета и накопленных данных");
    if (installed() && sh("apt-get", ["purge", "-y", "prometheus"]).status !== 0) fail("apt-get purge prometheus завершился ошибкой");

    console.log("\n  ✓ Prometheus удалён вместе с накопленными графиками. Панель продолжает отдавать /metrics.\n");
}

export async function monitorCmd(sub?: string) {
    switch (sub) {
        case "install": await install(); break;
        case "status":  await status();  break;
        case "remove":  remove();       break;
        default: console.log("Usage: awg-ctrl monitor <install|status|remove>"); process.exit(1);
    }
}
