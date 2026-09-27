// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { Fragment, useState, useEffect, useCallback } from 'react';
import QRCode from 'qrcode';
import { apiFetch, copyText, bytes, timeAgo, genLabel, type NodeInfo, type PageProps } from '../../lib/shared';
import { IcoPlus, IcoTrash, IcoCopy, IcoQR, IcoKey, IcoRefresh, IcoSettings } from '../../components/icons';
import './masterkeys.css';

// Вкладка «Мастер-ключи»: sen://-подписки для SenAWG. Мастер-ключ — это ссылка
// плюс лимит устройств; устройства регистрируются сами (приватный ключ остаётся
// на них) и здесь только отображаются и отзываются. Формат — docs/sen-link.md.
// deleting — ключ удалён и уже не работает, но эти серверы ещё не подтвердили, что сняли его пиров.
interface MasterKey { id: number; label: string; device_limit: number; devices: number; servers: number[]; created_at: number; deleting?: number[] }
interface Device {
    id: number; device_id: string; device_name: string; platform: string; version: string; created_at: number; last_seen: number | null;
    rekey_requested: boolean; online: boolean; lastHandshake: number; rx: number; tx: number;
    servers_total: number; servers_ok: number;   // на скольких серверах ключа уже стоит его текущий ключ
}

// Устройство ещё не на всех серверах ключа (нода не в сети при регистрации или смене ключа) —
// панель догонит, когда нода появится.
const syncLabel = (d: Device): string =>
    d.servers_ok < d.servers_total ? ` · на ${d.servers_ok} из ${d.servers_total} серверов` : '';

// Платформа и версия клиента: «macOS-0.6.5»; без версии — просто платформа.
const platformLabel = (d: Device): string =>
    d.platform ? (d.version ? `${d.platform}-${d.version}` : d.platform) : '—';

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

