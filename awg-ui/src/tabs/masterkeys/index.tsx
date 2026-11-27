// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { Fragment, useState, useEffect, useCallback, useMemo, useRef, type ReactNode } from 'react';
import {
    apiFetch, copyText, bytes, timeAgo, getServerId, cmpVersion, classifyApiError,
    limitBytes, limitDate, limitDay, timeLeft, inputDate, parseInputDate, blockText,
    LIMIT_UNITS, limitAmount, limitUnit, parseLimitAmount, TB, type LimitUnit, type BlockReason,
    type NodeInfo, type PageProps, type Priority, type ApiFailure,
} from '../../lib/shared';
import { qrDataUrl } from '../../lib/qr';
import { IcoPlus, IcoTrash, IcoCopy, IcoQR, IcoKey, IcoRefresh, IcoSettings, IcoMore, IcoClose } from '../../components/icons';
import ServerPicker, { serverName } from '../../components/ServerPicker';
import PriorityChip from '../../components/PriorityChip';
import TorrentChip from '../../components/TorrentChip';
import BulkServersModal from './BulkServersModal';
import './masterkeys.css';
import '../users/load-state.css';

// Вкладка «Мастер-ключи»: sen://-подписки для SenAWG. Мастер-ключ — это ссылка
// плюс лимит устройств; устройства регистрируются сами (приватный ключ остаётся
// на них) и здесь только отображаются и отзываются. Формат — docs/sen-link.md.
// deleting — ключ удалён и уже не работает, но эти серверы ещё не подтвердили, что сняли его пиров.
// Лимиты срока и трафика — те же поля, что у vpn:// пользователей (expiresAt и далее);
// старый awg-ctrl их не отдаёт, поэтому всё необязательное.
interface MasterKey {
    id: number; uuid: string; label: string; device_limit: number; devices: number; servers: number[]; created_at: number; deleting?: number[];
    expiresAt?: number | null; trafficLimitBytes?: number | null; usedBytes?: number; remainingBytes?: number | null;
    blocked?: boolean; blockReason?: BlockReason | null; expired?: boolean;
}
interface Device {
    id: number; device_id: string; device_name: string; platform: string; version: string; created_at: number; last_seen: number | null;
    rekey_requested: boolean; online: boolean; lastHandshake: number; rx: number; tx: number; priority?: Priority; torrent?: boolean;
    servers_total: number; servers_ok: number;   // на скольких серверах ключа уже стоит его текущий ключ
    ips?: { server_id: number; ip: string }[];   // IP на каждом сервере ключа (старая панель не отдаёт)
}

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

// Узкая таблица (1280–1439): подпись трафика короче, иначе строка лимитов не влезает.
// На карточках места хватает — там всегда с единицами: «940 ГБ из 1 ТБ».
const COMPACT_LIMITS = '(min-width: 1280px) and (max-width: 1439px)';
function useCompactLimits() {
    const [compact, setCompact] = useState(() => window.matchMedia(COMPACT_LIMITS).matches);
    useEffect(() => {
        const mq = window.matchMedia(COMPACT_LIMITS);
        const on = () => setCompact(mq.matches);
        mq.addEventListener('change', on);
        return () => mq.removeEventListener('change', on);
    }, []);
    return compact;
}

// Статус — как у пользователей: «онлайн», иначе сколько прошло с последнего handshake.
// Приоритет показываем, только когда он не обычный: «обычный» в каждой строке — шум
const bgOnly = (p?: Priority) => p === 'background' ? p : undefined;

// Статус устройства. У заблокированного ключа сетевая активность ничего не значит:
// устройства не подключаются, поэтому чип говорит о доступе, а не об онлайне.
const statusChip = (d: Device, blocked = false) => blocked ? (
    <span className="chip chip--offline" title="Ключ заблокирован — устройство не подключается">
        <span className="chip-dot" />заблокирован
    </span>
) : (
    <span className={`chip chip--${d.online ? 'online' : 'offline'}`}>
        <span className="chip-dot" />
        {d.online ? 'онлайн' : d.lastHandshake ? timeAgo(d.lastHandshake) + ' назад' : 'никогда'}
    </span>
);

// ── Лимиты срока и трафика ключа ──
// Один чип на ключ: заблокирован (срок/трафик/вручную), «в порядке» или «без лимитов».
// Срок вышел — доступ закрыт, даже если бэкенд ещё не выставил blocked.
const mkOver = (k: MasterKey) => !!k.blocked || !!k.expired;
const mkReason = (k: MasterKey): BlockReason | null =>
    k.blockReason ?? (k.expired ? 'expired' : k.blocked ? 'manual' : null);
const mkLimited = (k: MasterKey) =>
    (k.trafficLimitBytes != null && k.trafficLimitBytes > 0) || (k.expiresAt != null && k.expiresAt > 0);

// «Сбросить счётчик» нужен, только когда есть что сбрасывать: израсходован трафик или ключ
// закрыт по трафику. Ключ без лимита трафика его не считает, а пустой счётчик сбрасывать незачем.
const mkResetOn = (k: MasterKey) => (k.usedBytes ?? 0) > 0 || k.blockReason === 'traffic';
const mkResetTip = (k: MasterKey): string => {
    if (mkResetOn(k)) return 'Обнулить израсходованный трафик';
    return (k.trafficLimitBytes ?? 0) > 0 ? 'Трафик ещё не израсходован' : 'Нет лимита трафика — счётчик не ведётся';
};

// Чип состояния ключа — стоит у имени, поэтому доступ читается раньше сетевой активности.
// Тексты общие с вкладкой «Пользователи» (blockText).
const mkChip = (k: MasterKey) => {
    if (mkOver(k)) return <span className="chip chip--error"><span className="chip-dot" />{blockText(mkReason(k))}</span>;
    if (!mkLimited(k)) return <span className="chip chip--offline"><span className="chip-dot" />без лимитов</span>;
    return <span className="chip chip--online"><span className="chip-dot" />в порядке</span>;
};

// «940 ГБ из 1 ТБ» и тонкая шкала; у ключа без лимита трафика строки нет.
// Подпись остатка: на широком экране «940 ГБ из 1 ТБ», на узком — «940 / 1 ТБ», чтобы
// срок, число и шкала влезали в одну строку без переносов и без обрезаний чисел.
const usageLabel = (used: number, limit: number, compact: boolean): string =>
    compact
        ? `${limitBytes(used).split('\u00A0')[0]} / ${limitBytes(limit)}`
        : `${limitBytes(used)} из ${limitBytes(limit)}`;

const mkUsage = (k: MasterKey, compact = false) => {
    const limit = k.trafficLimitBytes ?? null;
    if (limit == null || limit <= 0) return null;
    const used = k.usedBytes ?? 0;
    const pct = Math.min(100, Math.round((used / limit) * 100));
    // Красная шкала — только когда трафик реально исчерпан; ручная блокировка красит чип статуса.
    const exhausted = used >= limit;
    return (
        <span className="mk-lim-usage">
            <span className={`mk-lim-text${exhausted ? ' mk-lim-text--over' : ''}`}>
                {usageLabel(used, limit, compact)}
            </span>
            <span className="mk-lim-bar" role="progressbar" aria-label="Израсходовано трафика"
                aria-valuemin={0} aria-valuemax={limit} aria-valuenow={Math.min(used, limit)}>
                <span className={`mk-lim-bar-fill${exhausted ? ' mk-lim-bar-fill--over' : pct >= 85 ? ' mk-lim-bar-fill--warn' : ''}`}
                    style={{ width: `${pct}%` }} />
            </span>
        </span>
    );
};

