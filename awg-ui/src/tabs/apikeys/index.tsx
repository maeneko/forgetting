import { useState, useEffect, useCallback } from 'react';
import { apiFetch, copyText, timeAgo, type ApiKey, type PageProps } from '../../lib/shared';
import { IcoPlus, IcoTrash, IcoCopy } from '../../components/icons';
import './apikeys.css';

// Вкладка «API-ключи»: ключи внешнего API (/api/v1). Открытый ключ показывается
// один раз при создании (в БД хранится только хэш). Сервер-бар сверху рендерит App.
//   TODO multi-server: сейчас сервер один (server_id = 0). Когда появится список
//   серверов — ключ будет привязываться к выбранному в сервер-баре.
export default function ApiKeysPage({ token, showMsg }: PageProps) {
    const [keys, setKeys]       = useState<ApiKey[]>([]);
    const [label, setLabel]     = useState('');
    const [created, setCreated] = useState<{ label: string; key: string } | null>(null);

    const load = useCallback(async () => {
        try {
            const data = await apiFetch('GET', '/ui/apikeys', token);
            setKeys(data.keys ?? []);
        } catch {
            showMsg('Ошибка загрузки ключей');
        }
    }, [token, showMsg]);

    const createKey = useCallback(async () => {
        if (!label.trim()) return;
        const r = await apiFetch('POST', '/ui/apikeys', token, { label: label.trim(), server_id: 0 });
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
            {/* Сервер-бар сверху рендерит App; к выбранному в нём серверу и привязывается ключ.
                TODO multi-server: server_id берётся из выбранной карточки (пока 0). */}
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
                            <th>Создан</th>
                            <th>Последний раз</th>
                            <th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {keys.length === 0 ? (
                            <tr><td colSpan={6} className="empty">Нет ключей</td></tr>
                        ) : keys.map((k, i) => (
                            <tr key={k.id}>
                                <td className="td-num">{i + 1}</td>
                                <td>{k.label}</td>
                                <td className="td-mono">{k.prefix}</td>
                                <td>{new Date(k.created_at * 1000).toLocaleDateString()}</td>
                                <td>{k.last_used ? timeAgo(k.last_used) + ' назад' : 'никогда'}</td>
                                <td className="td-actions">
                                    <div className="actions">
                                        <button
                                            className="btn-icon btn-icon--danger"
                                            title="Удалить ключ"
                                            onClick={() => deleteKey(k.id, k.label)}
                                        ><IcoTrash /></button>
                                    </div>
                                </td>
                            </tr>
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
                            <button
                                className="btn btn--danger btn--sq"
                                aria-label="Удалить ключ"
                                onClick={() => deleteKey(k.id, k.label)}
                            ><IcoTrash /></button>
                        </div>
                        <code className="apikey-card-prefix">{k.prefix}</code>
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