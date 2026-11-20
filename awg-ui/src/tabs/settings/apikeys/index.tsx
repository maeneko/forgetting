// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { Fragment, useState, useEffect, useCallback, useId } from 'react';
import { apiFetch, copyText, getServerId, type NodeInfo, type PageProps } from '../../../lib/shared';
import { IcoPlus, IcoTrash, IcoCopy, IcoSettings } from '../../../components/icons';
import ServerPicker, { serverName } from '../../../components/ServerPicker';
import './apikeys.css';

// Вкладка «API-ключи»: ключи внешнего API (/api/v1). Открытый ключ показывается
// один раз при создании (в БД хранится только хэш). Раздел вкладки «Настройки».
//
// Доступ к серверам: новый ключ видит ВСЕ серверы — включая те, что добавят позже
// (allServers, servers: null). Ограничение набора — вторичное действие: строка «Доступ»
// по умолчанию свёрнута в чип «все серверы» и раскрывается в ServerPicker по ссылке.
// /api/v1/users работает с одним сервером за запрос (X-Server-Id), мастер-ключи видны,
// только если весь их набор входит в доступ ключа.

// Ответ GET /ui/apikeys. Контракт сервера — allServers/defaultServerId/createdAt/lastUsed;
// старые имена (label/server_id/created_at/last_used) читаются как запасные, чтобы панель
// не ломалась на сервере, который ещё не обновлён.
interface ApiKey {
    id:              number;
    name:            string;
    prefix:          string;
    allServers:      boolean;
    servers:         number[] | null;   // null — все серверы (включая добавленные позже)
    defaultServerId: number;            // сервер по умолчанию для /api/v1/users
    createdAt:       number;
    lastUsed:        number | null;
}

const keyOf = (raw: any): ApiKey => {
    const servers: number[] | null = Array.isArray(raw?.servers) ? raw.servers : null;
    return {
        id:              raw?.id,
        name:            raw?.name ?? raw?.label ?? `#${raw?.id}`,
        prefix:          raw?.prefix ?? '',
        allServers:      typeof raw?.allServers === 'boolean' ? raw.allServers : servers === null,
        servers:         typeof raw?.allServers === 'boolean' && raw.allServers ? null : servers,
        defaultServerId: raw?.defaultServerId ?? raw?.server_id ?? 0,
        createdAt:       raw?.createdAt ?? raw?.created_at ?? 0,
        lastUsed:        raw?.lastUsed ?? raw?.last_used ?? null,
    };
};

// Та же формулировка, что у сервера на пустой набор: клиент и сервер говорят одинаково.
const EMPTY_SERVERS = 'servers: пустой набор недопустим';

// Одна строка пояснения — общая для диалога создания и настроек ключа.
const ACCESS_HINT = '/api/v1/users работает с одним сервером за запрос (X-Server-Id), '
    + 'мастер-ключи видны, только если весь их набор входит в доступ ключа.';

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

// Короткая относительная дата для «Последний раз»: «только что», «5 мин назад», «3 ч назад»,
// «2 дн назад», дальше — дата. Полные дата и время — в подсказке.
function shortAgo(ts: number | null): { text: string; full: string } | null {
    if (!ts) return null;
    const d = new Date(ts * 1000);
    if (Number.isNaN(d.getTime())) return null;
    const sec = Math.max(0, Math.floor(Date.now() / 1000) - ts);
    const day = (n: number) => String(n).padStart(2, '0');
    const text = sec < 60    ? 'только что'
        : sec < 3600         ? `${Math.floor(sec / 60)} мин назад`
        : sec < 86400        ? `${Math.floor(sec / 3600)} ч назад`
        : sec < 7 * 86400    ? `${Math.floor(sec / 86400)} дн назад`
        : `${day(d.getDate())}.${day(d.getMonth() + 1)}${d.getFullYear() === new Date().getFullYear() ? '' : '.' + d.getFullYear()}`;
    return { text, full: d.toLocaleString() };
}

const shortDate = (ts: number): string => ts ? new Date(ts * 1000).toLocaleDateString() : '—';

