// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useRef, useCallback } from 'react';
import {
    apiFetch, vpnKeyToConf, downloadFile, copyText, bytes, timeAgo, genLabel,
    limitBytes, limitDate, limitDay, timeLeft, inputDate, parseInputDate, blockText,
    LIMIT_UNITS, limitAmount, limitUnit, parseLimitAmount, TB, classifyApiError,
    type LimitUnit, type BlockReason, type User, type PageProps, type AwgGen, type ApiFailure,
} from '../../lib/shared';
import { qrDataUrl } from '../../lib/qr';
import { IcoPlus, IcoRefresh, IcoQR, IcoTrash, IcoGlobe, IcoHistory, IcoPower } from '../../components/icons';
import PriorityChip from '../../components/PriorityChip';
import TorrentChip from '../../components/TorrentChip';
import DateField from './DateField';
import './users.css';
import './load-state.css';

interface LimErrors { date?: string; amount?: string; general?: string }
// orig — значения, которые были в строке на момент открытия диалога. «Сохранить» без
// правок не должен ничего отправлять: раньше он затирал лимиты, которых не было в
// списке из-за merge-бага.
interface LimEdit {
    name: string; date: string; amount: string; unit: LimitUnit;
    orig: { expiresAt: number | null; trafficLimitBytes: number | null };
}

// Проверки до запроса: пусто — «без ограничения», остальное должно быть числом в разумных пределах.
const checkLimits = (e: LimEdit): LimErrors => {
    const errs: LimErrors = {};
    if (e.date) {
        const ts = parseInputDate(e.date);
        if (ts === null || Number.isNaN(ts)) errs.date = 'Некорректная дата';
    }
    const b = parseLimitAmount(e.amount, e.unit);
    if (b !== null) {
        if (Number.isNaN(b)) errs.amount = 'Нужно число, например 250';
        else if (b < 1024 ** 2) errs.amount = 'Минимум 1 МБ';
        else if (b > 1024 * TB) errs.amount = 'Больше 1024 ТБ не бывает';
    }
    return errs;
};

// Подсказка под полем даты: «до 12.03.2027 14:30 · осталось 12 дней» или предупреждение о прошлом.
const dateHint = (v: string): { text: string; warm?: boolean } => {
    if (!v) return { text: 'Без ограничения по сроку' };
    const ts = parseInputDate(v);
    if (ts === null || Number.isNaN(ts)) return { text: 'Дата не распознана', warm: true };
    return { text: `${limitDate(ts)} · ${timeLeft(ts)}`, warm: ts * 1000 <= Date.now() };
};
const trafficHint = (v: string, unit: LimitUnit): { text: string } => {
    const b = parseLimitAmount(v, unit);
    if (b === null) return { text: 'Без ограничения по трафику' };
    if (Number.isNaN(b) || b <= 0) return { text: 'Нужно положительное число' };
    if (b < 1024 ** 2) return { text: 'Минимум 1 МБ' };
    return { text: `= ${limitBytes(b)}` };
};

// Поля, которые приходят из /api/users/stats: лимиты, приоритет, счётчики. undefined в ответе
// значит «неизвестно», а не «снято», поэтому такие поля не затираем — ни при слиянии списка,
// ни при обновлении статистики.
const STAT_FIELDS = [
    'priority', 'torrent', 'expiresAt', 'trafficLimitBytes', 'usedBytes',
    'remainingBytes', 'blockReason', 'expired', 'blocked',
] as const;

// Свежий список из /api/users поверх прежнего: известные значения статистики доживают до
// следующего удачного /stats. Иначе перезагрузка при сбое статистики стирала бы лимиты.
function keepStats(fresh: User[], prev: User[]): User[] {
    const old = new Map(prev.map(u => [u.name, u]));
    return fresh.map(u => {
        const p = old.get(u.name);
        if (!p) return u;
        const out: User = { ...u };
        for (const k of STAT_FIELDS) {
            if (u[k] === undefined && p[k] !== undefined) (out as any)[k] = p[k];
        }
        return out;
    });
}

