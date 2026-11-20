// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useCallback, useEffect, useId, useState } from 'react';
import axios from 'axios';
import {
    apiFetch, timeAgo, copyText, limitDate,
    type RelayInfo, type RelayBindInput, type RelayJoinResult, type NodeInfo, type PageProps,
} from '../../lib/shared';
import { IcoPlus, IcoRefresh, IcoTrash, IcoCopy, IcoClose, IcoSettings, IcoRelay } from '../../components/icons';
import './relays.css';

// Виды привязок — те же, что в бэке (internal/ui/relays.go): vpn идёт UDP к VPN-порту
// выбранного сервера, остальные — TCP к тем же поверхностям этой панели.
const KINDS: { kind: NonNullable<RelayBindInput['kind']>; label: string; short: string; title: string }[] = [
    { kind: 'vpn', label: 'VPN-порт сервера', short: 'VPN',      title: 'UDP-порт endpoint сервера: VPN-клиенты ходят через релей' },
    { kind: 'hub', label: 'Хаб нод',          short: 'Хаб',      title: 'TCP к NODE_PORT: ноды подключаются к панели через релей' },
    { kind: 'sub', label: 'Подписка sen://',  short: 'Подписка', title: 'TCP к SUB_PORT: выдача и регистрация устройств' },
    { kind: 'ui',  label: 'Панель',           short: 'Панель',   title: 'TCP к панели: администраторы заходят через релей' },
];
const kindLabel = (k: string): string => KINDS.find(x => x.kind === k)?.label ?? k;
const kindShort = (k: string): string => KINDS.find(x => x.kind === k)?.short ?? k;
const kindTitle = (k: string): string => KINDS.find(x => x.kind === k)?.title ?? '';

// Числа и единицы не разрываются переносом: «3 привязки» остаётся одной фразой.
const NBSP = '\u00A0';
const plural = (n: number, one: string, few: string, many: string): string => {
    const m10 = n % 10, m100 = n % 100;
    return m10 === 1 && m100 !== 11 ? one
        : m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14) ? few : many;
};
const relaysWord = (n: number): string => `${n}${NBSP}${plural(n, 'релей', 'релея', 'релеев')}`;
const bindsWord  = (n: number): string => `${n}${NBSP}${plural(n, 'привязка', 'привязки', 'привязок')}`;

// Редактируемая привязка (форма): listen пуст — панель возьмёт порт цели.
interface BindForm {
    kind:     NonNullable<RelayBindInput['kind']>;
    serverId: number | null;
    listen:   string;
}
const defaultBind = (): BindForm => ({ kind: 'vpn', serverId: null, listen: '' });
// Список привязок релея → форма редактора; пусто — релей пока никуда не пересылает.
const formOf = (binds: RelayBindInput[] | undefined): BindForm[] =>
    binds?.length ? binds.map(b => ({
        kind: b.kind, serverId: b.server_id ?? null, listen: b.listen ? String(b.listen) : '',
    })) : [];

// ── Состояние релея ──────────────────────────────────────────────────────────
// pending — запись есть, машина ещё ни разу не подключалась; offline — подключалась,
// но сейчас связи нет (последний контакт — в подписи под именем).
type RelayState = 'online' | 'offline' | 'pending';
const relayState = (r: RelayInfo): RelayState => r.pending ? 'pending' : r.online ? 'online' : 'offline';
const stateClass = (s: RelayState): string => s === 'online' ? 'chip--online' : s === 'pending' ? 'chip--warm' : 'chip--offline';
const stateText = (r: RelayInfo): string => r.pending ? 'ждёт подключения'
    : r.online ? 'на связи'
    : r.last_seen ? `${timeAgo(r.last_seen)} назад` : 'нет связи';

