// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { Fragment, useState, useEffect, useCallback } from 'react';
import { apiFetch, copyText, timeAgo, getServerId, type ApiKey, type NodeInfo, type PageProps } from '../../lib/shared';
import { IcoPlus, IcoTrash, IcoCopy, IcoSettings } from '../../components/icons';
import ServerPicker, { serverName } from '../../components/ServerPicker';
import './apikeys.css';

// Вкладка «API-ключи»: ключи внешнего API (/api/v1). Открытый ключ показывается
// один раз при создании (в БД хранится только хэш). Сервер-бар сверху рендерит App.
// Ключ видит набор серверов: на них он заводит vpn://-пользователей и в их пределах
// управляет мастер-ключами. Создаётся на выбранном в сервер-баре сервере, набор
// правится в настройках ключа.
export default function ApiKeysPage({ token, showMsg }: PageProps) {
    const [keys, setKeys]       = useState<ApiKey[]>([]);
    const [nodes, setNodes]     = useState<NodeInfo[]>([]);
    const [label, setLabel]     = useState('');
    const [created, setCreated] = useState<{ label: string; key: string } | null>(null);
    const [tools, setTools]     = useState<number | null>(null);   // ключ с открытыми настройками

    const load = useCallback(async () => {
        try {
            const data = await apiFetch('GET', '/ui/apikeys', token);
            setKeys(data.keys ?? []);
            apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
        } catch {
            showMsg('Ошибка загрузки ключей');
        }
    }, [token, showMsg]);

    const setServers = useCallback(async (k: ApiKey, next: number[], n: NodeInfo, added: boolean) => {
        try {
            await apiFetch('PATCH', `/ui/apikeys/${k.id}`, token, { servers: next });
            showMsg(added ? `Ключ видит «${serverName(nodes, n.id)}»` : `«${serverName(nodes, n.id)}» убран из ключа`);
            await load();
        } catch (e: any) {
            showMsg(e?.response?.data?.error ?? 'Ошибка запроса');
        }
    }, [token, nodes, load, showMsg]);

    const serversText = (k: ApiKey) => (k.servers ?? [k.server_id]).map(id => serverName(nodes, id)).join(', ');

    const toolbox = (k: ApiKey) => (
        <section className="apikey-tools">
            <h4 className="apikey-tools-title">Серверы ключа</h4>
            <p className="apikey-tools-hint">
                На них ключ заводит vpn://-пользователей (по умолчанию — на «{serverName(nodes, k.server_id)}», другой
                выбирается заголовком X-Server-Id) и управляет мастер-ключами, чьи серверы все отмечены здесь.
            </p>
            <ServerPicker nodes={nodes} selected={k.servers ?? [k.server_id]} onChange={(next, n, added) => setServers(k, next, n, added)} />
        </section>
    );

    const createKey = useCallback(async () => {
        if (!label.trim()) return;
        const r = await apiFetch('POST', '/ui/apikeys', token, { label: label.trim(), server_id: getServerId() ?? 0 });
        if (r.error) { showMsg(r.error); return; }
        setLabel('');
        setCreated({ label: r.label, key: r.key });
        await load();
    }, [label, token, load, showMsg]);

    const deleteKey = useCallback(async (id: number, lbl: string) => {
        if (!confirm('Удалить ключ «' + lbl + '»? Клиенты с ним потеряют доступ.')) return;
        await apiFetch('DELETE', '/ui/apikeys/' + id, token);
        showMsg('Ключ удалён: ' + lbl);
        await load();
    }, [token, load, showMsg]);

    useEffect(() => { if (token) load(); }, [token, load]);

    return (
        <>
            {/* Сервер-бар сверху рендерит App; новый ключ получает выбранный в нём сервер. */}
            <div className="toolbar">
                <input
                    className="field"
                    placeholder="Метка ключа (напр. ci-bot)"
                    value={label}
                    onChange={e => setLabel(e.target.value)}
                    onKeyDown={e => e.key === 'Enter' && createKey()}
                />
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={createKey}>
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
                                    <td>{k.label}</td>
                                    <td className="td-mono">{k.prefix}</td>
                                    <td>{serversText(k)}</td>
                                    <td>{new Date(k.created_at * 1000).toLocaleDateString()}</td>
                                    <td>{k.last_used ? timeAgo(k.last_used) + ' назад' : 'никогда'}</td>
                                    <td className="td-actions">
                                        <div className="actions">
                                            <button
                                                className="btn-icon"
                                                title="Серверы ключа"
                                                aria-label="Серверы ключа"
                                                aria-expanded={tools === k.id}
                                                onClick={() => setTools(tools === k.id ? null : k.id)}
                                            ><IcoSettings /></button>
                                            <button
                                                className="btn-icon btn-icon--danger"
                                                title="Удалить ключ"
                                                aria-label="Удалить ключ"
                                                onClick={() => deleteKey(k.id, k.label)}
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
                                <span className="apikey-card-label">{k.label}</span>
                            </div>
                            <div className="apikey-card-btns">
                                <button
                                    className="btn btn--tonal btn--sq"
                                    aria-label="Серверы ключа"
                                    aria-expanded={tools === k.id}
                                    onClick={() => setTools(tools === k.id ? null : k.id)}
                                ><IcoSettings /></button>
                                <button
                                    className="btn btn--danger btn--sq"
                                    aria-label="Удалить ключ"
                                    onClick={() => deleteKey(k.id, k.label)}
                                ><IcoTrash /></button>
                            </div>
                        </div>
                        <code className="apikey-card-prefix">{k.prefix}</code>
                        <span className="apikey-card-servers">Серверы: {serversText(k)}</span>
                        {tools === k.id && toolbox(k)}
                        <div className="apikey-card-meta">
                            <span>Создан {new Date(k.created_at * 1000).toLocaleDateString()}</span>
                            <span>{k.last_used ? timeAgo(k.last_used) + ' назад' : 'не использован'}</span>
                        </div>
                    </div>
                ))}
            </div>

            {created && (
                <div className="qr-backdrop" onClick={() => setCreated(null)}>
                    <div className="qr-card" onClick={e => e.stopPropagation()}>
                        <p className="qr-name">Ключ «{created.label}» создан</p>
                        <p className="qr-hint">
                            Скопируй ключ сейчас — он показывается один раз и больше не будет доступен.
                        </p>
                        <code className="apikey-value">{created.key}</code>
                        <div className="qr-actions">
                            <button className="btn btn--primary" onClick={async () => {
                                await copyText(created.key);
                                showMsg('Ключ скопирован');
                            }}><IcoCopy /> Скопировать</button>
                            <button className="btn btn--tonal" onClick={() => setCreated(null)}>Закрыть</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}