// Доступ закрыт: непустая причина, флаг blocked или истёкший срок. expired проверяем
// отдельно — бэкенд может не выставить blocked и оставить blockReason пустым, а ключ
// уже не работает; «онлайн» у такого читается как «подключается».
const accessReason = (x: { blocked?: boolean; blockReason?: BlockReason | null; expired?: boolean }): BlockReason | null =>
    x.blockReason ?? (x.expired ? 'expired' : x.blocked ? 'manual' : null);

// Статус в таблице и карточке: у закрытого доступа — причина блокировки, а не сетевая
// активность (одна формулировка на обе вкладки — blockText из shared).
function StatusChip({ u }: { u: User }) {
    const reason = accessReason(u);
    if (reason) return (
        <span className="chip chip--error">
            <span className="chip-dot" />{blockText(reason)}
        </span>
    );
    return (
        <span className={`chip chip--${u.online ? 'online' : 'offline'}`}>
            <span className="chip-dot" />
            {u.online
                ? 'онлайн'
                : u.lastHandshake
                    ? timeAgo(u.lastHandshake) + ' назад'
                    : 'никогда'}
        </span>
    );
}

// Лимиты пользователя: израсходовано и срок. Про блокировку говорит чип статуса —
// здесь только лимиты, чтобы не дублировать красный чип в строке.
// Капсула срока живёт в строке имени (справа от него), а не под шкалой: дата видна
// рядом с именем, как у мастер-ключей. Полная дата с временем — в подсказке.
// Всё читается защитно — старый awg-ctrl этих полей не отдаёт вовсе.
function LimitsCell({ u }: { u: User }) {
    const limit = u.trafficLimitBytes ?? null;
    const used  = u.usedBytes ?? 0;
    const hasTraffic = limit != null && limit > 0;
    const hasDate = u.expiresAt != null && u.expiresAt > 0;
    // Шкала и цвет — про трафик, а не про блокировку: красной она становится, только когда
    // лимит действительно исчерпан. Закрытый вручную при 12 из 200 ГБ — обычная полоска.
    const exhausted = hasTraffic && used >= (limit as number);
    const pct = hasTraffic ? Math.min(100, Math.round((used / (limit as number)) * 100)) : 0;
    const tone = exhausted ? ' lim-bar-fill--over' : pct >= 85 ? ' lim-bar-fill--warn' : '';
    const tip = [
        hasTraffic ? `Израсходовано ${limitBytes(used)} из ${limitBytes(limit)}` : '',
        hasDate ? `до ${limitDate(u.expiresAt)}${timeLeft(u.expiresAt) ? ` · ${timeLeft(u.expiresAt)}` : ''}` : '',
    ].filter(Boolean).join(' · ');
    if (!hasTraffic) {
        // срок без трафика показывает капсула у имени — под шкалой дублировать нечего
        return hasDate ? null : <div className="lim-cell"><span className="lim-none">без лимитов</span></div>;
    }
    return (
        <div className="lim-cell" title={tip || undefined}>
            <div className="lim-usage">
                <span className="lim-usage-text">
                    <span className={exhausted ? 'lim-used lim-used--over' : 'lim-used'}>{limitBytes(used)}</span>
                    <span className="lim-of"> из {limitBytes(limit)}</span>
                </span>
                <span className="lim-bar" role="progressbar" aria-label="Израсходовано трафика"
                    aria-valuemin={0} aria-valuemax={limit as number} aria-valuenow={Math.min(used, limit as number)}>
                    <span className={`lim-bar-fill${tone}`} style={{ width: `${pct}%` }} />
                </span>
            </div>
        </div>
    );
}

// Капсула срока: «до 29.10», справа от имени. Полная дата — в подсказке.
function WhenChip({ u }: { u: User }) {
    if (u.expiresAt == null || u.expiresAt <= 0) return null;
    const tip = `до ${limitDate(u.expiresAt)}${timeLeft(u.expiresAt) ? ` · ${timeLeft(u.expiresAt)}` : ''}`;
    return <span className="lim-when" title={tip}>до {limitDay(u.expiresAt)}</span>;
}