// Куда ведёт привязка: имя сервера (vpn) или поверхность этой же панели.
interface BindTarget { name: string; missing?: boolean; offline?: boolean }
const bindTarget = (b: RelayBindInput, servers: NodeInfo[]): BindTarget => {
    if (b.kind !== 'vpn') return { name: 'эта панель' };
    const s = servers.find(x => x.id === b.server_id);
    if (!s) return { name: b.server_id == null ? 'сервер не выбран' : `сервер #${b.server_id}`, missing: true };
    return { name: s.name, offline: !s.online };
};

// ── Карточка релея ───────────────────────────────────────────────────────────
// Иерархия: имя → состояние и версия → пересылки → действия. Действия всегда внизу
// слева, удаление — отдельно справа: у него не тот вес, что у рабочих кнопок.
function RelayCard({ relay, servers, index, onSettings, onJoin, onRemove }: {
    relay:      RelayInfo;
    servers:    NodeInfo[];
    index:      number;
    onSettings: () => void;
    onJoin:     () => void;
    onRemove:   () => void;
}) {
    const binds = relay.binds ?? [];
    const state = relayState(relay);
    const nameId = `relay-${relay.id}-name`;
    const meta = relay.pending
        ? `id ${relay.id}${NBSP}· ещё не подключался`
        : relay.last_seen
            ? `id ${relay.id}${NBSP}· последний контакт ${limitDate(relay.last_seen)}`
            : `id ${relay.id}${NBSP}· не подключался`;

    return (
        <article className="relay-card" aria-labelledby={nameId} style={{ animationDelay: `${Math.min(index, 6) * 40}ms` }}>
            <header className="relay-card-head">
                <span className="relay-glyph" aria-hidden="true"><IcoRelay /></span>
                <h3 className="relay-name" id={nameId}>{relay.name}</h3>
                <div className="relay-state">
                    <span className={`chip ${stateClass(state)}`}>
                        <span className="chip-dot" />
                        {stateText(relay)}
                    </span>
                    {relay.version && (
                        <span className="relay-ver" title="Версия Forgetting на релее">v{relay.version}</span>
                    )}
                </div>
                <p className="relay-sub">{meta}</p>
            </header>

            <section className="relay-binds" aria-label={`Пересылки релея «${relay.name}»`}>
                <div className="relay-binds-head">
                    <span className="relay-binds-title">Пересылки</span>
                    <span className="relay-binds-count">{bindsWord(binds.length)}</span>
                </div>
                {binds.length === 0 ? (
                    <p className="relay-binds-empty">
                        Привязок нет: релей подключён, но ничего не пересылает. Серверы выбираются в настройках.
                    </p>
                ) : (
                    <ul className="relay-bind-list">
                        {binds.map((b, i) => {
                            const target = bindTarget(b, servers);
                            return (
                                <li className="relay-bind" key={i}>
                                    <span className="relay-kind" title={kindTitle(b.kind)}>{kindShort(b.kind)}</span>
                                    <span className="relay-bind-target">
                                        <span className="relay-bind-name" title={`${kindLabel(b.kind)} · ${target.name}`}>{target.name}</span>
                                        {target.missing && <span className="chip chip--error relay-bind-flag">сервер недоступен</span>}
                                        {target.offline && <span className="chip chip--warm relay-bind-flag">не на связи</span>}
                                    </span>
                                    <code className="relay-bind-port">{b.listen ? `:${b.listen}` : '—'}</code>
                                </li>
                            );
                        })}
                    </ul>
                )}
            </section>

            <footer className="relay-card-foot">
                <button className="btn btn--outline" onClick={onSettings}>
                    <IcoSettings /> Настройки
                </button>
                {relay.pending && (
                    <button className="btn btn--tonal" onClick={onJoin}>
                        <IcoCopy /> Строка подключения
                    </button>
                )}
                <button className="btn-icon btn-icon--danger relay-card-del" title="Удалить релей"
                    aria-label={`Удалить релей ${relay.name}`} onClick={onRemove}>
                    <IcoTrash />
                </button>
            </footer>
        </article>
    );
}

