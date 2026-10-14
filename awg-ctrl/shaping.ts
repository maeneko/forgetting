// Честное деление канала между устройствами (CAKE) + автоподстройка по RFC 6057.
//
// Схема:
//   загрузка (к клиенту)  — CAKE root на awg1, dual-dsthost, diffserv3
//   отдача (от клиента)   — ingress awg1 → ifb-awg, CAKE dual-srchost, besteffort
//   тяжёлые при перегрузке — DSCP CS1 (bulk-tin CAKE) через отдельную nft-таблицу
//
// Ничего не пишет в awg1.conf (PostUp не трогаем): всё применяется из процесса и
// идемпотентно. Правила tc пропадают вместе с интерфейсом, поэтому apply() зовётся
// на старте, после restartAwg()/startInterface() и из страховочной проверки в цикле.
//
// Сэмплер только КЛАССИФИЦИРУЕТ. Он никого не удаляет и не отключает — ни по
// времени, ни по неактивности (правило проекта: удаление только по явному списку
// от панели).
import { execFile, spawn } from "child_process";
import type { Logger } from "winston";

export interface ShapingSettings {
    enabled:  boolean;
    downMbit: number;
    upMbit:   number;
    auto:     boolean;
}

export interface PeerCounters {
    ip: string;
    rx: number;   // принято от клиента (его отдача)
    tx: number;   // отправлено клиенту (его загрузка)
}

export interface ShapingDeps {
    iface:    string;
    getCfg:   (key: string, fallback: string) => string;
    setCfg:   (key: string, value: string) => void;
    logger:   Pick<Logger, "info" | "warn" | "error">;
    // Байт на пакет, которые туннель добавляет на проводе (CAKE видит только внутренний IP-пакет).
    overhead: () => number;
    peers:    () => Promise<PeerCounters[]>;
}

export interface QdiscStats { packets: number; bytes: number; drops: number; backlog: number }

export interface ShapingStatus {
    settings:  ShapingSettings;
    active:    boolean;               // CAKE реально стоит на интерфейсе
    congested: boolean;
    heavy:     string[];              // IP устройств в bulk-классе
    devices:   number;                // активных устройств в окне
    rateDown:  number;                // Мбит/с за последний тик
    rateUp:    number;
    down?:     QdiscStats;
    up?:       QdiscStats;
}

// Окна и пороги. Переопределение через env — только для тестов на стенде.
const num = (v: string | undefined, d: number) => (v && Number.isFinite(Number(v)) && Number(v) > 0 ? Number(v) : d);
const SAMPLE_S       = num(process.env.SHAPE_SAMPLE_S, 10);
const WINDOW_S       = num(process.env.SHAPE_WINDOW_S, 600);
const CONGEST_ON     = 0.85;   // доля ширины, выше которой канал считается перегруженным
const CONGEST_OFF    = 0.70;   // и ниже которой перегрузка снимается (гистерезис)
const ACTIVE_MBIT    = 0.1;    // меньше — устройство не считается активным
const HEAVY_MULT     = 1.5;    // тяжёлое: ≥ 1.5 честных долей…
const HEAVY_MIN_FRAC = 0.05;   // …и не меньше 5% ширины канала
const CHECK_EVERY    = 3;      // страховочная проверка qdisc — раз в N тиков

const IFB  = "ifb-awg";
const NFT_TABLE = "awg_shape";

function exec(cmd: string, args: string[]): Promise<string> {
    return new Promise((resolve, reject) => {
        execFile(cmd, args, { encoding: "utf8", timeout: 15_000 }, (err, stdout, stderr) => {
            if (err) reject(new Error(`${cmd} ${args.join(" ")}: ${(stderr || err.message).toString().trim()}`));
            else resolve(stdout);
        });
    });
}