// Подсказки под полями тулбокса: полная дата с остатком и пересчёт лимита в байты.
const mkDateHint = (v: string): { text: string; warm?: boolean } => {
    if (!v) return { text: 'Без ограничения по сроку' };
    const ts = parseInputDate(v);
    if (ts === null || Number.isNaN(ts)) return { text: 'Дата не распознана', warm: true };
    return { text: `${limitDate(ts)} · ${timeLeft(ts)}`, warm: ts * 1000 <= Date.now() };
};
const mkTrafficHint = (v: string, unit: LimitUnit): string => {
    const b = parseLimitAmount(v, unit);
    if (b === null) return 'Без ограничения по трафику';
    if (Number.isNaN(b) || b <= 0) return 'Нужно положительное число';
    if (b < 1024 ** 2) return 'Минимум 1 МБ';
    return `= ${limitBytes(b)}`;
};

// IP устройства на выбранном сервере (на каждом сервере ключа он свой), иначе — на первом;
// other — адрес с другого сервера, его надо подписать, иначе его примут за адрес выбранного.
const ipPick = (d: Device): { ip: string; serverId?: number; other: boolean } => {
    const own = d.ips?.find(x => x.server_id === getServerId());
    const pick = own ?? d.ips?.[0];
    return { ip: pick?.ip ?? '—', serverId: pick?.server_id, other: !own && !!pick };
};