// ── Загрузка, ошибка, пусто ──────────────────────────────────────────────────
function RelaySkeleton() {
    return (
        <div className="relay-list" role="status" aria-live="polite">
            <span className="relay-sr">Загрузка релеев…</span>
            {[0, 1].map(i => (
                <div className="relay-card relay-card--skeleton" key={i} aria-hidden="true">
                    <div className="relay-sk-head">
                        <span className="relay-sk relay-sk-glyph" />
                        <span className="relay-sk-stack">
                            <span className="relay-sk relay-sk-line relay-sk-line--name" />
                            <span className="relay-sk relay-sk-line relay-sk-line--meta" />
                        </span>
                    </div>
                    <span className="relay-sk relay-sk-line" />
                    <span className="relay-sk relay-sk-line relay-sk-line--short" />
                </div>
            ))}
        </div>
    );
}

function RelayError({ onRetry }: { onRetry: () => void }) {
    return (
        <div className="relay-state-card" role="alert">
            <span className="relay-state-icon relay-state-icon--error" aria-hidden="true"><IcoRelay /></span>
            <p className="relay-state-title">Ошибка загрузки</p>
            <p className="relay-state-hint">Панель не ответила. Проверьте связь с сервером и попробуйте ещё раз.</p>
            <button className="btn btn--tonal" onClick={onRetry}><IcoRefresh /> Повторить</button>
        </div>
    );
}

function RelayEmpty({ hubOk, onCreate }: { hubOk: boolean; onCreate: () => void }) {
    return (
        <div className="relay-state-card">
            <span className="relay-state-icon" aria-hidden="true"><IcoRelay /></span>
            <p className="relay-state-title">Релеев нет</p>
            <p className="relay-state-hint">
                Релей — машина без VPN: принимает клиентов и пересылает их к серверам панели.
                Добавьте релей, выполните на нём строку подключения, а серверы выберите здесь —
                маршруты панель пришлёт сама.
            </p>
            <button className="btn btn--primary" onClick={onCreate} disabled={!hubOk}
                title={!hubOk ? 'Приём нод не настроен: релей подключается агентом, как сервер' : ''}>
                <IcoPlus /> Добавить релей
            </button>
        </div>
    );
}

