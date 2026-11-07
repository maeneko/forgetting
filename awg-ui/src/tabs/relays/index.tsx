// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useCallback, useEffect, useState } from 'react';
import axios from 'axios';
import {
    apiFetch, timeAgo, copyText,
    type RelayInfo, type RelayBindInput, type RelayJoinResult, type NodeInfo, type PageProps,
} from '../../lib/shared';
import { IcoPlus, IcoRefresh, IcoTrash, IcoCopy, IcoClose, IcoSettings, IcoGear } from '../../components/icons';
import './relays.css';

// Виды привязок — те же, что в бэке (internal/ui/relays.go): vpn идёт UDP к VPN-порту
// выбранного сервера, остальные — TCP к тем же поверхностям этой панели.
const KINDS: { kind: NonNullable<RelayBindInput['kind']>; label: string; title: string }[] = [
    { kind: 'vpn', label: 'VPN-порт сервера', title: 'UDP-порт endpoint сервера: VPN-клиенты ходят через релей' },
    { kind: 'hub', label: 'Хаб нод', title: 'TCP к NODE_PORT: ноды подключаются к панели через релей' },
    { kind: 'sub', label: 'Подписка sen://', title: 'TCP к SUB_PORT: выдача и регистрация устройств' },
    { kind: 'ui',  label: 'Панель',          title: 'TCP к панели: администраторы заходят через релей' },
];
const kindLabel = (k: string): string => KINDS.find(x => x.kind === k)?.label ?? k;

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

const bindsLine = (r: RelayInfo, servers: NodeInfo[]): string => {
    const list = r.binds ?? [];
    if (!list.length) return 'пересылок нет';
    return list.map(b => {
        if (b.kind === 'vpn') {
            const s = servers.find(x => x.id === b.server_id);
            return `VPN → ${s ? s.name : `сервер #${b.server_id}`}`;
        }
        return kindLabel(b.kind);
    }).join(' · ');
};

// Общий хвост модалок: строка для `awg-ctrl join` на машине релея.
function JoinResult({ result, showMsg }: { result: RelayJoinResult; showMsg: (t: string) => void }) {
    const command = result.join ? `awg-ctrl join '${result.join}'` : '';
    return (
        <>
            <p className="node-modal-hint">
                Выполните на машине «{result.name}» от root. Строка одноразовая и действует
                {' '}{Math.max(1, Math.round((result.expires_in ?? 3600) / 60))} мин. Маршруты
                {' '}придут сами: привяжите ему серверы в настройках — панель сама пришлёт их агенту.
            </p>
            {command && <code className="node-modal-cmd">{command}</code>}
            <p className="node-modal-hint">
                Канал — TLS с проверкой отпечатка сертификата этой панели, как при добавлении сервера.
            </p>
            {command && (
                <div className="node-modal-actions">
                    <button className="btn btn--tonal" onClick={() => copyText(command).then(() => showMsg('Команда скопирована'))}>
                        <IcoCopy /> Скопировать
                    </button>
                </div>
            )}
        </>
    );
}

// Редактор привязок: общий для создания-с-нуля и настроек релея.
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
        <>
            <div className="bind-rows">
                {binds.map((b, i) => (
                    <div className="bind-row" key={i}>
                        <select
                            className="field"
                            value={b.kind}
                            title={KINDS.find(x => x.kind === b.kind)?.title}
                            onChange={e => setAt(i, { kind: e.target.value as BindForm['kind'] })}
                        >
                            {KINDS.map(k => <option key={k.kind} value={k.kind}>{k.label}</option>)}
                        </select>
                        {b.kind === 'vpn' ? (
                            <select
                                className="field"
                                value={b.serverId ?? ''}
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
                            <span className="relays-hint">эта панель</span>
                        )}
                        <input
                            className="field field--listen"
                            placeholder="порт (цель)"
                            inputMode="numeric"
                            value={b.listen}
                            onChange={e => setAt(i, { listen: e.target.value.replace(/[^0-9]/g, '') })}
                        />
                        <button className="btn-icon btn-icon--danger" title="Убрать пересылку" aria-label="Убрать пересылку"
                            onClick={() => removeAt(i)}><IcoClose /></button>
                    </div>
                ))}
            </div>
            <button className="btn btn--text relays-add-bind" disabled={binds.length >= 8}
                title={invalid ? 'Выберите сервер для привязки «VPN-порт сервера»' : ''}
                onClick={() => onChange([...binds, defaultBind()])}>
                <IcoPlus /> Добавить пересылку{binds.length >= 8 ? ' (максимум 8)' : ''}
            </button>
        </>
    );
}

