import { useState, useEffect, useRef, useCallback } from 'react';
import QRCode from 'qrcode';
import {
    apiFetch, vpnKeyToConf, downloadFile, copyText, bytes, timeAgo,
    type User, type PageProps,
} from '../../lib/shared';
import { IcoPlus, IcoRefresh, IcoQR, IcoTrash, IcoGlobe } from '../../components/icons';
import './users.css';

// Вкладка «Пользователи»: форма создания + таблица (десктоп) / карточки (мобильный)
// + модалка QR. Сервер-бар сверху рендерит App. Свой стейт и поллинг — здесь.
export default function UsersPage({ token, showMsg }: PageProps) {
    const [users, setUsers]     = useState<User[]>([]);
    const [newName, setNewName] = useState('');
    const [qrModal, setQrModal] = useState<{ name: string; dataUrl: string; vpnKey: string } | null>(null);
    const statsRef              = useRef<ReturnType<typeof setInterval> | null>(null);

    const loadStats = useCallback(async (tok: string) => {
        try {
            const data = await apiFetch('GET', '/api/users/stats', tok);
            setUsers(prev => prev.map(u => {
                const s = (data.users as User[] ?? []).find(x => x.name === u.name);
                return s ? { ...u, online: s.online, lastHandshake: s.lastHandshake, rx: s.rx, tx: s.tx } : u;
            }));
        } catch { /* silent */ }
    }, []);

    const loadUsers = useCallback(async (tok: string) => {
        try {
            const data = await apiFetch('GET', '/api/users', tok);
            setUsers(data.users ?? []);
            await loadStats(tok);
        } catch {
            showMsg('Ошибка загрузки');
        }
    }, [loadStats, showMsg]);

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
            const dataUrl = await QRCode.toDataURL(vpnKey, {
                width: 560,
                margin: 2,
                errorCorrectionLevel: 'L',
            });
            setQrModal({ name, dataUrl, vpnKey });
        } catch {
            showMsg('Не удалось сгенерировать QR-код');
        }
    }, [token, showMsg]);

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
                    onChange={e => setNewName(e.target.value)}
                    onKeyDown={e => e.key === 'Enter' && createUser()}
                />
                <div className="toolbar-btns">
                    <button className="btn btn--primary" onClick={createUser}>
                        <IcoPlus /> Создать
                    </button>
                    <button className="btn btn--tonal" onClick={() => loadUsers(token)}>
                        <IcoRefresh /> Обновить
                    </button>
                </div>
            </div>

            <div className="table-card">
                <table>
                    <thead>
                        <tr>
                            <th>#</th>
                            <th>Статус</th>
                            <th>Имя</th>
                            <th>IP</th>
                            <th>↓ rx</th>
                            <th>↑ tx</th>
                            <th></th>
                        </tr>
                    </thead>
                    <tbody>
                        {users.length === 0 ? (
                            <tr><td colSpan={7} className="empty">Нет пользователей</td></tr>
                        ) : users.map((u, i) => (
                            <tr key={u.name}>
                                <td className="td-num">{i + 1}</td>
                                <td>
                                    <span className={`chip chip--${u.online ? 'online' : 'offline'}`}>
                                        <span className="chip-dot" />
                                        {u.online
                                            ? 'онлайн'
                                            : u.lastHandshake
                                                ? timeAgo(u.lastHandshake) + ' назад'
                                                : 'никогда'}
                                    </span>
                                </td>
                                <td>{u.name}</td>
                                <td className="td-mono">{u.ip}</td>
                                <td>{bytes(u.rx)}</td>
                                <td>{bytes(u.tx)}</td>
                                <td className="td-actions">
                                    <div className="actions">
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
                            </div>
                            <span className={`chip chip--${u.online ? 'online' : 'offline'}`}>
                                <span className="chip-dot" />
                                {u.online
                                    ? 'онлайн'
                                    : u.lastHandshake
                                        ? timeAgo(u.lastHandshake) + ' назад'
                                        : 'никогда'}
                            </span>
                        </div>
                        <div className="user-card-ip">
                            <IcoGlobe />
                            <span className="user-card-ip-text">{u.ip}</span>
                        </div>
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
                                className="btn btn--danger btn--sq"
                                aria-label="Удалить"
                                onClick={() => deleteUser(u.name)}
                            ><IcoTrash /></button>
                        </div>
                    </div>
                ))}
            </div>

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
                            <button className="btn btn--tonal" onClick={async () => {
                                try {
                                    const conf = await vpnKeyToConf(qrModal.vpnKey);
                                    const safe = qrModal.name.replace(/[\/\\:*?"<>|]+/g, '_').trim() || 'awg';
                                    downloadFile(safe + '.conf', conf);
                                    showMsg('Конфиг скачан');
                                } catch { showMsg('Не удалось декодировать ключ'); }
                            }}>Скачать .conf</button>
                            <button className="btn btn--tonal" onClick={() => setQrModal(null)}>Закрыть</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}