function execStdin(cmd: string, args: string[], input: string): Promise<void> {
    return new Promise((resolve, reject) => {
        const p = spawn(cmd, args, { stdio: ["pipe", "ignore", "pipe"] });
        let err = "";
        p.stderr.on("data", d => { err += d; });
        p.stdin.on("error", () => undefined);   // процесс умер раньше, чем дочитал stdin — причина придёт в close
        p.on("error", reject);
        p.on("close", code => code === 0 ? resolve() : reject(new Error(`${cmd}: ${err.trim() || `exit ${code}`}`)));
        p.stdin.end(input);
    });
}

interface Tick { dt: number; bytes: number }

export function createShaping(deps: ShapingDeps) {
    const { iface, logger } = deps;

    // ───────────── настройки ─────────────
    function settings(): ShapingSettings {
        return {
            enabled:  deps.getCfg("shapeEnabled", "0") === "1",
            downMbit: Number(deps.getCfg("shapeDownMbit", "0")) || 0,
            upMbit:   Number(deps.getCfg("shapeUpMbit", "0")) || 0,
            auto:     deps.getCfg("shapeAuto", "1") === "1",
        };
    }

    function save(s: ShapingSettings) {
        deps.setCfg("shapeEnabled",  s.enabled ? "1" : "0");
        deps.setCfg("shapeDownMbit", String(s.downMbit));
        deps.setCfg("shapeUpMbit",   String(s.upMbit));
        deps.setCfg("shapeAuto",     s.auto ? "1" : "0");
    }

    // ───────────── применение ─────────────
    // Все изменения tc/nft идут строго по одному: два параллельных apply() дали бы
    // полусобранные qdisc.
    let chain: Promise<unknown> = Promise.resolve();
    const serial = <T>(fn: () => Promise<T>): Promise<T> => {
        const r = chain.then(fn, fn);
        chain = r.catch(() => undefined);
        return r;
    };

    let lastError = "";
    const noteError = (e: unknown) => {
        const m = e instanceof Error ? e.message : String(e);
        if (m !== lastError) logger.warn("shaping: " + m);   // одинаковую ошибку в лог не спамим
        lastError = m;
    };

    const cakeOverhead = () => {
        const o = Number(process.env.SHAPE_OVERHEAD);
        return Number.isFinite(o) && o >= 0 && process.env.SHAPE_OVERHEAD ? o : deps.overhead();
    };

    async function nftApply(heavy: string[]) {
        const elements = heavy.length ? `\n        elements = { ${heavy.join(", ")} }` : "";
        // add+delete в одной транзакции: таблица пересоздаётся атомарно, без окна без правил.
        await execStdin("nft", ["-f", "-"], `
add table inet ${NFT_TABLE}
delete table inet ${NFT_TABLE}
table inet ${NFT_TABLE} {
    set heavy {
        type ipv4_addr${elements}
    }
    chain postrouting {
        type filter hook postrouting priority -150; policy accept;
        oifname "${iface}" ip daddr @heavy ip dscp set cs1
    }
}
`);
    }

    async function hasCake(dev: string): Promise<boolean> {
        try { return /\bcake\b/.test(await exec("tc", ["qdisc", "show", "dev", dev, "root"])); }
        catch { return false; }
    }

    async function hasIngressRedirect(): Promise<boolean> {
        try { return /mirred/.test(await exec("tc", ["filter", "show", "dev", iface, "parent", "ffff:"])); }
        catch { return false; }
    }

    async function applyNow() {
        const s = settings();
        if (!s.enabled || s.downMbit <= 0 || s.upMbit <= 0) { await removeNow(); return; }
        const oh = cakeOverhead();

        // Загрузка (к клиенту). replace = change, если qdisc уже стоит: скорость меняется на лету.
        await exec("tc", ["qdisc", "replace", "dev", iface, "root", "cake",
            "bandwidth", `${s.downMbit}mbit`, "diffserv3", "dual-dsthost", "overhead", String(oh)]);

        // Отдача (от клиента): зеркалим входящий трафик awg1 на ifb и делим там.
        try { await exec("ip", ["link", "show", IFB]); }
        catch { await exec("ip", ["link", "add", IFB, "type", "ifb"]); }
        await exec("ip", ["link", "set", IFB, "up"]);
        await exec("tc", ["qdisc", "replace", "dev", IFB, "root", "cake",
            "bandwidth", `${s.upMbit}mbit`, "besteffort", "dual-srchost", "overhead", String(oh)]);
        if (!(await hasIngressRedirect())) {
            await exec("tc", ["qdisc", "del", "dev", iface, "ingress"]).catch(() => undefined);
            await exec("tc", ["qdisc", "add", "dev", iface, "handle", "ffff:", "ingress"]);
            await exec("tc", ["filter", "add", "dev", iface, "parent", "ffff:", "protocol", "all", "prio", "1",
                "matchall", "action", "mirred", "egress", "redirect", "dev", IFB]);
        }

        await nftApply([...heavySet]);
        lastError = "";
        logger.info("shaping applied", { down: s.downMbit, up: s.upMbit, overhead: oh, heavy: heavySet.size });
    }

    async function removeNow() {
        await exec("tc", ["qdisc", "del", "dev", iface, "root"]).catch(() => undefined);
        await exec("tc", ["qdisc", "del", "dev", iface, "ingress"]).catch(() => undefined);
        await exec("ip", ["link", "del", IFB]).catch(() => undefined);
        await exec("nft", ["delete", "table", "inet", NFT_TABLE]).catch(() => undefined);
        heavySet.clear(); history.clear(); totals.length = 0; congested = false;
    }

    // Не бросает: отсутствие tc/nft/модуля не должно валить awg-ctrl (в отличие от ensureInterfaceUp).
    function apply(): Promise<void> {
        return serial(applyNow).catch(noteError);
    }

    async function update(s: ShapingSettings): Promise<void> {
        save(s);
        await serial(applyNow);     // пользователю нужна честная ошибка, а не молчаливый warn
        lastError = "";
    }

    // ───────────── сэмплер (RFC 6057) ─────────────
    const prev     = new Map<string, { rx: number; tx: number }>();
    const history  = new Map<string, Tick[]>();     // загрузка по IP
    const totals: Tick[] = [];                      // суммарная загрузка (всё, что ушло клиентам)
    const heavySet = new Set<string>();
    let congested = false;
    let lastRateDown = 0, lastRateUp = 0;
    let lastTickAt = 0;
    let tickNo = 0;
    const windowTicks = Math.max(3, Math.round(WINDOW_S / SAMPLE_S));

    const push = (arr: Tick[], t: Tick) => { arr.push(t); while (arr.length > windowTicks) arr.shift(); };
    const mbit = (arr: Tick[]) => {
        const dt = arr.reduce((a, t) => a + t.dt, 0);
        return dt > 0 ? arr.reduce((a, t) => a + t.bytes, 0) * 8 / dt / 1e6 : 0;
    };

    async function tick() {
        const s = settings();
        if (!s.enabled) return;
        const now = Date.now();
        const dt  = lastTickAt ? (now - lastTickAt) / 1000 : SAMPLE_S;
        lastTickAt = now;
        tickNo++;

        // Страховка: интерфейс могли поднять руками (awg-quick down/up) — qdisc тогда исчез.
        if (tickNo % CHECK_EVERY === 0 && !(await hasCake(iface))) {
            logger.warn("shaping: qdisc на интерфейсе пропал, применяю заново");
            await apply();
        }

        let peers: PeerCounters[];
        try { peers = await deps.peers(); } catch (e) { noteError(e); return; }

        let totalDown = 0, totalUp = 0;
        const seen = new Set<string>();
        for (const p of peers) {
            seen.add(p.ip);
            const before = prev.get(p.ip);
            // Счётчики ядра сбрасываются при down/up интерфейса и замене ключа пира → отрицательная дельта = 0.
            const dTx = before && p.tx >= before.tx ? p.tx - before.tx : 0;
            const dRx = before && p.rx >= before.rx ? p.rx - before.rx : 0;
            prev.set(p.ip, { rx: p.rx, tx: p.tx });
            totalDown += dTx; totalUp += dRx;
            let h = history.get(p.ip);
            if (!h) history.set(p.ip, h = []);
            push(h, { dt, bytes: dTx });
        }
        for (const ip of [...prev.keys()]) if (!seen.has(ip)) { prev.delete(ip); history.delete(ip); heavySet.delete(ip); }
        push(totals, { dt, bytes: totalDown });
        lastRateDown = totalDown * 8 / dt / 1e6;
        lastRateUp   = totalUp   * 8 / dt / 1e6;

        if (!s.auto) {
            if (heavySet.size || congested) { heavySet.clear(); congested = false; await serial(() => nftApply([])).catch(noteError); }
            return;
        }
        if (totals.length < windowTicks) return;    // пока окно не набралось, решений не принимаем

        const avg = mbit(totals);
        const wasCongested = congested;
        if (!congested && avg >= CONGEST_ON  * s.downMbit) congested = true;
        if ( congested && avg <  CONGEST_OFF * s.downMbit) congested = false;

        const nextHeavy = new Set<string>();
        let active = 0;
        if (congested) {
            const rates = [...history.entries()].filter(([, h]) => h.length >= windowTicks).map(([ip, h]) => [ip, mbit(h)] as const);
            active = rates.filter(([, r]) => r >= ACTIVE_MBIT).length;
            const share = s.downMbit / Math.max(1, active);
            for (const [ip, r] of rates) {
                const wasHeavy = heavySet.has(ip);
                // Входим на 1.5 доли, выходим ниже 1.0 доли — без дребезга на границе.
                if (r >= Math.max(HEAVY_MULT * share, HEAVY_MIN_FRAC * s.downMbit) || (wasHeavy && r >= share)) nextHeavy.add(ip);
            }
        }

        const changed = nextHeavy.size !== heavySet.size || [...nextHeavy].some(ip => !heavySet.has(ip));
        if (changed || wasCongested !== congested) {
            heavySet.clear(); nextHeavy.forEach(ip => heavySet.add(ip));
            logger.info("shaping: класс тяжёлых обновлён", { congested, avgMbit: Math.round(avg), heavy: [...heavySet] });
            await serial(() => nftApply([...heavySet])).catch(noteError);
        }
    }

    let timer: NodeJS.Timeout | null = null;
    function start() {
        if (timer) return;
        let busy = false;
        timer = setInterval(() => {
            if (busy) return;       // предыдущий тик ещё идёт — пропускаем, а не копим очередь
            busy = true;
            tick().catch(noteError).finally(() => { busy = false; });
        }, SAMPLE_S * 1000);
        timer.unref();
    }

    // ───────────── состояние ─────────────
    async function qdiscStats(dev: string): Promise<QdiscStats | undefined> {
        try {
            const arr = JSON.parse(await exec("tc", ["-s", "-j", "qdisc", "show", "dev", dev, "root"])) as Record<string, unknown>[];
            const q = arr.find(x => x.kind === "cake");
            if (!q) return undefined;
            return {
                packets: Number(q.packets) || 0, bytes: Number(q.bytes) || 0,
                drops:   Number(q.drops)   || 0, backlog: Number(q.backlog) || 0,
            };
        } catch { return undefined; }
    }

    async function status(): Promise<ShapingStatus> {
        const s = settings();
        const [active, down, up] = s.enabled
            ? await Promise.all([hasCake(iface), qdiscStats(iface), qdiscStats(IFB)])
            : [false, undefined, undefined];
        return {
            settings: s, active, congested, heavy: [...heavySet],
            devices:  [...history.values()].filter(h => mbit(h) >= ACTIVE_MBIT).length,
            rateDown: Math.round(lastRateDown * 10) / 10,
            rateUp:   Math.round(lastRateUp   * 10) / 10,
            down, up,
        };
    }

    // Текущий приоритет устройства по его внутреннему IP: «фоновый» — пока он в bulk-классе.
    const priorityOf = (ip: string): "normal" | "background" => (heavySet.has(ip) ? "background" : "normal");

    return { settings, update, apply, start, status, priorityOf };
}

export type Shaping = ReturnType<typeof createShaping>;