// Модалка создания: релей описывается по имени — строка выдаётся без маршрутов,
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

    useEffect(() => {
        const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [onClose]);

    const submit = async () => {
        setBusy(true); setError('');
        try {
            setResult(await apiFetch('POST', '/ui/relays', token, { name: name.trim(), host: window.location.hostname }));
        } catch (e) {
            setError(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось' : 'Не удалось');
        } finally { setBusy(false); }
    };

    return (
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal" role="dialog" aria-modal="true" aria-label="Добавить релей" onClick={e => e.stopPropagation()}>
                <h3 className="node-modal-title">{result ? `Релей «${result.name}»` : 'Добавить релей'}</h3>
                {!result && (
                    <p className="node-modal-hint">
                        Релей — отдельная машина без VPN: пересылает клиентский трафик к серверам.
                        Сразу после создания выдаётся строка подключения; привязка серверов — потом,
                        в настройках релея.
                    </p>
                )}
                {!result && (
                    <input
                        className="field"
                        placeholder="Название релея"
                        value={name}
                        maxLength={40}
                        autoFocus
                        onChange={e => setName(e.target.value)}
                        onKeyDown={e => { if (e.key === 'Enter' && name.trim() && !busy) void submit(); }}
                    />
                )}
                {result && <JoinResult result={result} showMsg={showMsg} />}
                {error && <p className="login-error">{error}</p>}
                <div className="node-modal-actions">
                    <button className="btn btn--text" onClick={onClose}>{result ? 'Готово' : 'Отмена'}</button>
                    {!result && (
                        <button className="btn btn--primary" onClick={submit} disabled={busy || !name.trim()}>
                            Собрать строку
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}

// Модалка «Выдать строку» — для релея, который ещё ни разу не подключался
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
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal" role="dialog" aria-modal="true" aria-label={`Выдать строку: ${relay.name}`} onClick={e => e.stopPropagation()}>
                <h3 className="node-modal-title">{result ? `Релей «${result.name}»` : 'Выдать строку'}</h3>
                {result && <JoinResult result={result} showMsg={showMsg} />}
                {error && <p className="login-error">{error}</p>}
                <div className="node-modal-actions">
                    <button className="btn btn--text" onClick={onClose}>{result ? 'Готово' : 'Отмена'}</button>
                    {!result && <button className="btn btn--primary" disabled={busy}>Выдаём…</button>}
                </div>
            </div>
        </div>
    );
}

// Модалка настроек релея: редактор привязок. У ещё не подключившегося релея сохранение
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

    useEffect(() => {
        const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [onClose]);

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
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal" role="dialog" aria-modal="true" aria-label={`Настройки релея: ${relay.name}`} onClick={e => e.stopPropagation()}>
                <h3 className="node-modal-title">
                    {result ? `Релей «${result.name}»` : `Настройки: ${relay.name}`}
                </h3>
                {!result && (
                    <p className="node-modal-hint">
                        {relay.pending
                            ? 'Релей ещё не подключался: выберите пересылки и сохраните — строка выдаётся заново.'
                            : 'Панель сама доставит маршруты агенту релея: тот перепишет cli.env и перезапустит awg-relay.'}
                    </p>
                )}
                {!result && <BindsEditor binds={binds} serverList={serverList} onChange={setBinds} />}
                {result && <JoinResult result={result} showMsg={showMsg} />}
                {error && <p className="login-error">{error}</p>}
                <div className="node-modal-actions">
                    <button className="btn btn--text" onClick={onClose}>{result ? 'Готово' : 'Отмена'}</button>
                    {!result && (
                        <button className="btn btn--primary" onClick={submit} disabled={busy || invalid}>
                            Сохранить
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}

// Вкладка «Релеи»: двухэтапный флоу — подключить строкой, привязать серверы из панели.
export default function RelaysPage({ token, showMsg }: PageProps) {
    const [relays, setRelays]         = useState<RelayInfo[]>([]);
    const [hubOk, setHubOk]           = useState(true);    // приём нод включён (NODE_PORT)
    const [serverList, setServerList] = useState<NodeInfo[]>([]);
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
        } catch { showMsg('Ошибка загрузки'); }
    }, [showMsg]);

    useEffect(() => {
        if (!token) return;
        void load(token);
        const t = setInterval(() => void load(token), 15_000);
        return () => clearInterval(t);
    }, [token, load]);

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

    const status = (r: RelayInfo): string =>
        r.pending ? 'ждёт строку' : r.online ? 'на связи' : r.last_seen ? `${timeAgo(r.last_seen)} назад` : 'нет связи';

    return (
        <>
            <div className="toolbar">
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

            {!hubOk && (
                <p className="relays-hint relays-hint--warn">
                    Приём нод не настроен (NODE_PORT в .env) — агенты релеев не подключатся к панели.
                </p>
            )}

            <div className="table-card">
                <table>
                    <thead>
                        <tr>
                            <th>#</th>
                            <th>Релей</th>
                            <th>Статус</th>
                            <th>Пересылки</th>
                            <th>Версия</th>
                            <th className="td-actions"></th>
                        </tr>
                    </thead>
                    <tbody>
                        {relays.map((r, i) => (
                            <tr key={r.id}>
                                <td>{i + 1}</td>
                                <td>{r.name}</td>
                                <td>
                                    <span className={`chip chip--${r.online ? 'online' : 'offline'}`}>
                                        <span className="chip-dot" />
                                        {status(r)}
                                    </span>
                                </td>
                                <td><span className="relay-binds-line" title={bindsLine(r, serverList)}>{bindsLine(r, serverList)}</span></td>
                                <td>{r.version ? `v${r.version}` : r.pending ? '—' : 'v?.?.?'}</td>
                                <td className="td-actions">
                                    <div className="actions">
                                        <button className="btn-icon" title="Настройки релея"
                                            onClick={() => setSettings(r)}>
                                            <IcoSettings />
                                        </button>
                                        {r.pending && (
                                            <button className="btn-icon" title="Выдать строку подключения"
                                                onClick={() => setJoining(r)}>
                                                <IcoCopy />
                                            </button>
                                        )}
                                        <button className="btn-icon btn-icon--danger" title="Удалить" onClick={() => void removeRelay(r)}>
                                            <IcoTrash />
                                        </button>
                                    </div>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            <div className="relays-cards">
                <p className="relays-count">{relays.length} релеев</p>
                {relays.length === 0 ? (
                    <p className="empty-cards">
                        Релеев нет. «Добавить релей» выдаёт строку awgrelay://, серверы привязываются после подключения.
                    </p>
                ) : relays.map(r => (
                    <div className="user-card" key={r.id}>
                        <div className="user-card-header">
                            <div className="user-card-title">
                                <span className="user-card-name">{r.name}</span>
                            </div>
                            <span className={`chip chip--${r.online ? 'online' : 'offline'}`}>
                                <span className="chip-dot" />
                                {status(r)}
                            </span>
                        </div>
                        <div className="relay-loss">
                            <IcoGear /> <span>{bindsLine(r, serverList)}</span>
                        </div>
                        <div className="user-card-actions">
                            <button className="btn btn--outline" onClick={() => setSettings(r)}>
                                <IcoSettings /> Настройки
                            </button>
                            {r.pending && (
                                <button className="btn btn--outline" onClick={() => setJoining(r)}>
                                    <IcoCopy /> Выдать строку
                                </button>
                            )}
                            <button className="btn btn--danger btn--sq" aria-label="Удалить" onClick={() => void removeRelay(r)}>
                                <IcoTrash />
                            </button>
                        </div>
                    </div>
                ))}
            </div>

            {creating && <RelayCreateModal token={token} onClose={closeAndReload} showMsg={showMsg} />}
            {settings && <RelaySettingsModal token={token} relay={settings} serverList={serverList} onClose={closeAndReload} showMsg={showMsg} />}
            {joining && <RelayJoinModal token={token} relay={joining} onClose={closeAndReload} showMsg={showMsg} />}
        </>
    );
}