export default function MasterKeysPage({ token, showMsg }: PageProps) {
    const compactLimits = useCompactLimits();
    const [keys, setKeys]       = useState<MasterKey[]>([]);
    // Почему список не загрузился: панель без локального VPN / сервер не на связи / ошибка.
    // Пустой список и «сервер не настроен или недоступен» — разные вещи, и говорить
    // «Нет мастер-ключей» во втором случае нельзя.
    const [fail, setFail]       = useState<ApiFailure | null>(null);
    const [enabled, setEnabled] = useState(true);
    const [tls, setTls]         = useState(false);
    const [nodes, setNodes]     = useState<NodeInfo[]>([]);   // серверы, которые можно включить в ключ
    // Маскировка первого пакета (I1) сервера из шапки: false — сервер её не умеет (старый awg-ctrl) или недоступен
    const [masking, setMasking] = useState<'ready' | 'busy' | false>(false);
    const [label, setLabel]     = useState('');
    const [limit, setLimit]     = useState('3');
    const [devs, setDevs]       = useState<Record<number, Device[]>>({});
    const [folded, setFolded]   = useState<Set<number>>(new Set());   // свёрнутые узлы
    // Модалка «Настройки: <ключ>»: id ключа, элемент-открыватель (для возврата фокуса)
    // и признак сохранения (пока идёт — подложка и Escape не закрывают).
    const [tools, setTools]     = useState<number | null>(null);
    const [saving, setSaving]   = useState(false);
    const savingRef             = useRef(false);
    const openerRef             = useRef<HTMLElement | null>(null);
    const modalRef              = useRef<HTMLDivElement>(null);
    // Меню «⋮» ключа или устройства. position: fixed по кнопке: .table-card прокручивается вбок
    // и обрезал бы меню у нижних строк. full — в меню ключа и «Скопировать ссылку» (в таблице;
    // в карточке на телефоне ссылка — отдельной кнопкой).
    const [menu, setMenu]       = useState<{ kind: 'key' | 'device'; id: number; full: boolean; top: number; right: number; up: boolean } | null>(null);
    const menuRef = useRef<HTMLDivElement>(null);
    const [limitEdit, setLimitEdit] = useState('');
    // Лимиты срока и трафика в тулбоксе ключа: поля строками (пусто — без ограничения).
    const [mkExp, setMkExp]         = useState('');
    const [mkTraffic, setMkTraffic] = useState('');
    const [mkUnit, setMkUnit]       = useState<LimitUnit>('ГБ');
    const [mkErr, setMkErr]         = useState<{ date?: string; amount?: string; general?: string }>({});
    const [failed, setFailed] = useState<Set<number>>(new Set());   // ключи, чьи устройства не загрузились
    const [down, setDown]     = useState<Record<number, number[]>>({});   // серверы ключа, не ответившие на статистику
    const [creating, setCreating] = useState(false);
    const loadSeq = useRef(0);       // номер последней загрузки: ответ более старой отбрасывается
    const creatingRef = useRef(false);
    useEffect(() => { savingRef.current = saving; }, [saving]);
    const [modal, setModal]     = useState<{ label: string; link: string; tls: boolean; dataUrl: string } | null>(null);
    const [defaults, setDefaults] = useState<number[] | null>(null);   // серверы новых ключей из «Серверы для всех»
    const [bulk, setBulk]       = useState(false);

    const load = useCallback(async () => {
        const seq = ++loadSeq.current;
        try {
            const d = await apiFetch('GET', '/ui/masterkeys', token);
            const list: MasterKey[] = d.keys ?? [];
            if (seq !== loadSeq.current) return;
            setFail(null);
            setKeys(list); setEnabled(!!d.enabled); setTls(!!d.tls); setDefaults(d.defaults ?? null);
            apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
            // Каждый ключ грузится сам по себе: сбой одного не должен прятать устройства остальных.
            const live = list.filter(k => !k.deleting);
            const res = await Promise.allSettled(live.map(k =>
                apiFetch('GET', `/ui/masterkeys/${k.id}/devices`, token)));
            if (seq !== loadSeq.current) return;
            const bad = new Set<number>(), unreachable: Record<number, number[]> = {};
            const fresh: Record<number, Device[]> = {};
            res.forEach((r, i) => {
                const id = live[i].id;
                if (r.status === 'fulfilled') {
                    fresh[id] = r.value.devices ?? [];
                    if (r.value.unreachable?.length) unreachable[id] = r.value.unreachable;
                } else bad.add(id);
            });
            // Не загрузилось — оставляем прежний список (устаревший лучше пустого), но помечаем.
            setDevs(prev => Object.fromEntries(live.flatMap(k =>
                fresh[k.id] ? [[k.id, fresh[k.id]]] : prev[k.id] ? [[k.id, prev[k.id]]] : [])));
            setFailed(bad); setDown(unreachable);
        } catch (e) {
            // Сервер без локального VPN и недоступную ноду показываем блоком на месте списка,
            // а тост оставляем только настоящей ошибке. Загруженный ранее список не трогаем.
            const why = classifyApiError(e);
            if (seq === loadSeq.current) setFail(why);
            if (why === 'error') showMsg('Ошибка загрузки мастер-ключей');
        }
    }, [token, showMsg]);

    // Действие над ключом/устройством: выполнить, показать сообщение, обновить данные.
    const act = useCallback(async (fn: () => Promise<unknown>, ok: string) => {
        try { await fn(); showMsg(ok); await load(); }
        catch (e) { showMsg(errText(e)); }
    }, [load, showMsg]);

    const create = useCallback(async () => {
        if (!label.trim() || creatingRef.current) return;
        creatingRef.current = true; setCreating(true);
        try { await act(async () => {
            // Новый ключ — с серверами из «Серверы для всех» (их подставит панель), а не задано — со всеми,
            // которые уже подключались; набор потом правится в настройках ключа.
            const ready = nodes.filter(n => !n.pending).map(n => n.id);
            await apiFetch('POST', '/ui/masterkeys', token, {
                label: label.trim(), device_limit: Number(limit), ...(!defaults && ready.length ? { servers: ready } : {}),
            });
            setLabel('');
        }, 'Мастер-ключ создан'); }
        finally { creatingRef.current = false; setCreating(false); }
    }, [label, limit, nodes, defaults, token, act]);
    const live = useMemo(() => keys.filter(k => !k.deleting), [keys]);

    useEffect(() => {
        let alive = true;
        apiFetch('GET', '/api/masking', token).then(() => { if (alive) setMasking('ready'); }).catch(() => {});
        return () => { alive = false; };
    }, [token]);

    // Новый случайный I1 на сервере из шапки. Интерфейс не трогается: I1 нужен только клиентам,
    // sen:// подхватят его со следующим обновлением профиля (≤ 60 с), выданные vpn:// остаются со старым.
    const regenMasking = async () => {
        setMasking('busy');
        try { await apiFetch('POST', '/api/masking', token); showMsg('Маскировка сменена'); }
        catch (e: any) { showMsg(e?.response?.data?.error ?? 'Не удалось сменить маскировку'); }
        finally { setMasking('ready'); }
    };

    const serverNames = useCallback((ids: number[]) => ids.map(id => serverName(nodes, id)).join(', '), [nodes]);

    const remove = useCallback(async (k: MasterKey) => {
        if (!confirm(`Удалить «${k.label}»? Все ${k.devices} устройств потеряют доступ.`)) return;
        try {
            const r = await apiFetch('DELETE', `/ui/masterkeys/${k.id}`, token);
            const pending: number[] = r.pending ?? [];
            showMsg(pending.length ? `«${k.label}» отключён, ждём подтверждения: ${serverNames(pending)}` : 'Удалён: ' + k.label);
            await load();
        } catch (e) { showMsg(errText(e)); }
    }, [token, load, showMsg, serverNames]);

    const fetchLink = useCallback(async (k: MasterKey) => {
        const r = await apiFetch('GET', `/ui/masterkeys/${k.id}/link`, token);
        return r as { link: string; tls: boolean };
    }, [token]);

    const showQR = useCallback(async (k: MasterKey) => {
        try {
            const { link, tls: t } = await fetchLink(k);
            setModal({ label: k.label, link, tls: t, dataUrl: qrDataUrl(link, 'H') });
        } catch (e) { showMsg(errText(e)); }
    }, [fetchLink, showMsg]);

    const copyLink = useCallback(async (k: MasterKey) => {
        try { await copyText((await fetchLink(k)).link); showMsg('Ссылка скопирована'); }
        catch (e) { showMsg(errText(e)); }
    }, [fetchLink, showMsg]);

    const fold = (id: number) => setFolded(prev => {
        const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n;
    });
    // Открыть настройки ключа в модалке: поля заполняются из строки, фокус вернётся
    // на кнопку-открыватель (шестерёнка, «⋮» или двойной клик по строке).
    const openSettings = useCallback((k: MasterKey, opener?: HTMLElement | null) => {
        openerRef.current = opener ?? null;
        setLimitEdit(String(k.device_limit));
        setMkExp(inputDate(k.expiresAt));
        setMkTraffic(limitAmount(k.trafficLimitBytes));
        setMkUnit(limitUnit(k.trafficLimitBytes));
        setMkErr({});
        setSaving(false);
        setTools(k.id);
    }, []);

    const closeSettings = useCallback(() => {
        if (savingRef.current) return;   // не закрываем на середине сохранения
        setTools(null);
        setMkErr({});
    }, []);

    // Одно сохранение на форму: лимит устройств, срок и лимит трафика — один PATCH.
    // Пустое поле = null = «без ограничения»; ошибки сервера показываем в модалке.
    const saveSettings = useCallback(async (k: MasterKey) => {
        if (savingRef.current) return;
        const errs: { date?: string; amount?: string; general?: string } = {};
        const limit = Number(limitEdit);
        if (!Number.isInteger(limit) || limit < 1 || limit > 100) errs.general = 'Лимит устройств: целое число от 1 до 100';
        let expires: number | null = null;
        if (mkExp) {
            const ts = parseInputDate(mkExp);
            if (ts === null || Number.isNaN(ts)) errs.date = 'Некорректная дата'; else expires = ts;
        }
        let traffic: number | null = null;
        const b = parseLimitAmount(mkTraffic, mkUnit);
        if (b !== null) {
            if (Number.isNaN(b)) errs.amount = 'Нужно число, например 250';
            else if (b < 1024 ** 2) errs.amount = 'Минимум 1 МБ';
            else if (b > 1024 * TB) errs.amount = 'Больше 1024 ТБ не бывает';
            else traffic = b;
        }
        if (errs.general || errs.date || errs.amount) { setMkErr(errs); return; }
        // Устройств больше нового лимита — сервер отвяжет лишних, начиная с подключившихся последними.
        const list = devs[k.id];
        const have = list?.length ?? k.devices;
        const extra = have - limit;
        if (extra > 0) {
            const who = list
                ? [...list].sort((a, b) => b.created_at - a.created_at || b.id - a.id).slice(0, extra)
                    .map(d => `«${d.device_name || d.id}»`).join(', ')
                : `${extra} шт.`;
            if (!confirm(`Лимит ${limit} меньше, чем устройств (${have}). Будут отвязаны подключившиеся последними: ${who}. Продолжить?`)) return;
        }
        setSaving(true); setMkErr({});
        try {
            const r = await apiFetch('PATCH', `/ui/masterkeys/${k.id}`, token,
                { device_limit: limit, expires_at: expires, traffic_limit_bytes: traffic });
            const n: number = r.unbound?.length ?? 0;
            showMsg(n ? `Настройки сохранены, отвязано устройств: ${n}` : 'Настройки сохранены');
            setSaving(false);
            setTools(null);
            setMkErr({});
            await load();
        } catch (e: any) {
            const d = e?.response?.data ?? {};
            setSaving(false);
            setMkErr({
                general: d.error ?? 'Не удалось сохранить настройки',
                date: d.errors?.expires_at, amount: d.errors?.traffic_limit_bytes,
            });
        }
    }, [limitEdit, mkExp, mkTraffic, mkUnit, devs, token, showMsg, load]);

    // Действия внутри модалки (счётчик, блокировка, ссылка, серверы): успех — тостом,
    // ошибка — строкой в самой модалке, а не за её пределами.
    const modalAct = useCallback(async (fn: () => Promise<unknown>, ok: string) => {
        setMkErr({});
        try { await fn(); showMsg(ok); await load(); }
        catch (e) { setMkErr({ general: errText(e) }); }
    }, [showMsg, load]);

    // Фокус-трап, Escape, клик по подложке и блокировка прокрутки страницы под модалкой.
    useEffect(() => {
        if (tools === null) return;
        const opener = openerRef.current;
        const main = document.querySelector<HTMLElement>('.main');
        const prevMain = main?.style.overflow ?? '';
        const prevBody = document.body.style.overflow;
        if (main) main.style.overflow = 'hidden';
        document.body.style.overflow = 'hidden';
        const focusables = () => [...(modalRef.current?.querySelectorAll<HTMLElement>(
            'button:not([disabled]), input:not([disabled]), select:not([disabled]), [href], [tabindex]:not([tabindex="-1"])') ?? [])]
            .filter(el => el.offsetParent !== null);
        modalRef.current?.focus();
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape') {
                e.preventDefault();
                if (!savingRef.current) closeSettings();
                return;
            }
            if (e.key !== 'Tab') return;
            const list = focusables();
            if (!list.length) return;
            const first = list[0], last = list[list.length - 1];
            const active = document.activeElement as HTMLElement | null;
            const inside = active ? modalRef.current?.contains(active) : false;
            if (!e.shiftKey && (!inside || active === last)) { e.preventDefault(); first.focus(); }
            else if (e.shiftKey && (!inside || active === first)) { e.preventDefault(); last.focus(); }
        };
        document.addEventListener('keydown', onKey);
        return () => {
            document.removeEventListener('keydown', onKey);
            if (main) main.style.overflow = prevMain;
            document.body.style.overflow = prevBody;
            requestAnimationFrame(() => opener?.focus?.());
        };
    }, [tools, closeSettings]);

    useEffect(() => { if (token) load(); }, [token, load]);

    // Пока какой-то ключ ждёт серверы, время от времени проверяем, не подтвердили ли они.
    const waiting = keys.some(k => k.deleting);
    useEffect(() => {
        if (!waiting) return;
        const t = setInterval(load, 15_000);
        return () => clearInterval(t);
    }, [waiting, load]);

    const deletingChip = (k: MasterKey) => (
        <span className="chip chip--offline" title="Ключ уже не работает; строка исчезнет, когда серверы подтвердят, что сняли его устройства">
            <span className="chip-dot" />Удаляется — ждёт: {serverNames(k.deleting ?? [])}
        </span>
    );

    useEffect(() => {
        if (!menu) return;
        menuRef.current?.querySelector<HTMLButtonElement>('button')?.focus();
        const close = () => setMenu(null);
        const down = (e: MouseEvent) => { if (!menuRef.current?.contains(e.target as Node)) close(); };
        const key = (e: KeyboardEvent) => {
            if (e.key === 'Escape') { close(); document.querySelector<HTMLButtonElement>(`[data-menu-for="${menu.kind}-${menu.id}"]`)?.focus(); return; }
            if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
            e.preventDefault();
            const items = [...(menuRef.current?.querySelectorAll<HTMLButtonElement>('button') ?? [])];
            const i = items.indexOf(document.activeElement as HTMLButtonElement);
            items[(i + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length]?.focus();
        };
        document.addEventListener('mousedown', down);
        document.addEventListener('keydown', key);
        window.addEventListener('scroll', close, true);
        window.addEventListener('resize', close);
        return () => {
            document.removeEventListener('mousedown', down);
            document.removeEventListener('keydown', key);
            window.removeEventListener('scroll', close, true);
            window.removeEventListener('resize', close);
        };
    }, [menu]);

    const openMenu = (kind: 'key' | 'device', id: number, el: HTMLElement, full = false) => {
        if (menu?.kind === kind && menu.id === id) return setMenu(null);
        const r = el.getBoundingClientRect();
        const up = r.bottom + 160 > window.innerHeight;   // у нижнего края — вверх от кнопки
        setMenu({ kind, id, full, top: up ? r.top - 4 : r.bottom + 4, right: window.innerWidth - r.right, up });
    };
    const moreButton = (kind: 'key' | 'device', id: number, label: string, full = false) => (
        <button className="btn-icon mk-more" aria-label={label} aria-haspopup="menu" aria-expanded={menu?.kind === kind && menu.id === id}
            data-menu-for={`${kind}-${id}`} onClick={e => openMenu(kind, id, e.currentTarget, full)}><IcoMore /></button>
    );

    // В таблице — QR, шестерёнка настроек и «⋮» (остальное в меню).
    // Подсказки — .tip-wrap, не title: нативная появляется с задержкой и бесполезна на тач-экране.
    const actions = (k: MasterKey) => (
        <div className="actions">
            <span className="tip-wrap tip-wrap--end" data-tip="QR-код"><button className="btn-icon" aria-label="QR-код" onClick={() => showQR(k)}><IcoQR /></button></span>
            <span className="tip-wrap tip-wrap--end" data-tip="Настройки">
                <button className="btn-icon" aria-label={`Настройки: ${k.label}`} aria-haspopup="dialog"
                    onClick={e => openSettings(k, e.currentTarget)}><IcoSettings /></button>
            </span>
            {moreButton('key', k.id, `Ещё: ${k.label}`, true)}
        </div>
    );

    // Тело модалки «Настройки: <ключ>». Тот же набор, что был в раскрытии строки, но в одном
    // окне: поля лимитов сохраняет футер, остальные действия применяются сразу.
    const settingsBody = (k: MasterKey) => (
        <div className="mk-body">
            <section className="mk-box">
                <h3 className="mk-box-title">Ключ</h3>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">UUID</span>
                        <code className="mk-uuid" title={k.uuid}>{k.uuid}</code>
                        <span className="mk-setting-hint">Постоянный: перевыпуск ссылки его не меняет</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <button className="btn btn--tonal" onClick={() =>
                            copyText(k.uuid).then(() => showMsg('UUID скопирован'), () => showMsg('Не удалось скопировать'))}>
                            <IcoCopy /> Скопировать
                        </button>
                    </div>
                </div>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <label className="mk-setting-name" htmlFor={`mk-limit-${k.id}`}>Лимит устройств</label>
                        <span className="mk-setting-hint">Сколько устройств может подключить ключ</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <input id={`mk-limit-${k.id}`} className="field mk-limit-input" type="number" min={1} max={100}
                            value={limitEdit} onChange={e => setLimitEdit(e.target.value)} />
                    </div>
                </div>
                {/* Срок и трафик: пустые поля снимают ограничение; сохраняются вместе с лимитом устройств */}
                <div className="mk-setting mk-setting--wide">
                    <div className="mk-setting-text">
                        <label className="mk-setting-name" htmlFor={`mk-exp-${k.id}`}>Срок и трафик</label>
                        <span className="mk-setting-hint">Пусто — без ограничения</span>
                    </div>
                    <div className="mk-setting-ctl mk-lim-ctl">
                        <input id={`mk-exp-${k.id}`} className="field mk-date-input" type="datetime-local" aria-label="Действует до"
                            value={mkExp} onChange={e => {
                                const v = e.target.value;
                                setMkExp(v); setMkErr(p => ({ ...p, date: undefined }));
                            }} />
                        {mkErr.date
                            ? <span className="mk-lim-err">{mkErr.date}</span>
                            : <span className={`mk-lim-hint${mkDateHint(mkExp).warm ? ' mk-lim-hint--warm' : ''}`}>{mkDateHint(mkExp).text}</span>}
                        <span className="mk-lim-pair">
                            <input id={`mk-traffic-${k.id}`} className="field mk-traffic-input" type="number" min={0} step="any" inputMode="decimal"
                                placeholder="без ограничения" aria-label="Лимит трафика" value={mkTraffic}
                                onChange={e => {
                                    const v = e.target.value;
                                    setMkTraffic(v); setMkErr(p => ({ ...p, amount: undefined }));
                                }} />
                            <select className="field mk-unit" aria-label="Единица лимита" value={mkUnit}
                                onChange={e => setMkUnit(e.target.value as LimitUnit)}>
                                {LIMIT_UNITS.map(x => <option key={x} value={x}>{x}</option>)}
                            </select>
                        </span>
                        {mkErr.amount
                            ? <span className="mk-lim-err">{mkErr.amount}</span>
                            : <span className="mk-lim-hint">{mkTrafficHint(mkTraffic, mkUnit)}</span>}
                    </div>
                </div>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">Счётчик трафика</span>
                        <span className="mk-setting-hint">
                            Израсходовано {limitBytes(k.usedBytes ?? 0)}
                            {k.trafficLimitBytes ? ` из ${limitBytes(k.trafficLimitBytes)}` : ' · лимита нет'}
                            {k.blockReason === 'traffic' ? ' · доступ закрыт по трафику' : ''}
                        </span>
                    </div>
                    <div className="mk-setting-ctl">
                        <span className="tip-wrap tip-wrap--end" data-tip={mkResetTip(k)}>
                            <button className="btn btn--tonal" disabled={!mkResetOn(k)} onClick={() =>
                                modalAct(() => apiFetch('POST', `/ui/masterkeys/${k.id}/usage/reset`, token), 'Счётчик трафика сброшен')}>
                                <IcoRefresh /> Сбросить счётчик
                            </button>
                        </span>
                    </div>
                </div>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">Доступ</span>
                        <span className="mk-setting-hint">{mkOver(k) ? blockText(mkReason(k)) : 'открыт'}</span>
                    </div>
                    <div className="mk-setting-ctl">
                        {mkOver(k) ? (
                            <button className="btn btn--tonal" onClick={() =>
                                modalAct(() => apiFetch('POST', `/ui/masterkeys/${k.id}/unblock`, token), 'Ключ разблокирован')}>
                                Разблокировать
                            </button>
                        ) : (
                            <button className="btn btn--danger" onClick={() =>
                                modalAct(() => apiFetch('POST', `/ui/masterkeys/${k.id}/block`, token), 'Ключ заблокирован')}>
                                Заблокировать
                            </button>
                        )}
                    </div>
                </div>
            </section>

            <section className="mk-box">
                <h3 className="mk-box-title">Ссылка и ключи</h3>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">Ссылка</span>
                        <span className="mk-setting-hint">Старая перестанет принимать новые устройства</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <button className="btn btn--tonal" onClick={() => {
                            if (confirm('Выпустить новую ссылку? Старая перестанет принимать новые устройства, уже добавленные продолжат работать.'))
                                modalAct(() => apiFetch('POST', `/ui/masterkeys/${k.id}/rotate`, token), 'Ссылка перевыпущена');
                        }}>Перевыпустить</button>
                    </div>
                </div>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">Ключи устройств</span>
                        <span className="mk-setting-hint">Клиенты сменят их при следующем опросе</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <button className="btn btn--tonal" onClick={() =>
                            modalAct(() => apiFetch('POST', `/ui/masterkeys/${k.id}/rekey`, token), 'Смена ключей запрошена')}>
                            Запросить смену
                        </button>
                    </div>
                </div>
            </section>

            {/* Какие серверы отдаёт ключ: в config.servers[] каждого устройства. Снять галочку —
                пиры его устройств с этого сервера удалятся; поставить — добавятся. Последний не снимается. */}
            <section className="mk-box">
                <h3 className="mk-box-title">Серверы ключа</h3>
                <ServerPicker nodes={nodes} selected={k.servers} onChange={(next, n, added) =>
                    modalAct(() => apiFetch('PATCH', `/ui/masterkeys/${k.id}`, token, { servers: next }),
                        added ? `«${serverName(nodes, n.id)}» добавлен в ключ` : `«${serverName(nodes, n.id)}» убран из ключа`)} />
                <p className="mk-setting-hint">Новые серверы добавляются кнопкой «Добавить сервер» над вкладкой.</p>
            </section>
        </div>
    );

    // Срок и остаток трафика — справа от имени, одной колонкой: капсула «до 12.03» над
    // «940 ГБ из 1 ТБ» со шкалой. Чип состояния стоит у имени, здесь его нет.
    // Полные дата и время с остатком — в подсказке.
    const sideLimits = (k: MasterKey) => {
        const when = k.expiresAt ? <span className="mk-lim-when">до {limitDay(k.expiresAt)}</span> : null;
        const usage = mkUsage(k, compactLimits);
        if (!when && !usage) return null;
        const tip = [
            k.expiresAt ? `до ${limitDate(k.expiresAt)}${timeLeft(k.expiresAt) ? ` · ${timeLeft(k.expiresAt)}` : ''}` : '',
            k.trafficLimitBytes ? `Израсходовано ${limitBytes(k.usedBytes ?? 0)} из ${limitBytes(k.trafficLimitBytes)}` : '',
        ].filter(Boolean).join(' · ');
        return (
            <span className="mk-lim mk-lim--side" title={tip || undefined}>
                {when}{usage}
            </span>
        );
    };

    // Полоска занятых слотов: числа по краям внутри; цвет по заполнению.
    const meter = (k: MasterKey, n: number) => (
        <div className={`mk-meter${n >= k.device_limit ? ' mk-meter--full' : ''}`}
            role="progressbar" aria-label="Устройства" aria-valuemin={0}
            aria-valuemax={k.device_limit} aria-valuenow={n}
            title={`Устройств: ${n} из ${k.device_limit}`}>
            <span className="mk-meter-text"><span>{n}</span><span>{k.device_limit}</span></span>
            <div className="mk-meter-fill"
                style={{ width: `${Math.min(100, (n / Math.max(1, k.device_limit)) * 100)}%` }}>
                {/* тот же текст поверх заливки — светлый, чтобы читался на ней */}
                <span className="mk-meter-text mk-meter-text--on" aria-hidden="true"><span>{n}</span><span>{k.device_limit}</span></span>
            </div>
        </div>
    );

    const newPsk = async (d: Device) => {
        try {
            const r = await apiFetch('POST', `/ui/devices/${d.id}/psk`, token);
            showMsg(r.pending ? `PSK обновлён; ещё на ${r.pending} — когда сервер будет на связи` : 'PSK обновлён');
            await load();
        } catch (e) { showMsg(errText(e)); }
    };
    const revoke = (d: Device) => {
        if (confirm('Отозвать устройство «' + (d.device_name || d.id) + '»?'))
            act(() => apiFetch('DELETE', `/ui/devices/${d.id}`, token), 'Устройство отозвано');
    };

    const deviceActions = (d: Device) => (
        <div className="actions">
            <span className="tip-wrap tip-wrap--end" data-tip="Новый PSK"><button className="btn-icon" aria-label="Новый PSK" onClick={() => newPsk(d)}><IcoRefresh /></button></span>
            <span className="tip-wrap tip-wrap--end" data-tip="Отозвать устройство"><button className="btn-icon btn-icon--danger" aria-label="Отозвать устройство" onClick={() => revoke(d)}><IcoTrash /></button></span>
        </div>
    );

    // Короткий ID устройства: полностью — в подсказке, по клику копируется
    const deviceId = (d: Device) => (
        <span className="tip-wrap tip-wrap--start" data-tip={`ID устройства ${d.device_id} · нажми, чтобы скопировать`}>
            <button className="copy-id" aria-label={`Скопировать ID устройства ${d.device_id}`}
                onClick={() => copyText(d.device_id).then(() => showMsg('ID устройства скопирован'), () => showMsg('Не удалось скопировать'))}>
                {d.device_id.slice(0, 8)}…
            </button>
        </span>
    );

    // Метка ключа и под ней UUID — коротко, чтобы не раздувать колонку «Имя». Полностью — в подсказке,
    // по клику копируется.
    const nameCell = (k: MasterKey, trailing?: ReactNode) => (
        <span className="name-stack">
            <span className="name-line">
                <span className="name-text">{k.label}</span>
                {trailing}
            </span>
            <span className="tip-wrap" data-tip={`UUID ${k.uuid} · нажми, чтобы скопировать`}>
                <button className="copy-id" aria-label={`Скопировать UUID ${k.uuid}`}
                    onClick={() => copyText(k.uuid).then(() => showMsg('UUID скопирован'), () => showMsg('Не удалось скопировать'))}>
                    {k.uuid.slice(0, 8)}…
                </button>
            </span>
        </span>
    );

    // Данные по ключу неполные: устройства не загрузились или сервер не ответил на статистику.
    const dataFlag = (k: MasterKey) => {
        if (failed.has(k.id)) return <span className="mk-flag" title="Список устройств не загрузился — показан последний известный"> · устройства не загрузились</span>;
        const d = down[k.id];
        return d?.length ? <span className="mk-flag" title="Онлайн и трафик с этих серверов не получены"> · нет связи: {serverNames(d)}</span> : null;
    };

    // Серверы ключа, куда ещё не добавлено устройство: был не в сети, когда оно подключалось или меняло
    // ключ; панель добавит его сама, когда сервер появится (reconcile). Без ips (старая панель) — неизвестно.
    const missingOn = (k: MasterKey, d: Device) => d.ips ? k.servers.filter(s => !d.ips!.some(x => x.server_id === s)) : [];
    // «из 3 серверов», «из 21 сервера»
    const ofServers = (n: number) => `${n} ${n % 10 === 1 && n % 100 !== 11 ? 'сервера' : 'серверов'}`;
    const syncFlag = (k: MasterKey, d: Device) => {
        const miss = missingOn(k, d);
        if (miss.length) return `${k.servers.length - miss.length} из ${ofServers(k.servers.length)}`;
        return d.servers_ok < d.servers_total ? `${d.servers_ok} из ${ofServers(d.servers_total)}` : '';
    };
    const syncTip = (k: MasterKey, d: Device) => {
        const miss = missingOn(k, d);
        return miss.length ? `Нет на ${miss.map(s => serverName(nodes, s)).join(', ')} — добавится само, когда сервер будет на связи`
            : 'Не на всех серверах ключа — добавится само, когда они будут на связи';
    };

    // Серверы ключа — в его строке под колонками IP и «Платформа»: «все серверы» (у ключа с одним
    // сервером — его имя) светло-зелёным или «2 из 3 серверов» тёплым, если на каком-то ещё нет всех
    // устройств ключа.
    // Имена — в подсказке.
    const keyServers = (k: MasterKey, list: Device[]) => {
        const behind = (s: number) => list.some(d => missingOn(k, d).includes(s));
        const late = k.servers.filter(behind);
        const ready = k.servers.filter(s => !behind(s));
        const names = (ids: number[]) => ids.map(s => serverName(nodes, s)).join(', ');
        const text = late.length ? `${ready.length} из ${ofServers(k.servers.length)}`
            : k.servers.length === 1 ? serverName(nodes, k.servers[0]) : 'все серверы';
        const tip = late.length ? `Есть: ${names(ready) || 'нигде'}. Нет: ${names(late)} — добавятся сами, когда сервер будет на связи`
            : k.servers.length > 1 ? names(k.servers) : null;
        const chip = <span className={`chip ${late.length ? 'chip--warm' : 'chip--online'} mk-srv`}>{text}</span>;   // всё на месте — светло-зелёный
        return tip ? <span className="tip-wrap tip-wrap--end" data-tip={tip}>{chip}</span> : chip;
    };

    // Статус в колонке «Статус» — только устройства: доступ показан чипом у имени, второй
    // такой же чип не нужен. У заблокированного ключа устройств в сети нет — прочерк.
    const keyStatus = (k: MasterKey, list: Device[], empty: boolean) => {
        if (mkOver(k)) return <span className="mk-flag" title="Ключ заблокирован — устройства не подключаются">—</span>;
        if (!list.length) return empty ? <span className="mk-flag">нет устройств</span> : null;
        const on = list.filter(d => d.online).length;
        const last = Math.max(...list.map(d => d.lastHandshake || 0));
        // Только состояние устройств: «фоновый» и торренты — это чипы самих устройств,
        // они живут в строках ниже и не должны попадать в шапку ключа.
        return (
            <span className={`chip chip--${on ? 'online' : 'offline'}`}>
                <span className="chip-dot" />
                {on ? `онлайн: ${on} из ${list.length}` : last ? timeAgo(last) + ' назад' : 'никогда'}
            </span>
        );
    };

    // Занятые слоты одним чипом: на карточке полоска разваливалась на две плашки «2» и «2».
    const slotChip = (k: MasterKey, n: number) => (
        <span className={`chip ${n >= k.device_limit ? 'chip--warm' : 'chip--offline'}`}
            title={`Устройств: ${n} из ${k.device_limit}`}>
            {n} из {k.device_limit}
        </span>
    );

    // Суммарный трафик устройств ключа — в метаданные, а не в шапку.
    const totals = (list: Device[]) => list.length ? (
        <span className="mk-dev-traffic">
            ↓ {bytes(list.reduce((a, d) => a + d.rx, 0))}  ↑ {bytes(list.reduce((a, d) => a + d.tx, 0))}
        </span>
    ) : null;

    // Подпись под именем ключа: серверы и суммарный трафик — то, что раньше было колонками
    // «IP», «Платформа», «↓ rx» и «↑ tx». Колонок осталось четыре, поэтому таблица
    // помещается по ширине и не листается вбок.
    const keyMeta = (k: MasterKey, list: Device[]) => (
        <span className="mk-key-meta">
            {keyServers(k, list)}
            {totals(list)}
        </span>
    );

    // Один адрес — выбранного в шапке сервера, остальные — «+N» и подсказка со всеми. Адрес с другого
    // сервера подписан его именем строкой ниже, иначе его примут за адрес выбранного.
    const ipCell = (d: Device) => {
        const all = d.ips ?? [];
        const { ip, serverId, other } = ipPick(d);
        const body = (
            <span className="name-stack">
                <span className="mk-ip-line">
                    <code className="mk-ip">{ip}</code>
                    {all.length > 1 && <span className="mk-ip-more">+{all.length - 1}</span>}
                </span>
                {other && serverId !== undefined && <span className="mk-sub">{serverName(nodes, serverId)}</span>}
            </span>
        );
        return all.length > 1
            ? <span className="tip-wrap" data-tip={all.map(x => `${serverName(nodes, x.server_id)}: ${x.ip}`).join(' · ')}>{body}</span>
            : body;
    };

    // Самая новая версия SenAWG среди устройств панели: кто отстал — тёплой плашкой, как ноды в меню серверов
    const newest = useMemo(() => Object.values(devs).flat().map(d => d.version).filter(Boolean)
        .reduce<string | null>((a, v) => !a || cmpVersion(v, a) > 0 ? v : a, null), [devs]);
    const versionTag = (d: Device, tip: 'start' | 'end') => {
        if (!d.version) return null;
        const old = !!newest && cmpVersion(d.version, newest) < 0;
        return old
            ? <span className={`tip-wrap tip-wrap--${tip}`} data-tip={`Есть новее: ${newest} — обнови SenAWG на устройстве`}><span className="chip chip--warm mk-ver">{d.version}</span></span>
            : <span className="mk-sub">{d.version}</span>;
    };
    // Раскрывать нечего — вместо стрелки пустое место той же ширины, имена остаются в одну линию
    const caret = (k: MasterKey, n: number) => n === 0 ? <span className="mk-caret-gap" aria-hidden="true" /> : (
        <button className={`mk-caret${folded.has(k.id) ? ' mk-caret--folded' : ''}`}
            aria-label={folded.has(k.id) ? 'Развернуть' : 'Свернуть'} aria-expanded={!folded.has(k.id)}
            onClick={() => fold(k.id)}>▾</button>
    );

    // Подпись устройства в таблице: платформа с версией, IP (все адреса — в подсказке) и трафик.
    const deviceMeta = (d: Device) => (
        <span className="mk-dev-meta">
            <span>{d.platform || '—'}</span>
            {versionTag(d, 'start')}
            <span aria-hidden="true">·</span>
            {ipCell(d)}
            <span className="mk-dev-traffic">↓ {bytes(d.rx)}  ↑ {bytes(d.tx)}</span>
        </span>
    );

    // Телефон: устройство — строка списка в карточке ключа, а не карточка в карточке. Имя, статус и «⋮»;
    // ниже мелко — платформа, версия, IP; трафик, приоритет и то, на скольких серверах оно есть.
    const deviceRow = (k: MasterKey, d: Device) => {
        const all = d.ips ?? [];
        const { ip, serverId, other } = ipPick(d);
        return (
            <li className="mk-dev" key={d.id}>
                <div className="mk-dev-head">
                    <span className="name-stack mk-dev-title">
                        <span className="mk-dev-name">{d.device_name || 'Устройство'}</span>
                        {deviceId(d)}
                    </span>
                    {statusChip(d, mkOver(k))}
                    {moreButton('device', d.id, `Ещё: ${d.device_name || 'устройство'}`)}
                </div>
                <div className="mk-dev-meta">
                    <span>{d.platform || '—'}</span>
                    {versionTag(d, 'start')}
                    <span aria-hidden="true">·</span>
                    <code className="mk-ip">{ip}</code>
                    {all.length > 1 && <span className="mk-ip-more" aria-label={all.map(x => `${serverName(nodes, x.server_id)}: ${x.ip}`).join(', ')}>+{all.length - 1}</span>}
                    {other && serverId !== undefined && <span>{serverName(nodes, serverId)}</span>}
                </div>
                <div className="mk-dev-meta">
                    <span className="mk-dev-traffic">↓ {bytes(d.rx)}  ↑ {bytes(d.tx)}</span>
                    {!mkOver(k) && <><PriorityChip priority={bgOnly(d.priority)} torrent={d.torrent} /><TorrentChip torrent={d.torrent} /></>}
                    {syncFlag(k, d) && <span>{syncFlag(k, d)}</span>}
                    {d.rekey_requested && <span>ждёт смены ключа</span>}
                </div>
            </li>
        );
    };

    // Причина сбоя словами: одни и те же формулировки, что во вкладке «Пользователи».
    const failTitle = fail === 'no-core' ? 'На этом сервере нет VPN (панель без awg-ctrl).'
        : fail === 'offline' ? 'Сервер не на связи'
        : 'Не удалось загрузить список';
    const failText = fail === 'no-core'
        ? 'Мастер-ключи живут на нодах. Выберите сервер в шапке — ничего не сломано: локальный awg-ctrl на этой панели просто не запущен.'
        : fail === 'offline' ? 'Список мастер-ключей сейчас недоступен — сервер не отвечает.'
        : 'Сервер ответил ошибкой — можно попробовать ещё раз.';

    return (
        <>
            {!enabled && (
                <p className="mk-notice">Подписка не настроена на сервере: нет ключа подписи или SUB_PORT. Ссылки выдавать нельзя.</p>
            )}
            {enabled && !tls && (
                <p className="mk-notice">Подписка работает без TLS: ответы подписаны, но secret ссылки и конфиг видны в сети.</p>
            )}

            {masking && (
                <div className="mk-warn" role="note">
                    <span className="mk-warn-text">
                        <b>Маскировка первого пакета · {serverName(nodes, getServerId() ?? 0)}</b>
                        Перед подключением клиент шлёт пакет, похожий на обычный трафик. Новый шаблон устройства sen://
                        получат в течение минуты.
                    </span>
                    <button className="btn btn--tonal" onClick={regenMasking} disabled={masking === 'busy'}>
                        <IcoRefresh /> Сменить
                    </button>
                </div>
            )}

            <div className="toolbar">
                <input className="field" placeholder="Метка (напр. Семья)" value={label}
                    onChange={e => setLabel(e.target.value)} onKeyDown={e => e.key === 'Enter' && create()} />
                <input className="field mk-limit-input" type="number" min={1} max={100} title="Лимит устройств"
                    aria-label="Лимит устройств" value={limit} onChange={e => setLimit(e.target.value)} />
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={create} disabled={creating}><IcoPlus /> Создать ключ</button>
                    <button className="btn btn--tonal" onClick={load}><IcoRefresh /> Обновить</button>
                    <button className="btn btn--tonal" onClick={() => setBulk(true)} disabled={!live.length || !nodes.length}
                        title="Добавить или убрать серверы у всех мастер-ключей сразу"><IcoSettings /> Серверы для всех</button>
                </div>
            </div>

            {/* Нет списка и известна причина: вместо пустой таблицы — объяснение. «Нет
                мастер-ключей» остаётся честным только когда сервер правда ответил пустым. */}
            {fail && keys.length === 0 && (
                <div className={`load-state${fail === 'offline' ? ' load-state--warm' : ''}`} role="status">
                    <p className="load-state-title">{failTitle}</p>
                    <p className="load-state-text">{failText}</p>
                    {fail !== 'no-core' && (
                        <button className="btn btn--tonal" onClick={load}><IcoRefresh /> Повторить</button>
                    )}
                </div>
            )}

            {(!fail || keys.length > 0) && (<>
            {/* Десктоп: таблица из четырёх колонок. IP, платформа и трафик переехали
                в подписи под именем — иначе таблица не помещалась и листалась вбок. */}
            <div className="table-card mk-table-card">
                <table className="mk-table">
                    <thead>
                        <tr>
                            <th>Имя</th><th>Статус</th><th>Устройства</th><th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {keys.length === 0 ? (
                            <tr><td colSpan={4} className="empty">Нет мастер-ключей</td></tr>
                        ) : keys.map(k => {
                            if (k.deleting) return (
                                <tr key={k.id} className="mk-tr-master mk-last">
                                    <td><span className="mk-name"><IcoKey />{nameCell(k)}</span></td>
                                    <td colSpan={3}>{deletingChip(k)}</td>
                                </tr>
                            );
                            const list = devs[k.id] ?? [];
                            const count = devs[k.id]?.length ?? k.devices;
                            const open = !folded.has(k.id);
                            return (
                                <Fragment key={k.id}>
                                    <tr className="mk-tr-master">
                                        <td>
                                            <span className="mk-name-cell">
                                                <span className="mk-name-main">
                                                    <span className="mk-name-row">
                                                        <span className="mk-name">
                                                            {caret(k, count)}<IcoKey />{nameCell(k)}{dataFlag(k)}
                                                        </span>
                                                        {mkChip(k)}
                                                    </span>
                                                    {keyMeta(k, list)}
                                                </span>
                                                {sideLimits(k)}
                                            </span>
                                        </td>
                                        <td><span className="user-chips">{keyStatus(k, list, count === 0)}</span></td>
                                        <td>{meter(k, count)}</td>
                                        <td className="td-actions mk-td-actions">{actions(k)}</td>
                                    </tr>
                                    {open && list.map((d, i) => (
                                        <tr key={d.id} className={`mk-tr-device${i === list.length - 1 ? ' mk-last' : ''}`}>
                                            <td>
                                                <span className="mk-indent">
                                                    <span className="name-stack">
                                                        {d.device_name || 'Устройство'}
                                                        {/* пометки — в строке ID, не рядом с именем */}
                                                        <span className="mk-id-line">
                                                            {deviceId(d)}
                                                            {syncFlag(k, d) && <span className="tip-wrap tip-wrap--start mk-flag" data-tip={syncTip(k, d)}>· {syncFlag(k, d)}</span>}
                                                            {d.rekey_requested && <span className="mk-flag">· ждёт смены ключа</span>}
                                                        </span>
                                                    </span>
                                                    {deviceMeta(d)}
                                                </span>
                                            </td>
                                            <td><span className="user-chips">{statusChip(d, mkOver(k))}{!mkOver(k) && <><PriorityChip priority={bgOnly(d.priority)} torrent={d.torrent} /><TorrentChip torrent={d.torrent} /></>}</span></td>
                                            <td></td>
                                            <td className="td-actions mk-td-actions">{deviceActions(d)}</td>
                                        </tr>
                                    ))}
                                </Fragment>
                            );
                        })}
                    </tbody>
                </table>
            </div>

            {/* Телефон и планшет: таблица скрыта (.table-card) — карточка на ключ: шапка, сводка, действия,
                ниже списком его устройства */}
            {keys.length === 0 ? <p className="mk-empty mk-tree">Нет мастер-ключей</p> : (
                <ul className="mk-tree">
                    {keys.map(k => {
                        if (k.deleting) return (
                            <li className="mk-root" key={k.id}>
                                <div className="mk-master">
                                    <IcoKey />
                                    <span className="mk-master-label">{nameCell(k)}</span>
                                </div>
                                <div className="mk-master-sum">{deletingChip(k)}</div>
                            </li>
                        );
                        const list = devs[k.id] ?? [];
                        const count = devs[k.id]?.length ?? k.devices;
                        const isFolded = folded.has(k.id);
                        return (
                            <li className="mk-root" key={k.id}>
                                <div className="mk-master">
                                    {caret(k, count)}
                                    <IcoKey />
                                    <span className="mk-master-label">
                                        {nameCell(k, mkChip(k))}
                                        {/* лимиты — сразу за статусом, одной строкой, вровень с именем */}
                                        {sideLimits(k)}
                                    </span>
                                </div>
                                <div className="mk-master-sum">
                                    {/* прочерк — заменитель пустой ячейки в таблице; в карточке он ни к чему */}
                                    {!mkOver(k) && keyStatus(k, list, count === 0)}
                                    {keyServers(k, list)}
                                    {slotChip(k, count)}
                                    {totals(list)}
                                    {dataFlag(k)}
                                </div>
                                <div className="mk-master-actions">
                                    <button className="btn btn--tonal" onClick={() => showQR(k)}><IcoQR /> QR</button>
                                    <button className="btn btn--tonal" onClick={() => copyLink(k)}><IcoCopy /> Ссылка</button>
                                    <button className="btn-icon" aria-label={`Настройки: ${k.label}`} aria-haspopup="dialog"
                                        onClick={e => openSettings(k, e.currentTarget)}><IcoSettings /></button>
                                    {moreButton('key', k.id, `Ещё: ${k.label}`)}
                                </div>
                                {!isFolded && list.length > 0 && <ul className="mk-devs">{list.map(d => deviceRow(k, d))}</ul>}
                            </li>
                        );
                    })}
                </ul>
            )}
            </>)}

            {menu && (() => {
                const run = (f: () => void) => () => { setMenu(null); f(); };
                const at = { top: menu.top, right: menu.right };
                const cls = `mk-menu${menu.up ? ' mk-menu--up' : ''}`;
                if (menu.kind === 'device') {
                    const d = Object.values(devs).flat().find(x => x.id === menu.id);
                    if (!d) return null;
                    return (
                        <div className={cls} role="menu" aria-label={`Устройство ${d.device_name}`} ref={menuRef} style={at}>
                            <button role="menuitem" className="mk-menu-item" onClick={run(() => newPsk(d))}><IcoRefresh /> Новый PSK</button>
                            <button role="menuitem" className="mk-menu-item mk-menu-item--danger" onClick={run(() => revoke(d))}><IcoTrash /> Отозвать</button>
                        </div>
                    );
                }
                const k = keys.find(x => x.id === menu.id);
                if (!k) return null;
                return (
                    <div className={cls} role="menu" aria-label={`Мастер-ключ ${k.label}`} ref={menuRef} style={at}>
                        {menu.full && <button role="menuitem" className="mk-menu-item" onClick={run(() => copyLink(k))}><IcoCopy /> Скопировать ссылку</button>}
                        <button role="menuitem" className="mk-menu-item" onClick={run(() =>
                            openSettings(k, document.querySelector<HTMLElement>(`[data-menu-for="key-${k.id}"]`)))}><IcoSettings /> Настройки ключа</button>
                        <button role="menuitem" className="mk-menu-item mk-menu-item--danger" onClick={run(() => remove(k))}><IcoTrash /> Удалить</button>
                    </div>
                );
            })()}

            {bulk && (
                <BulkServersModal token={token} nodes={nodes} keys={live} defaults={defaults} onClose={() => setBulk(false)}
                    onDone={n => { setBulk(false); showMsg(n ? `Серверы обновлены у ${n} ключей` : 'Сохранено'); load(); }} />
            )}

            {/* Настройки ключа — модалка: поля лимитов сохраняет футер, остальные действия
                применяются сразу; ошибки остаются внутри окна. */}
            {tools !== null && (() => {
                const k = keys.find(x => x.id === tools);
                if (!k) return null;
                return (
                    <div className="node-scrim mk-scrim" onClick={e => { if (e.target === e.currentTarget) closeSettings(); }}>
                        <div className="node-modal mk-modal" role="dialog" aria-modal="true" aria-labelledby="mk-modal-title"
                            ref={modalRef} tabIndex={-1} onClick={e => e.stopPropagation()}>
                            <header className="mk-modal-head">
                                <h2 className="mk-modal-title" id="mk-modal-title">Настройки: {k.label}</h2>
                                <button className="btn-icon mk-modal-x" aria-label="Закрыть" onClick={closeSettings}><IcoClose /></button>
                            </header>
                            <div className="mk-modal-body">{settingsBody(k)}</div>
                            <footer className="mk-modal-foot">
                                {mkErr.general && !mkErr.date && !mkErr.amount && (
                                    <p className="mk-lim-err mk-modal-err" role="alert">{mkErr.general}</p>
                                )}
                                <div className="mk-modal-actions">
                                    <button className="btn btn--text" onClick={closeSettings}>Отмена</button>
                                    <button className="btn btn--primary" onClick={() => saveSettings(k)} disabled={saving}>
                                        {saving ? 'Сохранение…' : 'Сохранить'}
                                    </button>
                                </div>
                            </footer>
                        </div>
                    </div>
                );
            })()}

            {modal && (
                <div className="qr-backdrop" onClick={() => setModal(null)}>
                    <div className="qr-card" onClick={e => e.stopPropagation()}>
                        <p className="qr-name">{modal.label}</p>
                        <img className="qr-img" src={modal.dataUrl} alt="QR" title="Нажми чтобы скопировать sen:// ссылку"
                            onClick={async () => { await copyText(modal.link); showMsg('Ссылка скопирована'); }} />
                        <p className="qr-hint">Отсканируй в SenAWG · нажми на QR чтобы скопировать{modal.tls ? '' : ' · без TLS'}</p>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setModal(null)}>Закрыть</button>
                            <button className="btn btn--primary" onClick={async () => { await copyText(modal.link); showMsg('Ссылка скопирована'); }}><IcoCopy /> Скопировать</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}