// Строка «Доступ»: чип «все серверы» + ссылка «изменить…», которая раскрывает ServerPicker.
// Всё, что не «все серверы», — явно ограниченный набор; вернуть полноту — ссылкой
// «разрешить все серверы» (all: true / servers: null). Используется и в диалоге создания,
// и в настройках ключа, чтобы поведение было одинаковым.
function AccessLine({ nodes, all, servers, expanded, onExpand, onChange }: {
    nodes:    NodeInfo[];
    all:      boolean;
    servers:  number[];
    expanded: boolean;
    onExpand: (v: boolean) => void;
    onChange: (all: boolean, next: number[]) => void;
}) {
    const every = nodes.map(n => n.id);
    const uid = useId();
    return (
        <div className="apikey-access">
            <div className="apikey-access-row">
                <span className="apikey-access-name" id={`${uid}-name`}>Доступ</span>
                {all && !expanded ? (
                    <>
                        <span className="chip chip--offline apikey-all" title="Включая добавленные позже">все серверы</span>
                        <button type="button" className="apikey-link" aria-expanded={false} aria-controls={`${uid}-picker`}
                            onClick={() => onExpand(true)}>изменить…</button>
                    </>
                ) : (
                    <>
                        <span className="chip chip--offline apikey-all" title="Только отмеченные: новые серверы в ключ не попадут">
                            {servers.length} из {nodes.length}
                        </span>
                        {all
                            ? <button type="button" className="apikey-link" aria-expanded={true} aria-controls={`${uid}-picker`}
                                onClick={() => onExpand(false)}>свернуть</button>
                            : <button type="button" className="apikey-link" aria-expanded={true} aria-controls={`${uid}-picker`}
                                onClick={() => { onExpand(false); onChange(true, every); }}>разрешить все серверы</button>}
                    </>
                )}
            </div>
            {expanded && (
                <div className="apikey-picker" id={`${uid}-picker`} role="group" aria-labelledby={`${uid}-name`}>
                    <ServerPicker nodes={nodes} selected={servers} onChange={next => onChange(false, next)} />
                </div>
            )}
        </div>
    );
}