export default function MasterKeysPage({ token, showMsg }: PageProps) {
    const [keys, setKeys]       = useState<MasterKey[]>([]);
    const [enabled, setEnabled] = useState(true);
    const [tls, setTls]         = useState(false);
    const [nodes, setNodes]     = useState<NodeInfo[]>([]);   // серверы, которые можно включить в ключ
    const [label, setLabel]     = useState('');
    const [limit, setLimit]     = useState('3');
    const [devs, setDevs]       = useState<Record<number, Device[]>>({});
    const [folded, setFolded]   = useState<Set<number>>(new Set());   // свёрнутые узлы
    const [tools, setTools]     = useState<number | null>(null);        // узел с открытыми настройками
    const [limitEdit, setLimitEdit] = useState('');
    const [modal, setModal]     = useState<{ label: string; link: string; tls: boolean; dataUrl: string } | null>(null);

    const load = useCallback(async () => {
        try {
            const d = await apiFetch('GET', '/ui/masterkeys', token);
            const list: MasterKey[] = d.keys ?? [];
            setKeys(list); setEnabled(!!d.enabled); setTls(!!d.tls);
            apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
            const all = await Promise.all(list.filter(k => !k.deleting).map(k =>
                apiFetch('GET', `/ui/masterkeys/${k.id}/devices`, token).then(r => [k.id, r.devices ?? []] as const)));
            setDevs(Object.fromEntries(all));
        } catch { showMsg('Ошибка загрузки мастер-ключей'); }
    }, [token, showMsg]);

    // Действие над ключом/устройством: выполнить, показать сообщение, обновить данные.
    const act = useCallback(async (fn: () => Promise<unknown>, ok: string) => {
        try { await fn(); showMsg(ok); await load(); }
        catch (e) { showMsg(errText(e)); }
    }, [load, showMsg]);

    const create = useCallback(async () => {
        if (!label.trim()) return;
        await act(async () => {
            // Новый ключ отдаёт все серверы, которые уже подключались; набор потом правится в настройках ключа.
            const ready = nodes.filter(n => !n.pending).map(n => n.id);
            await apiFetch('POST', '/ui/masterkeys', token, {
                label: label.trim(), device_limit: Number(limit), ...(ready.length ? { servers: ready } : {}),
            });
            setLabel('');
        }, 'Мастер-ключ создан');
    }, [label, limit, nodes, token, act]);

    const serverNames = useCallback((ids: number[]) =>
        ids.map(id => nodes.find(n => n.id === id)?.name ?? `#${id}`).join(', '), [nodes]);

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
            const dataUrl = await QRCode.toDataURL(link, { width: 560, margin: 2, errorCorrectionLevel: 'H' });
            setModal({ label: k.label, link, tls: t, dataUrl });
        } catch (e) { showMsg(errText(e)); }
    }, [fetchLink, showMsg]);

    const copyLink = useCallback(async (k: MasterKey) => {
        try { await copyText((await fetchLink(k)).link); showMsg('Ссылка скопирована'); }
        catch (e) { showMsg(errText(e)); }
    }, [fetchLink, showMsg]);

    const fold = (id: number) => setFolded(prev => {
        const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n;
    });
    const openTools = (k: MasterKey) => { setLimitEdit(String(k.device_limit)); setTools(tools === k.id ? null : k.id); };

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

    const actions = (k: MasterKey) => (
        <div className="actions">
            <button className="btn-icon" title="QR-код" aria-label="QR-код" onClick={() => showQR(k)}><IcoQR /></button>
            <button className="btn-icon" title="Скопировать ссылку" aria-label="Скопировать ссылку" onClick={() => copyLink(k)}><IcoCopy /></button>
            <button className="btn-icon" title="Настройки мастер-ключа" aria-label="Настройки мастер-ключа" aria-expanded={tools === k.id} onClick={() => openTools(k)}><IcoSettings /></button>
            <button className="btn-icon btn-icon--danger" title="Удалить мастер-ключ" aria-label="Удалить мастер-ключ" onClick={() => remove(k)}><IcoTrash /></button>
        </div>
    );

    const toolbox = (k: MasterKey) => (
        <div className="mk-tools">
            {/* Слева — настройки самого ключа */}
            <section className="mk-box">
                <h4 className="mk-box-title">Настройки ключа</h4>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <label className="mk-setting-name" htmlFor={`mk-limit-${k.id}`}>Лимит устройств</label>
                        <span className="mk-setting-hint">Сколько устройств может подключить ключ</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <input id={`mk-limit-${k.id}`} className="field mk-limit-input" type="number" min={1} max={100}
                            value={limitEdit} onChange={e => setLimitEdit(e.target.value)} />
                        <button className="btn btn--tonal" onClick={() =>
                            act(() => apiFetch('PATCH', `/ui/masterkeys/${k.id}`, token, { device_limit: Number(limitEdit) }), 'Лимит сохранён')}>
                            Сохранить
                        </button>
                    </div>
                </div>
                <div className="mk-setting">
                    <div className="mk-setting-text">
                        <span className="mk-setting-name">Ссылка</span>
                        <span className="mk-setting-hint">Старая перестанет принимать новые устройства</span>
                    </div>
                    <div className="mk-setting-ctl">
                        <button className="btn btn--tonal" onClick={() => {
                            if (confirm('Выпустить новую ссылку? Старая перестанет принимать новые устройства, уже добавленные продолжат работать.'))
                                act(() => apiFetch('POST', `/ui/masterkeys/${k.id}/rotate`, token), 'Ссылка перевыпущена');
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
                            act(() => apiFetch('POST', `/ui/masterkeys/${k.id}/rekey`, token), 'Смена ключей запрошена')}>
                            Запросить смену
                        </button>
                    </div>
                </div>
            </section>

            {/* Справа — какие серверы отдаёт ключ: в config.servers[] каждого устройства. Снять галочку —
                пиры его устройств с этого сервера удалятся; поставить — добавятся. Последний не снимается. */}
            <section className="mk-box">
                <h4 className="mk-box-title">Серверы ключа</h4>
                {nodes.map(n => {
                    const on = k.servers.includes(n.id);
                    const last = on && k.servers.length === 1;
                    const h = n.health;
                    return (
                        <label className={`mk-server${last ? ' mk-server--locked' : ''}`} key={n.id}>
                            <input type="checkbox" checked={on} disabled={last}
                                onChange={() => {
                                    const next = on ? k.servers.filter(id => id !== n.id) : [...k.servers, n.id];
                                    act(() => apiFetch('PATCH', `/ui/masterkeys/${k.id}`, token, { servers: next }),
                                        on ? `«${h?.server || n.name}» убран из ключа` : `«${h?.server || n.name}» добавлен в ключ`);
                                }} />
                            <span className={`chip chip--${n.online ? 'online' : 'offline'} mk-server-dot`} title={n.online ? 'В сети' : 'Нет связи'}>
                                <span className="chip-dot" />
                            </span>
                            <span className="mk-server-name">{h?.server || n.name}</span>
                            {h?.ip && <code className="mk-server-ip">{h.ip}</code>}
                            {h?.gen && <span className="mk-server-gen">{genLabel(h.gen)}</span>}
                        </label>
                    );
                })}
                <p className="mk-setting-hint">Новые серверы добавляются кнопкой «Добавить сервер» над вкладкой.</p>
            </section>
        </div>
    );

    // Полоска занятых слотов: числа по краям внутри; цвет по заполнению.
    const meter = (k: MasterKey, n: number) => (
        <div className={`mk-meter${n >= k.device_limit ? ' mk-meter--full' : n * 3 >= k.device_limit * 2 ? ' mk-meter--warn' : ''}`}
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

    const deviceActions = (d: Device) => (
        <div className="actions">
            <button className="btn-icon" title="Новый PSK" aria-label="Новый PSK" onClick={async () => {
                try {
                    const r = await apiFetch('POST', `/ui/devices/${d.id}/psk`, token);
                    showMsg(r.pending ? `PSK обновлён; ещё на ${r.pending} — когда сервер будет на связи` : 'PSK обновлён');
                    await load();
                } catch (e) { showMsg(errText(e)); }
            }}><IcoRefresh /></button>
            <button className="btn-icon btn-icon--danger" title="Отозвать устройство" aria-label="Отозвать устройство" onClick={() => {
                if (confirm('Отозвать устройство «' + (d.device_name || d.id) + '»?'))
                    act(() => apiFetch('DELETE', `/ui/devices/${d.id}`, token), 'Устройство отозвано');
            }}><IcoTrash /></button>
        </div>
    );

    const caret = (k: MasterKey) => (
        <button className={`mk-caret${folded.has(k.id) ? ' mk-caret--folded' : ''}`}
            aria-label={folded.has(k.id) ? 'Развернуть' : 'Свернуть'} aria-expanded={!folded.has(k.id)}
            onClick={() => fold(k.id)}>▾</button>
    );

    const deviceNode = (k: MasterKey, d: Device) => (
        <li className="mk-node mk-node--device" key={d.id}>
            <div className="mk-device">
                <div className="mk-device-main">
                    <span className="mk-device-title">
                        <span className="mk-device-name">{d.device_name || 'Устройство'}</span>
                        <code className="mk-device-id" title={`ID устройства: ${d.device_id}`}>{d.device_id}</code>
                    </span>
                    <span className="mk-device-sub">{platformLabel(d)} · был {d.last_seen ? timeAgo(d.last_seen) + ' назад' : 'никогда'}
                        {d.rekey_requested ? ' · ждёт смены ключа' : ''}{syncLabel(d)}</span>
                </div>
                <span className={`chip chip--${d.online ? 'online' : 'offline'}`}>
                    <span className="chip-dot" />{d.online ? 'Онлайн' : 'Офлайн'}
                </span>
                <div className="mk-device-traffic">
                    <span className="mk-metric"><i className="mk-metric-name" data-sfx=" rx">↓</i><b>{bytes(d.rx)}</b></span>
                    <span className="mk-metric"><i className="mk-metric-name" data-sfx=" tx">↑</i><b>{bytes(d.tx)}</b></span>
                </div>
                {deviceActions(d)}
            </div>
        </li>
    );

    return (
        <>
            {!enabled && (
                <p className="mk-notice">Подписка не настроена на сервере: нет ключа подписи или SUB_PORT. Ссылки выдавать нельзя.</p>
            )}
            {enabled && !tls && (
                <p className="mk-notice">Подписка работает без TLS: ответы подписаны, но secret ссылки и конфиг видны в сети.</p>
            )}

            <div className="toolbar">
                <input className="field" placeholder="Метка (напр. Семья)" value={label}
                    onChange={e => setLabel(e.target.value)} onKeyDown={e => e.key === 'Enter' && create()} />
                <input className="field mk-limit-input" type="number" min={1} max={100} title="Лимит устройств"
                    aria-label="Лимит устройств" value={limit} onChange={e => setLimit(e.target.value)} />
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={create}><IcoPlus /> Создать ключ</button>
                </div>
            </div>

            {/* Десктоп: таблица, иерархия — строка мастер-ключа и устройства под ней */}
            <div className="table-card mk-table-card">
                <table className="mk-table">
                    <thead>
                        <tr>
                            <th>Имя</th><th>ID устройства</th><th>Платформа</th><th>Статус</th>
                            <th>↓ rx</th><th>↑ tx</th><th>Был</th><th>Устройства</th><th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {keys.length === 0 ? (
                            <tr><td colSpan={9} className="empty">Нет мастер-ключей</td></tr>
                        ) : keys.map(k => {
                            if (k.deleting) return (
                                <tr key={k.id} className="mk-tr-master mk-last">
                                    <td><span className="mk-name"><IcoKey />{k.label}</span></td>
                                    <td colSpan={8}>{deletingChip(k)}</td>
                                </tr>
                            );
                            const list = devs[k.id] ?? [];
                            const free = Math.max(0, k.device_limit - list.length);
                            const open = !folded.has(k.id);
                            return (
                                <Fragment key={k.id}>
                                    <tr className="mk-tr-master">
                                        <td><span className="mk-name">{caret(k)}<IcoKey />{k.label}</span></td>
                                        <td colSpan={6}></td>
                                        <td>{meter(k, list.length)}</td>
                                        <td className="td-actions mk-td-actions">{actions(k)}</td>
                                    </tr>
                                    {tools === k.id && <tr className="mk-tr-tools"><td colSpan={9}>{toolbox(k)}</td></tr>}
                                    {open && list.map((d, i) => (
                                        <tr key={d.id} className={`mk-tr-device${i === list.length - 1 && free === 0 ? ' mk-last' : ''}`}>
                                            <td><span className="mk-indent">{d.device_name || 'Устройство'}</span></td>
                                            <td className="td-mono"><code className="mk-device-id" title={d.device_id}>{d.device_id}</code></td>
                                            <td>{platformLabel(d)}</td>
                                            <td>
                                                <span className={`chip chip--${d.online ? 'online' : 'offline'}`}>
                                                    <span className="chip-dot" />{d.online ? 'Онлайн' : 'Офлайн'}
                                                </span>
                                            </td>
                                            <td className="td-mono">{bytes(d.rx)}</td>
                                            <td className="td-mono">{bytes(d.tx)}</td>
                                            <td>{d.last_seen ? timeAgo(d.last_seen) + ' назад' : 'никогда'}
                                                {d.rekey_requested && <span className="mk-flag"> · ждёт смены ключа</span>}
                                                {syncLabel(d) && <span className="mk-flag">{syncLabel(d)}</span>}</td>
                                            <td></td>
                                            <td className="td-actions mk-td-actions">{deviceActions(d)}</td>
                                        </tr>
                                    ))}
                                    {open && free > 0 && (
                                        <tr className="mk-tr-free mk-last">
                                            <td colSpan={9}><span className="mk-indent">Свободно: {free}</span></td>
                                        </tr>
                                    )}
                                </Fragment>
                            );
                        })}
                    </tbody>
                </table>
            </div>

            {/* Мобильный: таблица скрыта (.table-card), то же — деревом карточек */}
            {keys.length === 0 ? <p className="mk-empty mk-tree">Нет мастер-ключей</p> : (
                <ul className="mk-tree">
                    {keys.map(k => {
                        if (k.deleting) return (
                            <li className="mk-root" key={k.id}>
                                <div className="mk-master">
                                    <IcoKey />
                                    <span className="mk-master-label">{k.label}</span>
                                    {deletingChip(k)}
                                </div>
                            </li>
                        );
                        const list = devs[k.id] ?? [];
                        const free = Math.max(0, k.device_limit - list.length);
                        const isFolded = folded.has(k.id);
                        return (
                            <li className="mk-root" key={k.id}>
                                <div className="mk-master">
                                    {caret(k)}
                                    <IcoKey />
                                    <span className="mk-master-label">{k.label}</span>
                                    {meter(k, list.length)}
                                    {actions(k)}
                                </div>
                                {tools === k.id && toolbox(k)}
                                {!isFolded && (
                                    <ul className="mk-children">
                                        {list.map(d => deviceNode(k, d))}
                                        {free > 0 && (
                                            <li className="mk-node mk-node--free"><div className="mk-free">Свободно: {free}</div></li>
                                        )}
                                    </ul>
                                )}
                            </li>
                        );
                    })}
                </ul>
            )}

            {modal && (
                <div className="qr-backdrop" onClick={() => setModal(null)}>
                    <div className="qr-card" onClick={e => e.stopPropagation()}>
                        <p className="qr-name">{modal.label}</p>
                        <img className="qr-img" src={modal.dataUrl} alt="QR" title="Нажми чтобы скопировать sen:// ссылку"
                            onClick={async () => { await copyText(modal.link); showMsg('Ссылка скопирована'); }} />
                        <p className="qr-hint">Отсканируй в SenAWG · нажми на QR чтобы скопировать{modal.tls ? '' : ' · без TLS'}</p>
                        <div className="qr-actions">
                            <button className="btn btn--primary" onClick={async () => { await copyText(modal.link); showMsg('Ссылка скопирована'); }}><IcoCopy /> Скопировать</button>
                            <button className="btn btn--tonal" onClick={() => setModal(null)}>Закрыть</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}