// Вкладка «Пользователи»: форма создания + таблица (десктоп) / карточки (мобильный)
// + модалка QR. Сервер-бар сверху рендерит App. Свой стейт и поллинг — здесь.
export default function UsersPage({ token, showMsg }: PageProps) {
    const [users, setUsers]     = useState<User[]>([]);
    // Почему списка нет: 'no-core' — здесь нет VPN, 'offline' — сервер не на связи,
    // 'error' — настоящая ошибка. null — всё в порядке. Без этого «пусто» от «нет VPN»
    // не отличить и вкладка врёт «Нет пользователей».
    const [fail, setFail]             = useState<ApiFailure | null>(null);
    const [loadedOnce, setLoadedOnce] = useState(false);
    const [loadedAt, setLoadedAt]     = useState(0);
    const [statsDown, setStatsDown]   = useState(false);
    const [newName, setNewName] = useState('');
    const [qrModal, setQrModal] = useState<{ name: string; dataUrl: string; vpnKey: string } | null>(null);
    // Диалог «Лимиты…»: поля хранятся строками (пусто — без ограничения), состояние
    // блокировки и счётчик читаются из самого пользователя — после reload они свежие.
    const [lim, setLim]       = useState<LimEdit | null>(null);
    const [limErr, setLimErr] = useState<LimErrors>({});
    const [limBusy, setLimBusy] = useState(false);
    // Открыт календарь срока: Esc в нём закрывает только поповер, а не весь диалог.
    const [dateOpen, setDateOpen] = useState(false);
    // Поколение, на котором сейчас работает интерфейс. Ключи, выданные на другом
    // поколении, уже не подключатся — их надо перевыпустить.
    const [serverGen, setServerGen] = useState<AwgGen>('2');
    const [upgrading, setUpgrading]  = useState(false);
    const statsRef              = useRef<ReturnType<typeof setInterval> | null>(null);
    const stale                 = users.filter(u => u.key_gen !== serverGen);
    const noCore                = fail === 'no-core';
    // Список показываем, только когда он действительно загружен: при «сервер не на связи»
    // остаётся последний удачный, при «здесь нет VPN» — ничего.
    const showList              = !noCore && (loadedOnce || fail === null);

    const loadStats = useCallback(async (tok: string) => {
        try {
            const data = await apiFetch('GET', '/api/users/stats', tok);
            setStatsDown(false);
            setUsers(prev => prev.map(u => {
                const s = (data.users as User[] ?? []).find(x => x.name === u.name);
                if (!s) return u;
                // Лимиты живут только в /stats (старый /api/users их не отдаёт вовсе), поэтому
                // переносим их целиком — но лишь те, что пришли: undefined не должен стереть
                // известное значение, иначе строка навсегда «без лимитов».
                const out: User = {
                    ...u,
                    online: s.online, lastHandshake: s.lastHandshake, rx: s.rx, tx: s.tx,
                };
                for (const k of STAT_FIELDS) {
                    if (s[k] !== undefined) (out as any)[k] = s[k];
                }
                return out;
            }));
        } catch { setStatsDown(true); }
    }, []);

    const loadUsers = useCallback(async (tok: string) => {
        try {
            const data = await apiFetch('GET', '/api/users', tok);
            setUsers(prev => keepStats(data.users ?? [], prev));
            setFail(null);
            setLoadedOnce(true);
            setLoadedAt(Date.now());
            // /health нужен только для пометки устаревших ключей: его сбой не должен
            // прятать уже загруженный список.
            try {
                const health = await apiFetch('GET', '/health', tok);
                setServerGen(health.gen ?? '2');
            } catch { /* не критично */ }
            await loadStats(tok);
        } catch (e) {
            const why = classifyApiError(e);
            setFail(why);
            // Панель без локального VPN: пользователей здесь нет в принципе. Список с другой
            // ноды (если был) не показываем — он к этому серверу не относится.
            if (why === 'no-core') setUsers([]);
            // «Нет связи» и «нет VPN» объясняет блок на месте списка, тост — только у настоящей ошибки.
            if (why === 'error') showMsg('Ошибка загрузки');
        }
    }, [loadStats, showMsg]);

    // Пересобирает vpn:// ключи всех пользователей на текущих параметрах интерфейса.
    // IP и ключевая пара сохраняются, но клиентам нужно заново импортировать ключ.
    const reissueKeys = useCallback(async () => {
        if (!confirm(
            `Перевыпустить ключи (${stale.length} шт.) на поколении ${genLabel(serverGen)}?\n\n` +
            'Старые ключи перестанут работать — всем придётся импортировать ключ заново.',
        )) return;
        try {
            const r = await apiFetch('POST', '/api/users/reissue', token);
            showMsg(`Перевыпущено: ${r.reissued} из ${r.total}`);
            await loadUsers(token);
        } catch {
            showMsg('Не удалось перевыпустить ключи');
        }
    }, [stale.length, serverGen, token, loadUsers, showMsg]);

    // Перевод сервера с 2.0 на 3.1 без переустановки: awg-ctrl дописывает
    // 3.x-параметры в awg1.conf, перезапускает интерфейс (с откатом, если ядро
    // их не примет) и сразу перевыпускает ключи — старые собраны на
    // 2.0-параметрах и работать не будут.
    const upgradeGen = useCallback(async () => {
        if (!confirm(
            'Перевести сервер на AmneziaWG 3.1?\n\n' +
            'Интерфейс будет перезапущен, а все vpn:// ключи перевыпущены — ' +
            'клиентам придётся импортировать ключ заново.',
        )) return;
        setUpgrading(true);
        try {
            const r = await apiFetch('POST', '/awg/upgrade', token);
            showMsg(`Сервер переведён на ${genLabel(r.gen)}, перевыпущено ключей: ${r.reissue?.reissued ?? 0}`);
            await loadUsers(token);
        } catch (e: any) {
            showMsg(e?.response?.data?.error ?? 'Не удалось перейти на 3.1');
        } finally {
            setUpgrading(false);
        }
    }, [token, loadUsers, showMsg]);

    const createUser = useCallback(async () => {
        if (!newName) return;
        const u = await apiFetch('POST', '/api/users', token, { name: newName });
        if (u.error) { showMsg(u.error); return; }
        setNewName('');
        showMsg('Создан: ' + newName);
        await loadUsers(token);
    }, [newName, token, loadUsers, showMsg]);

    const deleteUser = useCallback(async (name: string) => {
        if (!confirm('Удалить ' + name + '?')) return;
        await apiFetch('DELETE', '/api/users/' + name, token);
        showMsg('Удалён: ' + name);
        await loadUsers(token);
    }, [token, loadUsers, showMsg]);

    const showQR = useCallback(async (name: string) => {
        try {
            const u       = await apiFetch('POST', '/api/users/' + name, token);
            const vpnKey  = u.vpn_key ?? '';
            // errorCorrectionLevel 'L' (а не 'H'): ключ vpn:// длинный (~1.3 КБ),
            // при 'H' ёмкость QR падает до ~1273 байт и кодирование падает.
            setQrModal({ name, dataUrl: qrDataUrl(vpnKey, 'L'), vpnKey });
        } catch {
            showMsg('Не удалось сгенерировать QR-код');
        }
    }, [token, showMsg]);

    // ── Лимиты: диалог, быстрые действия ──
    const openLimits = useCallback((u: User) => {
        const date = inputDate(u.expiresAt), amount = limitAmount(u.trafficLimitBytes), unit = limitUnit(u.trafficLimitBytes);
        setLim({
            name: u.name, date, amount, unit,
            // Снимок того, что уйдёт на сервер при нетронутых полях: сравниваем «до/после»
            // по одним и тем же преобразованиям, поэтому «Сохранить» без правок — no-op.
            orig: { expiresAt: parseInputDate(date), trafficLimitBytes: parseLimitAmount(amount, unit) },
        });
        setLimErr({});
        setDateOpen(false);
    }, []);
    const closeLimits = useCallback(() => { setLim(null); setLimErr({}); }, []);

    const saveLimits = useCallback(async () => {
        if (!lim) return;
        const errs = checkLimits(lim);
        if (errs.date || errs.amount) { setLimErr(errs); return; }
        // Шлём только изменившиеся поля: PUT трактует null как «снять ограничение», и
        // лишний ключ стёр бы то, чего оператор не касался (в т.ч. лимит, не доехавший
        // до списка). Ничего не изменилось — закрываем без запроса и без тоста.
        const expiresAt = parseInputDate(lim.date);
        const trafficLimitBytes = parseLimitAmount(lim.amount, lim.unit);
        const body: Record<string, number | null> = {};
        if (expiresAt !== lim.orig.expiresAt) body.expires_at = expiresAt;
        if (trafficLimitBytes !== lim.orig.trafficLimitBytes) body.traffic_limit_bytes = trafficLimitBytes;
        if (!('expires_at' in body) && !('traffic_limit_bytes' in body)) { closeLimits(); return; }
        setLimBusy(true); setLimErr({});
        try {
            await apiFetch('PUT', `/api/users/${encodeURIComponent(lim.name)}/limits`, token, body);
            showMsg('Лимиты сохранены');
            closeLimits();
            await loadUsers(token);
        } catch (e: any) {
            const d = e?.response?.data ?? {};
            setLimErr({
                general: d.error ?? 'Не удалось сохранить лимиты',
                date: d.errors?.expires_at, amount: d.errors?.traffic_limit_bytes,
            });
        } finally { setLimBusy(false); }
    }, [lim, token, showMsg, closeLimits, loadUsers]);

    // Блокировка и сброс счётчика применяются сразу, без «Сохранить».
    const limAct = useCallback(async (path: string, ok: string) => {
        if (!lim) return;
        setLimBusy(true); setLimErr({});
        try {
            await apiFetch('POST', `/api/users/${encodeURIComponent(lim.name)}/${path}`, token);
            showMsg(ok);
            await loadUsers(token);
        } catch (e: any) {
            setLimErr({ general: e?.response?.data?.error ?? 'Ошибка запроса' });
        } finally { setLimBusy(false); }
    }, [lim, token, showMsg, loadUsers]);

    // Данные для диалога берём из списка: после сброса счётчика они обновятся сами.
    const limUser = lim ? users.find(u => u.name === lim.name) ?? null : null;
    const limReason = limUser
        ? limUser.blockReason ?? (limUser.expired ? 'expired' : limUser.blocked ? 'manual' : null)
        : null;
    const limBusyNow = !!limUser?.blocked || !!limUser?.expired;

    // Esc закрывает диалог — как у остальных модалок вкладки. Открытый календарь
    // перехватывает Esc сам, поэтому в этом случае диалог не трогаем.
    useEffect(() => {
        if (!lim) return;
        const key = (e: KeyboardEvent) => { if (e.key === 'Escape' && !dateOpen) closeLimits(); };
        document.addEventListener('keydown', key);
        return () => document.removeEventListener('keydown', key);
    }, [lim, dateOpen, closeLimits]);

    // Загрузить пользователей и запустить поллинг статистики, пока вкладка открыта.
    useEffect(() => {
        if (!token) return;
        loadUsers(token);
        statsRef.current = setInterval(() => loadStats(token), 60_000);
        return () => { if (statsRef.current) clearInterval(statsRef.current); };
    }, [token, loadUsers, loadStats]);

    return (
        <>
            <div className="toolbar">
                <input
                    className="field"
                    placeholder="Имя пользователя"
                    value={newName}
                    disabled={noCore}
                    onChange={e => setNewName(e.target.value)}
                    onKeyDown={e => e.key === 'Enter' && createUser()}
                />
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={createUser} disabled={noCore}>
                        <IcoPlus /> Создать
                    </button>
                    <button className="btn btn--tonal" onClick={() => loadUsers(token)}>
                        <IcoRefresh /> Обновить
                    </button>
                    {!noCore && serverGen !== '3.1' && (
                        <button className="btn btn--danger" onClick={upgradeGen} disabled={upgrading}>
                            <IcoRefresh /> {upgrading ? 'Переход…' : 'Перейти на AWG 3.1'}
                        </button>
                    )}
                    {!noCore && stale.length > 0 && (
                        <button className="btn btn--danger" onClick={reissueKeys}>
                            <IcoRefresh /> Перевыпустить ключи ({stale.length})
                        </button>
                    )}
                </div>
            </div>

            {/* Состояние вместо списка: «здесь нет VPN», «сервер не на связи», сбой загрузки.
                «Нет пользователей» говорим, только когда сервер правда ответил пустым списком. */}
            {fail === 'no-core' && (
                <div className="load-state" role="status">
                    <p className="load-state-title">На этом сервере нет VPN (панель без awg-ctrl).</p>
                    <p className="load-state-text">
                        Выберите сервер в шапке — пользователи живут на нодах. Ничего не сломано:
                        локальный awg-ctrl на этой панели просто не запущен.
                    </p>
                </div>
            )}
            {fail === 'offline' && (
                <div className="load-state load-state--warm" role="status">
                    <p className="load-state-title">Сервер не на связи</p>
                    <p className="load-state-text">
                        {users.length
                            ? `Показан последний загруженный список${loadedAt && Date.now() - loadedAt >= 60_000 ? ` — ${timeAgo(Math.floor(loadedAt / 1000))} назад` : ''}.`
                            : 'Список пользователей сейчас недоступен — сервер не отвечает.'}
                    </p>
                    <button className="btn btn--tonal" onClick={() => loadUsers(token)}>
                        <IcoRefresh /> Повторить
                    </button>
                </div>
            )}
            {fail === 'error' && !loadedOnce && (
                <div className="load-state" role="status">
                    <p className="load-state-title">Не удалось загрузить список</p>
                    <p className="load-state-text">Сервер ответил ошибкой — можно попробовать ещё раз.</p>
                    <button className="btn btn--tonal" onClick={() => loadUsers(token)}>
                        <IcoRefresh /> Повторить
                    </button>
                </div>
            )}
            {fail === 'error' && loadedOnce && (
                <p className="load-note" role="status">Список не обновился — показан последний загруженный.</p>
            )}
            {/* Статистика не пришла, а список рабочий: тихая пометка, без тоста и без потери строк. */}
            {statsDown && users.length > 0 && (
                <p className="load-note" role="status">Счётчики трафика недоступны</p>
            )}

            {showList && (<>
            <div className="table-card">
                <table className="users-table">
                    <thead>
                        <tr>
                            <th>#</th>
                            <th>Статус</th>
                            <th>Приоритет</th>
                            <th>Имя</th>
                            <th>IP</th>
                            <th>↓ rx</th>
                            <th>↑ tx</th>
                            <th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {users.length === 0 ? (
                            <tr><td colSpan={8} className="empty">Нет пользователей</td></tr>
                        ) : users.map((u, i) => (
                            <tr key={u.name}>
                                <td className="td-num">{i + 1}</td>
                                <td><StatusChip u={u} /></td>
                                <td><span className="user-chips"><PriorityChip priority={u.priority} torrent={u.torrent} /><TorrentChip torrent={u.torrent} /></span></td>
                                <td>
                                    <span className="user-name-cell">
                                        <span className="user-name-line">
                                            <span className="user-name-text">{u.name}</span>
                                            {u.key_gen !== serverGen && (
                                                <span
                                                    className="tip-wrap user-gen"
                                                    data-tip={`Ключ выдан на ${genLabel(u.key_gen)}, сервер работает на ${genLabel(serverGen)}`}
                                                >
                                                    <span className="chip chip--error">
                                                        <span className="chip-dot" />
                                                        {genLabel(u.key_gen)}
                                                    </span>
                                                </span>
                                            )}
                                            <WhenChip u={u} />
                                        </span>
                                        <LimitsCell u={u} />
                                    </span>
                                </td>
                                <td className="td-mono">{u.ip}</td>
                                <td>{bytes(u.rx)}</td>
                                <td>{bytes(u.tx)}</td>
                                <td className="td-actions">
                                    <div className="actions">
                                        <button
                                            className="btn-icon"
                                            title="Лимиты…"
                                            aria-label="Лимиты"
                                            onClick={() => openLimits(u)}
                                        ><IcoHistory /></button>
                                        <button
                                            className="btn-icon"
                                            title="QR-код vpn://"
                                            onClick={() => showQR(u.name)}
                                        ><IcoQR /></button>
                                        <button
                                            className="btn-icon btn-icon--danger"
                                            title="Удалить"
                                            onClick={() => deleteUser(u.name)}
                                        ><IcoTrash /></button>
                                    </div>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            <div className="user-cards-list">
                <p className="users-count">{users.length} пользователей</p>
                {users.length === 0 ? (
                    <p className="empty-cards">Нет пользователей</p>
                ) : users.map((u, i) => (
                    <div className="user-card" key={u.name}>
                        <div className="user-card-header">
                            <div className="user-card-title">
                                <span className="user-card-num">#{i + 1}</span>
                                <span className="user-card-name">{u.name}</span>
                                {u.key_gen !== serverGen && (
                                    <span className="chip chip--error">
                                        <span className="chip-dot" />
                                        {genLabel(u.key_gen)}
                                    </span>
                                )}
                                <WhenChip u={u} />
                            </div>
                            <StatusChip u={u} />
                        </div>
                        <div className="user-card-ip">
                            <IcoGlobe />
                            <span className="user-card-ip-text">{u.ip}</span>
                            <PriorityChip priority={u.priority} torrent={u.torrent} />
                            <TorrentChip torrent={u.torrent} />
                        </div>
                        <LimitsCell u={u} />
                        <div className="user-card-metrics">
                            <div className="metric-block">
                                <span className="metric-label">↓ rx</span>
                                <span className="metric-value">{bytes(u.rx)}</span>
                            </div>
                            <div className="metric-block">
                                <span className="metric-label">↑ tx</span>
                                <span className="metric-value">{bytes(u.tx)}</span>
                            </div>
                        </div>
                        <div className="user-card-actions">
                            <button
                                className="btn btn--outline"
                                aria-label="QR-код"
                                onClick={() => showQR(u.name)}
                            ><IcoQR /> QR</button>
                            <button
                                className="btn btn--outline"
                                onClick={() => openLimits(u)}
                            ><IcoHistory /> Лимиты</button>
                            <button
                                className="btn btn--danger btn--sq"
                                aria-label="Удалить"
                                onClick={() => deleteUser(u.name)}
                            ><IcoTrash /></button>
                        </div>
                    </div>
                ))}
            </div>
            </>)}

            {qrModal && (
                <div className="qr-backdrop" onClick={() => setQrModal(null)}>
                    <div className="qr-card" onClick={e => e.stopPropagation()}>
                        <p className="qr-name">{qrModal.name}</p>
                        <img
                            className="qr-img"
                            src={qrModal.dataUrl}
                            alt="QR"
                            title="Нажми чтобы скопировать vpn:// ключ"
                            onClick={async () => { await copyText(qrModal.vpnKey); showMsg('Ключ скопирован'); }}
                        />
                        <p className="qr-hint">Отсканируй в AmneziaVPN · нажми на QR чтобы скопировать</p>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setQrModal(null)}>Закрыть</button>
                            <button className="btn btn--tonal" onClick={async () => {
                                try {
                                    const conf = await vpnKeyToConf(qrModal.vpnKey);
                                    const safe = qrModal.name.replace(/[\/\\:*?"<>|]+/g, '_').trim() || 'awg';
                                    downloadFile(safe + '.conf', conf);
                                    showMsg('Конфиг скачан');
                                } catch { showMsg('Не удалось декодировать ключ'); }
                            }}>Скачать .conf</button>
                        </div>
                    </div>
                </div>
            )}

            {lim && (() => {
                const dh = dateHint(lim.date), th = trafficHint(lim.amount, lim.unit);
                const used = limUser?.usedBytes ?? 0;
                const limit = limUser?.trafficLimitBytes ?? null;
                return (
                    <div className="node-scrim" onClick={closeLimits}>
                        <div className="node-modal lim-dialog" role="dialog" aria-modal="true"
                            aria-labelledby="lim-title" onClick={e => e.stopPropagation()}>
                            <h3 className="node-modal-title" id="lim-title">Лимиты · {lim.name}</h3>
                            <p className="node-modal-hint">Пустое поле — без ограничения. Блокировка и сброс счётчика применяются сразу.</p>

                            <div className="lim-field">
                                <label className="lim-label" htmlFor="lim-date">Действует до</label>
                                <DateField
                                    id="lim-date"
                                    value={lim.date}
                                    invalid={!!limErr.date}
                                    describedBy="lim-date-hint"
                                    onOpenChange={setDateOpen}
                                    onChange={date => {
                                        setLim(v => v && { ...v, date });
                                        setLimErr(v => ({ ...v, date: undefined }));
                                    }}
                                />
                                {limErr.date
                                    ? <span className="lim-err" id="lim-date-hint">{limErr.date}</span>
                                    : <span className={`lim-hint${dh.warm ? ' lim-hint--warm' : ''}`} id="lim-date-hint">{dh.text}</span>}
                            </div>

                            <div className="lim-field">
                                <label className="lim-label" htmlFor="lim-amount">Лимит трафика</label>
                                <div className="lim-traffic">
                                    <input id="lim-amount" className="field lim-amount" type="number" min={0} step="any" inputMode="decimal"
                                        placeholder="без ограничения" value={lim.amount}
                                        onChange={e => {
                                            const amount = e.target.value;
                                            setLim(v => v && { ...v, amount });
                                            setLimErr(v => ({ ...v, amount: undefined }));
                                        }} />
                                    <select className="field lim-unit" value={lim.unit} aria-label="Единица лимита"
                                        onChange={e => setLim(v => v && { ...v, unit: e.target.value as LimitUnit })}>
                                        {LIMIT_UNITS.map(x => <option key={x} value={x}>{x}</option>)}
                                    </select>
                                </div>
                                {limErr.amount
                                    ? <span className="lim-err">{limErr.amount}</span>
                                    : <span className="lim-hint">{th.text}</span>}
                            </div>

                            <div className="lim-now">
                                <span className="lim-now-title">
                                    Израсходовано {limitBytes(used)}{limit != null ? ` из ${limitBytes(limit)}` : ''}
                                </span>
                                <span className="lim-now-sub">
                                    {limBusyNow ? blockText(limReason) : 'Доступ открыт'}
                                    {limUser?.remainingBytes != null ? ` · осталось ${limitBytes(limUser.remainingBytes)}` : ''}
                                </span>
                            </div>

                            <div className="lim-quick">
                                <button className="btn btn--outline" onClick={() => limAct('usage/reset', 'Счётчик сброшен')} disabled={limBusy}>
                                    <IcoRefresh /> Сбросить счётчик
                                </button>
                                {/* Кнопка есть при любой причине: разблокировка снимает и api-блокировку,
                                    а подпись причины уже стоит строкой выше (blockText). */}
                                {limBusyNow ? (
                                    <button className="btn btn--tonal" onClick={() => limAct('unblock', 'Доступ открыт')} disabled={limBusy}>
                                        <IcoPower /> Разблокировать
                                    </button>
                                ) : (
                                    <button className="btn btn--danger" onClick={() => limAct('block', 'Пользователь заблокирован')} disabled={limBusy}>
                                        <IcoPower /> Заблокировать
                                    </button>
                                )}
                            </div>

                            {limErr.general && !limErr.date && !limErr.amount && (
                                <p className="lim-err lim-err--general" role="alert">{limErr.general}</p>
                            )}

                            <div className="node-modal-actions">
                                <button className="btn btn--text" onClick={closeLimits}>Отмена</button>
                                <button className="btn btn--primary" onClick={saveLimits} disabled={limBusy}>Сохранить</button>
                            </div>
                        </div>
                    </div>
                );
            })()}
        </>
    );
}