export default function ApiKeysPage({ token, showMsg }: PageProps) {
    const [keys, setKeys]     = useState<ApiKey[]>([]);
    const [nodes, setNodes]   = useState<NodeInfo[]>([]);
    const [created, setCreated] = useState<{ label: string; key: string } | null>(null);

    // Диалог создания: имя и доступ. По умолчанию — все серверы, набор не трогаем.
    const [newOpen, setNewOpen]   = useState(false);
    const [name, setName]         = useState('');
    const [restrict, setRestrict] = useState(false);
    const [picked, setPicked]     = useState<number[]>([]);
    const [busy, setBusy]         = useState(false);
    const [newErr, setNewErr]     = useState('');

    // Настройки ключа в строке таблицы: раскрыт ли ServerPicker у ключа «все серверы»
    const [tools, setTools] = useState<number | null>(null);
    const [reveal, setReveal] = useState(false);

    // Подтверждение, когда ограничение скрывает живые мастер-ключи (lostMasters от сервера)
    const [lost, setLost] = useState<{ id: number; servers: number[]; prev: number[] | null; labels: string[] } | null>(null);

    const load = useCallback(async () => {
        try {
            const data = await apiFetch('GET', '/ui/apikeys', token);
            setKeys((data.keys ?? []).map(keyOf));
            apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
        } catch {
            showMsg('Ошибка загрузки ключей');
        }
    }, [token, showMsg]);

    useEffect(() => { if (token) load(); }, [token, load]);

    // Сервер, который предложим при ограничении: тот, что открыт в шапке (он же станет
    // сервером по умолчанию для /api/v1/users), иначе первый в списке.
    const startPicked = useCallback((): number[] => {
        const def = getServerId() ?? 0;
        if (nodes.some(n => n.id === def)) return [def];
        return nodes.length ? [nodes[0].id] : [];
    }, [nodes]);

    const openCreate = () => { setName(''); setRestrict(false); setPicked(startPicked()); setNewErr(''); setNewOpen(true); };

    const createKey = useCallback(async () => {
        if (!name.trim() || busy) return;
        if (restrict && !picked.length) { setNewErr(EMPTY_SERVERS); return; }
        setBusy(true); setNewErr('');
        try {
            // Нетронутый выбор — all: true (все серверы, включая добавленные позже).
            const body = restrict
                ? { name: name.trim(), servers: [...picked].sort((a, b) => a - b) }
                : { name: name.trim(), all: true };
            const r = await apiFetch('POST', '/ui/apikeys', token, body);
            setNewOpen(false);
            setCreated({ label: r?.name ?? r?.label ?? body.name, key: r?.key });
            await load();
        } catch (e) {
            setNewErr(errText(e));
        } finally {
            setBusy(false);
        }
    }, [name, restrict, picked, busy, token, load]);

    // PATCH набора серверов. Ограничение может скрыть мастер-ключи — сервер возвращает
    // lostMasters; в этом случае сначала спрашиваем, а не молча применяем.
    const patchServers = useCallback(async (k: ApiKey, next: number[] | null, confirmed = false) => {
        try {
            const r = await apiFetch('PATCH', `/ui/apikeys/${k.id}`, token, { servers: next });
            const hidden: { id: number; label: string }[] = Array.isArray(r?.lostMasters) ? r.lostMasters : [];
            await load();
            if (!confirmed && next && hidden.length) {
                setLost({
                    id: k.id, servers: next,
                    prev: k.allServers ? null : k.servers,
                    labels: hidden.map(h => h.label ?? `#${h.id}`),
                });
                return;
            }
            showMsg(next === null ? 'Ключ снова видит все серверы' : 'Серверы ключа обновлены');
        } catch (e) {
            showMsg(errText(e));
        }
    }, [token, load, showMsg]);

    // «Продолжить»: повторяем тот же набор — сервер применяет ограничение (первый PATCH мог
    // только спросить о потерянных мастер-ключах).
    const confirmLost = useCallback(async () => {
        const d = lost; setLost(null);
        if (!d) return;
        try {
            await apiFetch('PATCH', `/ui/apikeys/${d.id}`, token, { servers: d.servers });
            await load();
            showMsg('Серверы ключа обновлены');
        } catch (e) {
            showMsg(errText(e));
        }
    }, [lost, token, load, showMsg]);

    // «Отмена»: возвращаем прежний набор, чтобы ограничение не осталось применённым.
    const cancelRestrict = useCallback(async () => {
        const d = lost; setLost(null);
        if (!d) return;
        try {
            await apiFetch('PATCH', `/ui/apikeys/${d.id}`, token, { servers: d.prev });
            await load();
            showMsg('Доступ к серверам прежний');
        } catch (e) {
            showMsg(errText(e));
        }
    }, [lost, token, load, showMsg]);

    const deleteKey = useCallback(async (id: number, lbl: string) => {
        if (!confirm('Удалить ключ «' + lbl + '»? Клиенты с ним потеряют доступ.')) return;
        await apiFetch('DELETE', '/ui/apikeys/' + id, token);
        showMsg('Ключ удалён: ' + lbl);
        await load();
    }, [token, load, showMsg]);

    // Серверы ключа: нейтральный чип «все серверы» или чипы ограниченного набора,
    // у сервера по умолчанию для /api/v1/users — точка и подсказка.
    const serversCell = (k: ApiKey) => {
        if (k.allServers) {
            return <span className="chip chip--offline" title="Включая добавленные позже">все серверы</span>;
        }
        const list = k.servers ?? [];
        if (!list.length) return <span className="apikey-all-empty">набор пуст</span>;
        return (
            <span className="apikey-srv-list">
                {list.map(id => {
                    const def = id === k.defaultServerId;
                    return (
                        <span key={id} className="chip chip--offline apikey-srv"
                            title={def ? `${serverName(nodes, id)} — по умолчанию для /api/v1/users` : serverName(nodes, id)}>
                            {def && <span className="apikey-def-dot" aria-hidden="true" />}
                            {serverName(nodes, id)}
                            {def && <span className="apikey-sr">по умолчанию</span>}
                        </span>
                    );
                })}
            </span>
        );
    };

    // «Последний раз» — короткая относительная дата, полная — в подсказке.
    const lastUsedCell = (k: ApiKey) => {
        const a = shortAgo(k.lastUsed);
        return a ? <span title={a.full}>{a.text}</span> : <span className="apikey-never">никогда</span>;
    };

    // Настройки ключа: та же строка «Доступ», что и при создании. Ограниченный набор
    // показывается раскрытым, «все серверы» — свёрнутым до ссылки «изменить…».
    const toolbox = (k: ApiKey) => (
        <section className="apikey-tools">
            <h4 className="apikey-tools-title">Серверы ключа</h4>
            <AccessLine nodes={nodes} all={k.allServers} servers={k.allServers ? nodes.map(n => n.id) : (k.servers ?? [])}
                expanded={!k.allServers || reveal} onExpand={setReveal}
                onChange={(all, next) => patchServers(k, all ? null : [...next].sort((a, b) => a - b))} />
            <p className="apikey-tools-hint">{ACCESS_HINT}</p>
        </section>
    );

    return (
        <>
            <div className="toolbar apikey-toolbar">
                <span className="apikey-tools-hint">Ключи внешнего API /api/v1</span>
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={openCreate}>
                        <IcoPlus /> Создать ключ
                    </button>
                </div>
            </div>

            <div className="table-card">
                <table>
                    <thead>
                        <tr>
                            <th>#</th>
                            <th>Метка</th>
                            <th>Ключ</th>
                            <th>Серверы</th>
                            <th>Создан</th>
                            <th>Последний раз</th>
                            <th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {keys.length === 0 ? (
                            <tr><td colSpan={7} className="empty">Нет ключей</td></tr>
                        ) : keys.map((k, i) => (
                            <Fragment key={k.id}>
                                <tr>
                                    <td className="td-num">{i + 1}</td>
                                    <td>{k.name}</td>
                                    <td className="td-mono">{k.prefix}</td>
                                    <td>{serversCell(k)}</td>
                                    <td>{shortDate(k.createdAt)}</td>
                                    <td>{lastUsedCell(k)}</td>
                                    <td className="td-actions">
                                        <div className="actions">
                                            <button
                                                className="btn-icon"
                                                title="Серверы ключа"
                                                aria-label="Серверы ключа"
                                                aria-expanded={tools === k.id}
                                                onClick={() => { setTools(tools === k.id ? null : k.id); setReveal(false); }}
                                            ><IcoSettings /></button>
                                            <button
                                                className="btn-icon btn-icon--danger"
                                                title="Удалить ключ"
                                                aria-label="Удалить ключ"
                                                onClick={() => deleteKey(k.id, k.name)}
                                            ><IcoTrash /></button>
                                        </div>
                                    </td>
                                </tr>
                                {tools === k.id && <tr className="apikey-tr-tools"><td colSpan={7}>{toolbox(k)}</td></tr>}
                            </Fragment>
                        ))}
                    </tbody>
                </table>
            </div>

            {/* Мобильный вид: таблица скрыта (.table-card), ключи — карточками */}
            <div className="apikey-cards-list">
                <p className="apikey-count">{keys.length} ключей</p>
                {keys.length === 0 ? (
                    <p className="apikey-empty">Нет ключей</p>
                ) : keys.map((k, i) => (
                    <div className="apikey-card" key={k.id}>
                        <div className="apikey-card-header">
                            <div className="apikey-card-title">
                                <span className="apikey-card-num">#{i + 1}</span>
                                <span className="apikey-card-label">{k.name}</span>
                            </div>
                            <div className="apikey-card-btns">
                                <button
                                    className="btn btn--tonal btn--sq"
                                    aria-label="Серверы ключа"
                                    aria-expanded={tools === k.id}
                                    onClick={() => { setTools(tools === k.id ? null : k.id); setReveal(false); }}
                                ><IcoSettings /></button>
                                <button
                                    className="btn btn--danger btn--sq"
                                    aria-label="Удалить ключ"
                                    onClick={() => deleteKey(k.id, k.name)}
                                ><IcoTrash /></button>
                            </div>
                        </div>
                        <code className="apikey-card-prefix">{k.prefix}</code>
                        <div className="apikey-card-servers">
                            <span>Серверы:</span> {serversCell(k)}
                        </div>
                        {tools === k.id && toolbox(k)}
                        <div className="apikey-card-meta">
                            <span>Создан {shortDate(k.createdAt)}</span>
                            <span>{lastUsedCell(k)}</span>
                        </div>
                    </div>
                ))}
            </div>

            {/* Создание ключа: имя и доступ. Серверы по умолчанию — все, выбор не трогаем. */}
            {newOpen && (
                <div className="node-scrim" onClick={() => setNewOpen(false)}>
                    <div className="node-modal apikey-modal" role="dialog" aria-modal="true" aria-labelledby="apikey-new-title" onClick={e => e.stopPropagation()}>
                        <h3 className="node-modal-title" id="apikey-new-title">Новый API-ключ</h3>
                        <label className="apikey-field">
                            <span className="apikey-field-name">Метка</span>
                            <input className="field" autoFocus placeholder="напр. ci-bot" value={name}
                                onChange={e => { setName(e.target.value); setNewErr(''); }}
                                onKeyDown={e => e.key === 'Enter' && createKey()} />
                        </label>
                        <AccessLine nodes={nodes} all={!restrict} servers={restrict ? picked : nodes.map(n => n.id)}
                            expanded={restrict} onExpand={v => { setRestrict(v); if (v) setPicked(p => p.length ? p : startPicked()); }}
                            onChange={(all, next) => { setRestrict(!all); setPicked(all ? nodes.map(n => n.id) : next); }} />
                        <p className="apikey-tools-hint">{ACCESS_HINT}</p>
                        {restrict && !picked.length && <p className="apikey-err">{EMPTY_SERVERS}</p>}
                        {newErr && <p className="apikey-err">{newErr}</p>}
                        <div className="node-modal-actions">
                            <button className="btn btn--text" onClick={() => setNewOpen(false)}>Отмена</button>
                            <button className="btn btn--primary" disabled={busy || !name.trim() || (restrict && !picked.length)} onClick={createKey}>
                                {busy ? 'Создаём…' : 'Создать'}
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Ограничение скрывает мастер-ключи: спрашиваем до сохранения */}
            {lost && (
                <div className="node-scrim" onClick={cancelRestrict}>
                    <div className="node-modal apikey-modal" role="dialog" aria-modal="true" aria-labelledby="apikey-lost-title" onClick={e => e.stopPropagation()}>
                        <h3 className="node-modal-title" id="apikey-lost-title">Ключ потеряет мастер-ключи</h3>
                        <p className="node-modal-hint">
                            Этот ключ потеряет видимость мастер-ключей: {lost.labels.join(', ')}. Продолжить?
                        </p>
                        <p className="apikey-tools-hint">
                            Мастер-ключ виден, только если весь его набор серверов входит в доступ ключа.
                        </p>
                        <div className="node-modal-actions">
                            <button className="btn btn--text" onClick={cancelRestrict}>Отмена</button>
                            <button className="btn btn--primary" onClick={confirmLost}>Продолжить</button>
                        </div>
                    </div>
                </div>
            )}

            {/* Открытый ключ — один раз, как и раньше */}
            {created && (
                <div className="qr-backdrop" onClick={() => setCreated(null)}>
                    <div className="qr-card" onClick={e => e.stopPropagation()}>
                        <p className="qr-name">Ключ «{created.label}» создан</p>
                        <p className="qr-hint">
                            Скопируй ключ сейчас — он показывается один раз и больше не будет доступен.
                        </p>
                        <code className="apikey-value">{created.key}</code>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setCreated(null)}>Закрыть</button>
                            <button className="btn btn--primary" onClick={async () => {
                                await copyText(created.key);
                                showMsg('Ключ скопирован');
                            }}><IcoCopy /> Скопировать</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}