// ── Общая оболочка модалок ───────────────────────────────────────────────────
// Одинаковая ширина, заголовок с крестиком, прокручиваемое тело и футер с действиями.
// На телефоне диалог занимает весь экран, как остальные длинные диалоги панели.
function RelayModal({ title, onClose, children, footer }: {
    title:    string;
    onClose:  () => void;
    children: React.ReactNode;
    footer:   React.ReactNode;
}) {
    const titleId = useId();
    useEffect(() => {
        const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [onClose]);

    return (
        <div className="node-scrim relay-scrim" onClick={onClose}>
            <div className="node-modal relay-modal" role="dialog" aria-modal="true" aria-labelledby={titleId}
                onClick={e => e.stopPropagation()}>
                <header className="relay-modal-head">
                    <h3 className="node-modal-title relay-modal-title" id={titleId}>{title}</h3>
                    <button className="btn-icon relay-modal-x" aria-label="Закрыть" onClick={onClose}><IcoClose /></button>
                </header>
                <div className="relay-modal-body">{children}</div>
                <div className="node-modal-actions relay-modal-foot">{footer}</div>
            </div>
        </div>
    );
}

// Общий хвост модалок: строка для `awg-ctrl join` на машине релея.
function JoinResult({ result, showMsg }: { result: RelayJoinResult; showMsg: (t: string) => void }) {
    const command = result.join ? `awg-ctrl join '${result.join}'` : '';
    const minutes = Math.max(1, Math.round((result.expires_in ?? 3600) / 60));
    return (
        <>
            <p className="node-modal-hint">
                Выполните на машине «{result.name}» от root. Строка одноразовая и действует
                {' '}{minutes}{NBSP}мин. Маршруты придут сами: привяжите ему серверы
                {' '}в настройках — панель сама пришлёт их агенту.
            </p>
            {command && (
                <div className="relay-cmd-row">
                    <code className="relay-cmd">{command}</code>
                    <button className="btn btn--tonal relay-cmd-copy"
                        onClick={() => copyText(command).then(() => showMsg('Команда скопирована'))}>
                        <IcoCopy /> Скопировать
                    </button>
                </div>
            )}
            <p className="node-modal-hint">
                Канал — TLS с проверкой отпечатка сертификата этой панели, как при добавлении сервера.
            </p>
        </>
    );
}

// ── Редактор привязок ────────────────────────────────────────────────────────
// Строки-таблицей: что пересылать → куда → порт на релее. Пустой порт — порт цели,
// это видно подсказкой под полем, а не угадывается по плейсхолдеру.
function BindsEditor({ binds, serverList, onChange }: {
    binds:      BindForm[];
    serverList: NodeInfo[];
    onChange:   (v: BindForm[]) => void;
}) {
    const setAt = (i: number, patch: Partial<BindForm>) =>
        onChange(binds.map((b, j) => (j === i ? { ...b, ...patch } : b)));
    const removeAt = (i: number) => onChange(binds.filter((_, j) => j !== i));
    const invalid = !!binds.find(b => b.kind === 'vpn' && b.serverId === null);
    return (
        <div className="relay-editor">
            <div className="relay-editor-head" aria-hidden="true">
                <span>Что пересылать</span>
                <span>Куда</span>
                <span>Порт на релее</span>
                <span />
            </div>

            {binds.length === 0 ? (
                <p className="relay-editor-empty">
                    Пересылок нет: релей подключится, но трафик пересылать не будет.
                </p>
            ) : (
                <div className="relay-bind-rows">
                    {binds.map((b, i) => (
                        <div className="relay-bind-row" key={i}>
                            <label className="relay-edit-field">
                                <span className="relay-edit-label">Что пересылать</span>
                                <select
                                    className="field"
                                    value={b.kind}
                                    title={kindTitle(b.kind)}
                                    aria-label={`Пересылка ${i + 1}: что пересылать`}
                                    onChange={e => setAt(i, { kind: e.target.value as BindForm['kind'] })}
                                >
                                    {KINDS.map(k => <option key={k.kind} value={k.kind}>{k.label}</option>)}
                                </select>
                            </label>
                            <label className="relay-edit-field">
                                <span className="relay-edit-label">Куда</span>
                                {b.kind === 'vpn' ? (
                                    <select
                                        className="field"
                                        value={b.serverId ?? ''}
                                        aria-label={`Пересылка ${i + 1}: сервер`}
                                        onChange={e => setAt(i, { serverId: e.target.value === '' ? null : Number(e.target.value) })}
                                    >
                                        <option value="">Сервер…</option>
                                        {serverList.map(s => (
                                            <option key={s.id} value={s.id}>
                                                {s.local ? `${s.name} (этот сервер)` : s.name}
                                            </option>
                                        ))}
                                    </select>
                                ) : (
                                    <span className="relay-edit-static">эта панель</span>
                                )}
                            </label>
                            <label className="relay-edit-field">
                                <span className="relay-edit-label">Порт на релее</span>
                                <input
                                    className="field"
                                    placeholder="порт цели"
                                    inputMode="numeric"
                                    aria-label={`Пересылка ${i + 1}: порт на релее`}
                                    value={b.listen}
                                    onChange={e => setAt(i, { listen: e.target.value.replace(/[^0-9]/g, '') })}
                                />
                            </label>
                            <button className="btn-icon btn-icon--neutral relay-bind-del"
                                title="Убрать пересылку" aria-label={`Убрать пересылку ${i + 1}`}
                                onClick={() => removeAt(i)}><IcoTrash /></button>
                        </div>
                    ))}
                </div>
            )}

            <p className="relay-editor-hint">
                Пустой порт — порт цели. Если он занят другой пересылкой, панель возьмёт следующий свободный.
            </p>
            <button className="btn btn--text relay-add-bind" disabled={binds.length >= 8}
                title={invalid ? 'Выберите сервер для привязки «VPN-порт сервера»' : ''}
                onClick={() => onChange([...binds, defaultBind()])}>
                <IcoPlus /> Добавить пересылку{binds.length >= 8 ? ' (максимум 8)' : ''}
            </button>
        </div>
    );
}

// ── Модалки ──────────────────────────────────────────────────────────────────
// Создание: релей описывается по имени — строка выдаётся без маршрутов,
// серверы привяжутся из настроек после подключения (панель пришлёт их агенту).
function RelayCreateModal({ token, onClose, showMsg }: {
    token:   string;
    onClose: () => void;
    showMsg: (t: string) => void;
}) {
    const [name, setName]     = useState('');
    const [busy, setBusy]     = useState(false);
    const [error, setError]   = useState('');
    const [result, setResult] = useState<RelayJoinResult | null>(null);

    const submit = async () => {
        setBusy(true); setError('');
        try {
            setResult(await apiFetch('POST', '/ui/relays', token, { name: name.trim(), host: window.location.hostname }));
        } catch (e) {
            setError(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось' : 'Не удалось');
        } finally { setBusy(false); }
    };

    return (
        <RelayModal
            title={result ? `Подключение · ${result.name}` : 'Добавить релей'}
            onClose={onClose}
            footer={result ? (
                <button className="btn btn--primary" onClick={onClose}>Готово</button>
            ) : (
                <>
                    <button className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button className="btn btn--primary" onClick={submit} disabled={busy || !name.trim()}>
                        {busy ? 'Собираем…' : 'Собрать строку'}
                    </button>
                </>
            )}
        >
            {!result && (
                <p className="node-modal-hint">
                    Релей — отдельная машина без VPN: пересылает клиентский трафик к серверам.
                    Сразу после создания выдаётся строка подключения; привязка серверов — потом,
                    в настройках релея.
                </p>
            )}
            {!result && (
                <label className="relay-field">
                    <span className="relay-field-label">Название релея</span>
                    <input
                        className="field"
                        placeholder="Например, Франкфурт"
                        value={name}
                        maxLength={40}
                        autoFocus
                        onChange={e => setName(e.target.value)}
                        onKeyDown={e => { if (e.key === 'Enter' && name.trim() && !busy) void submit(); }}
                    />
                    <span className="relay-field-hint">Так релей будет называться в списке и в строке подключения.</span>
                </label>
            )}
            {result && <JoinResult result={result} showMsg={showMsg} />}
            {error && <p className="relay-form-error" role="alert">{error}</p>}
        </RelayModal>
    );
}

// «Выдать строку» — для релея, который ещё ни разу не подключался
// (привязки при этом не меняются).
function RelayJoinModal({ token, relay, onClose, showMsg }: {
    token:   string;
    relay:   RelayInfo;
    onClose: () => void;
    showMsg: (t: string) => void;
}) {
    const [result, setResult] = useState<RelayJoinResult | null>(null);
    const [error, setError]   = useState('');
    const [busy, setBusy]     = useState(false);

    const issue = useCallback(async () => {
        setBusy(true); setError('');
        try {
            setResult(await apiFetch('POST', `/ui/relays/${relay.id}/join`, token, { host: window.location.hostname }));
        } catch (e) {
            setError(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось' : 'Не удалось');
        } finally { setBusy(false); }
    }, [relay.id, token]);

    useEffect(() => {
        void issue();
    }, [issue]);

    return (
        <RelayModal
            title={`Подключение · ${result?.name ?? relay.name}`}
            onClose={onClose}
            footer={result ? (
                <button className="btn btn--primary" onClick={onClose}>Готово</button>
            ) : error ? (
                <>
                    <button className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button className="btn btn--primary" onClick={issue} disabled={busy}>
                        <IcoRefresh /> Повторить
                    </button>
                </>
            ) : (
                <>
                    <button className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button className="btn btn--primary" disabled>Выдаём…</button>
                </>
            )}
        >
            {result && <JoinResult result={result} showMsg={showMsg} />}
            {!result && !error && <p className="node-modal-hint">Панель готовит строку подключения…</p>}
            {error && <p className="relay-form-error" role="alert">{error}</p>}
        </RelayModal>
    );
}

// Настройки: редактор привязок. У ещё не подключившегося релея сохранение
// выдаёт строку заново (старый секрет одноразовый), у подключённого — пуш маршрутов
// агенту через панель.
function RelaySettingsModal({ token, relay, serverList, onClose, showMsg }: {
    token:      string;
    relay:      RelayInfo;
    serverList: NodeInfo[];
    onClose:    () => void;
    showMsg:    (t: string) => void;
}) {
    const [binds, setBinds]   = useState<BindForm[]>(() => formOf(relay.binds));
    const [busy, setBusy]     = useState(false);
    const [error, setError]   = useState('');
    const [result, setResult] = useState<RelayJoinResult | null>(null);

    const submit = async () => {
        const out: RelayBindInput[] = binds.map(b => {
            const v: RelayBindInput = { kind: b.kind };
            if (b.kind === 'vpn' && b.serverId !== null) v.server_id = b.serverId;
            if (b.listen !== '') v.listen = Number(b.listen);
            return v;
        });
        setBusy(true); setError('');
        try {
            const r: RelayJoinResult = await apiFetch('POST', `/ui/relays/${relay.id}/bind`, token, { binds: out, host: window.location.hostname });
            if (r.join) {           // релей ещё не подключался: строка перевыпущена
                setResult(r);
            } else {                // подключённому привязки ушли агенту по пушу
                showMsg(r.note ? r.note : r.pushed ? 'Привязки применены' : 'Привязки сохранены');
                onClose();
            }
        } catch (e) {
            setError(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось' : 'Не удалось');
        } finally { setBusy(false); }
    };

    const invalid = !!binds.find(b => b.kind === 'vpn' && b.serverId === null);

    return (
        <RelayModal
            title={result ? `Подключение · ${result.name}` : `Настройки · ${relay.name}`}
            onClose={onClose}
            footer={result ? (
                <button className="btn btn--primary" onClick={onClose}>Готово</button>
            ) : (
                <>
                    <button className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button className="btn btn--primary" onClick={submit} disabled={busy || invalid}>
                        Сохранить
                    </button>
                </>
            )}
        >
            {!result && (
                <p className="node-modal-hint">
                    {relay.pending
                        ? 'Релей ещё не подключался: выберите пересылки и сохраните — строка выдаётся заново.'
                        : 'Панель сама доставит маршруты агенту релея: тот перепишет cli.env и перезапустит awg-relay.'}
                </p>
            )}
            {!result && <BindsEditor binds={binds} serverList={serverList} onChange={setBinds} />}
            {result && <JoinResult result={result} showMsg={showMsg} />}
            {error && <p className="relay-form-error" role="alert">{error}</p>}
        </RelayModal>
    );
}

// ── Вкладка ──────────────────────────────────────────────────────────────────
// Двухэтапный флоу: подключить строкой, привязать серверы из панели.
export default function RelaysPage({ token, showMsg }: PageProps) {
    const [relays, setRelays]         = useState<RelayInfo[] | null>(null);
    const [hubOk, setHubOk]           = useState(true);    // приём нод включён (NODE_PORT)
    const [serverList, setServerList] = useState<NodeInfo[]>([]);
    const [failed, setFailed]         = useState(false);
    const [creating, setCreating]     = useState(false);
    const [settings, setSettings]     = useState<RelayInfo | null>(null);
    const [joining, setJoining]       = useState<RelayInfo | null>(null);

    const load = useCallback(async (tok: string) => {
        try {
            const [relayData, nodeData] = await Promise.all([
                apiFetch('GET', '/ui/relays', tok),
                apiFetch('GET', '/ui/nodes', tok),   // серверы для привязок vpn (релеев в этом списке нет)
            ]);
            setRelays(relayData.relays ?? []);
            setHubOk(!!relayData.hub);
            setServerList(nodeData.nodes ?? []);
            setFailed(false);
        } catch { setFailed(true); showMsg('Ошибка загрузки'); }
    }, [showMsg]);

    useEffect(() => {
        if (!token) return;
        void load(token);
        const t = setInterval(() => void load(token), 15_000);
        return () => clearInterval(t);
    }, [token, load]);

    const retry = () => { setFailed(false); void load(token); };

    const closeAndReload = useCallback(() => {
        setCreating(false);
        setSettings(null);
        setJoining(null);
        void load(token);
    }, [token, load]);

    const removeRelay = useCallback(async (r: RelayInfo) => {
        if (!confirm(`Удалить «${r.name}»? Запись и привязки уйдут вместе; awg-relay на машине отключится.`)) return;
        try {
            await apiFetch('DELETE', `/ui/relays/${r.id}`, token);
            showMsg('Релей удалён');
            await load(token);
        } catch (e) {
            showMsg(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось удалить' : 'Не удалось удалить');
        }
    }, [token, load, showMsg]);

    const list = relays ?? [];
    const online = list.filter(r => r.online).length;
    const pending = list.filter(r => r.pending).length;
    const summary = list.length
        ? `${relaysWord(list.length)}${NBSP}· ${online}${NBSP}на связи${pending ? `${NBSP}· ${pending}${NBSP}ждёт подключения` : ''}`
        : '';

    return (
        <>
            {!hubOk && (
                <p className="relay-note relay-note--warn">
                    Приём нод не настроен (NODE_PORT в .env) — агенты релеев не подключатся к панели.
                </p>
            )}

            <p className="relay-note">
                Это экспериментальная функция: релеи появились недавно — поведение (ключи, привязки,
                подписка через релей) может меняться; проверяйте связку до боевого запуска.
            </p>

            <div className="toolbar relay-toolbar">
                {relays !== null && summary && <p className="relay-summary">{summary}</p>}
                <div className="toolbar-btns">
                    <button
                        className="btn btn--primary"
                        onClick={() => setCreating(true)}
                        disabled={!hubOk}
                        title={!hubOk ? 'Приём нод не настроен: релей подключается агентом, как сервер' : ''}
                    >
                        <IcoPlus /> Добавить релей
                    </button>
                    <button className="btn btn--tonal" onClick={() => void load(token)}>
                        <IcoRefresh /> Обновить
                    </button>
                </div>
            </div>

            {relays === null ? (
                failed ? <RelayError onRetry={retry} /> : <RelaySkeleton />
            ) : list.length === 0 ? (
                failed ? <RelayError onRetry={retry} /> : <RelayEmpty hubOk={hubOk} onCreate={() => setCreating(true)} />
            ) : (
                <div className="relay-list">
                    {list.map((r, i) => (
                        <RelayCard
                            key={r.id}
                            relay={r}
                            servers={serverList}
                            index={i}
                            onSettings={() => setSettings(r)}
                            onJoin={() => setJoining(r)}
                            onRemove={() => void removeRelay(r)}
                        />
                    ))}
                </div>
            )}

            {creating && <RelayCreateModal token={token} onClose={closeAndReload} showMsg={showMsg} />}
            {settings && <RelaySettingsModal token={token} relay={settings} serverList={serverList} onClose={closeAndReload} showMsg={showMsg} />}
            {joining && <RelayJoinModal token={token} relay={joining} onClose={closeAndReload} showMsg={showMsg} />}
        </>
    );
}
