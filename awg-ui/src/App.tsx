// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useRef, useCallback } from 'react';
import axios from 'axios';
import './App.css';
import {
    TOKEN_KEY, THEME_KEY, SERVER_KEY, BRAND_FALLBACK, apiFetch, setServerId, getServerId,
    type NodeInfo, type Brand,
} from './lib/shared';
import { IcoMenu, IcoLogout, IcoSun, IcoMoon, IcoRefresh } from './components/icons';
import ServerBar from './components/ServerBar';
import AddServerModal from './components/AddServerModal';
import { TABS } from './tabs';

// Оболочка приложения: логин, тема, drawer/сайдбар, общая сессия (token,
// список серверов и выбранный сервер, снэкбар) и переключение вкладок. Контент каждой вкладки приходит
// из реестра TABS — App про конкретные вкладки ничего не знает.
export default function App() {
    const [token, setToken]           = useState(() => localStorage.getItem(TOKEN_KEY) ?? '');
    const [loginUser, setLoginUser]   = useState('');
    const [loginPass, setLoginPass]   = useState('');
    const [statusText, setStatusText] = useState('');
    const [nodes, setNodes]           = useState<NodeInfo[]>([]);
    const [hubEnabled, setHubEnabled] = useState(false);
    const [serverId, setServerIdState] = useState<number | null>(null);
    const [modal, setModal]           = useState<{ node?: NodeInfo } | null>(null);
    const [msg, setMsg]               = useState('');
    const [drawerOpen, setDrawerOpen] = useState(false);
    const [activeTab, setActiveTab]   = useState(TABS[0].id);
    const [restarting, setRestarting] = useState(false);
    const [brand, setBrand]           = useState<Brand>({ brand: BRAND_FALLBACK, channel: '', version: '' });
    const [theme, setTheme]           = useState<'light' | 'dark'>(() =>
        localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light');
    const touchStartX                 = useRef(0);
    const touchCurrentX               = useRef(0);

    const showMsg = useCallback((text: string) => {
        setMsg(text);
        setTimeout(() => setMsg(''), 3000);
    }, []);

    // Выбрать сервер: id уходит заголовком X-Server-Id во все запросы вкладок
    // (setServerId — до смены state, чтобы вкладка с новым key читала уже с ним).
    const pickServer = useCallback((id: number | null) => {
        setServerId(id);
        setServerIdState(id);
        try { if (id !== null) localStorage.setItem(SERVER_KEY, String(id)); } catch { /* не критично */ }
    }, []);

    // Список серверов (core + ноды) с их состоянием. Если выбранного больше нет —
    // берём сохранённый, иначе первый.
    const loadNodes = useCallback(async (tok: string) => {
        const { data } = await axios.get('/ui/nodes', { headers: { Authorization: `Bearer ${tok}` } });
        const list: NodeInfo[] = data.nodes ?? [];
        setNodes(list);
        setHubEnabled(!!data.hub);
        const cur = getServerId();
        if (cur !== null && list.some(n => n.id === cur)) return;
        let saved: number | null = null;
        try { const raw = localStorage.getItem(SERVER_KEY); saved = raw === null ? null : Number(raw); } catch { /* не критично */ }
        pickServer(list.find(n => n.id === saved)?.id ?? list[0]?.id ?? null);
    }, [pickServer]);

    // Проверяет токен и подтягивает список серверов. Данные конкретных вкладок
    // (юзеры, ключи) грузят сами вкладки.
    const startSession = useCallback(async (tok: string) => {
        await loadNodes(tok);
    }, [loadNodes]);

    const login = useCallback(async () => {
        try {
            const { data: auth } = await axios.post('/login', { user: loginUser, pass: loginPass });
            const tok = auth.token as string;
            await startSession(tok);
            localStorage.setItem(TOKEN_KEY, tok);
            setToken(tok);
        } catch (e) {
            if (axios.isAxiosError(e) && e.response?.status === 401) {
                showMsg('Неверный логин или пароль');
            } else if (axios.isAxiosError(e) && e.response?.status === 429) {
                showMsg(e.response.data?.error ?? 'Слишком много попыток');
            } else {
                setStatusText('offline:Недоступен');
            }
        }
    }, [loginUser, loginPass, startSession, showMsg]);

    const logout = useCallback(async () => {
        try {
            await axios.post('/logout', null, { headers: { Authorization: `Bearer ${token}` } });
        } catch { /* токен уже мог истечь */ }
        localStorage.removeItem(TOKEN_KEY);
        setToken('');
        setStatusText('');
        setNodes([]);
        setServerId(null);
        setServerIdState(null);
        setDrawerOpen(false);
    }, [token]);

    const deleteNode = useCallback(async (n: NodeInfo) => {
        if (!confirm(`Удалить сервер «${n.name}»? Нода отключится от панели.`)) return;
        try {
            await apiFetch('DELETE', `/ui/nodes/${n.id}`, token);
            showMsg('Сервер удалён');
            await loadNodes(token);
        } catch (e) {
            showMsg(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось удалить' : 'Не удалось удалить');
        }
    }, [token, showMsg, loadNodes]);

    // Перезапуск AWG-интерфейса (awg-quick down/up + ресинк пиров в awg-ctrl).
    // Соединения клиентов кратковременно прерываются — поэтому подтверждение.
    const restartAwg = useCallback(async () => {
        if (restarting) return;
        if (!confirm('Перезапустить AWG? Соединения клиентов кратковременно прервутся.')) return;
        setRestarting(true);
        try {
            await apiFetch('POST', '/awg/restart', token);
            showMsg('AWG перезапущен');
        } catch {
            showMsg('Не удалось перезапустить AWG');
        } finally {
            setRestarting(false);
        }
    }, [restarting, token, showMsg]);

    const handleTouchStart = useCallback((e: React.TouchEvent) => {
        touchStartX.current   = e.touches[0].clientX;
        touchCurrentX.current = e.touches[0].clientX;
    }, []);
    const handleTouchMove = useCallback((e: React.TouchEvent) => {
        touchCurrentX.current = e.touches[0].clientX;
    }, []);
    const handleTouchEnd = useCallback(() => {
        if (touchStartX.current - touchCurrentX.current > 40) setDrawerOpen(false);
    }, []);

    // Восстановить сессию из сохранённого токена
    useEffect(() => {
        const saved = localStorage.getItem(TOKEN_KEY);
        if (saved) {
            startSession(saved).catch(() => {
                localStorage.removeItem(TOKEN_KEY);
                setToken('');
            });
        }
    }, [startSession]);

    // Состояние серверов (онлайн, пиры) обновляем раз в 15 с, пока открыта панель.
    useEffect(() => {
        if (!token) return;
        const t = setInterval(() => { loadNodes(token).catch(() => { /* следующий тик */ }); }, 15_000);
        return () => clearInterval(t);
    }, [token, loadNodes]);

    // Имя продукта и версия — с сервера, до логина (роут без авторизации).
    // Если не ответил, остаётся фолбэк из констант.
    useEffect(() => {
        axios.get('/ui/brand')
            .then(({ data }) => setBrand({
                brand:   data.brand || BRAND_FALLBACK,
                channel: data.channel || '',
                version: data.version || '',
            }))
            .catch(() => { /* остаётся фолбэк */ });
    }, []);

    // Применять и сохранять тему
    useEffect(() => {
        document.documentElement.setAttribute('data-theme', theme);
        localStorage.setItem(THEME_KEY, theme);
    }, [theme]);

    // Блокировать прокрутку фона когда drawer открыт
    useEffect(() => {
        document.body.style.overflow = drawerOpen ? 'hidden' : '';
        return () => { document.body.style.overflow = ''; };
    }, [drawerOpen]);

    // Закрывать drawer по Escape
    useEffect(() => {
        if (!drawerOpen) return;
        const h = (e: KeyboardEvent) => { if (e.key === 'Escape') setDrawerOpen(false); };
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [drawerOpen]);

        const statusLabel = statusText.replace(/^(online|offline):/, '');
    const tab = TABS.find(t => t.id === activeTab) ?? TABS[0];
    // Хром вкладки (сервер-бар + перезапуск). Любой флаг по умолчанию включён.
    const chrome = tab.chrome ?? {};
    const showServerBar = chrome.serverBar !== false;
    const showRestart   = chrome.restart   !== false;

    return (
        <div className="app">
            <button
                className="theme-toggle"
                aria-label={theme === 'dark' ? 'Светлая тема' : 'Тёмная тема'}
                onClick={() => setTheme(t => (t === 'dark' ? 'light' : 'dark'))}
            >
                {theme === 'dark' ? <IcoSun /> : <IcoMoon />}
            </button>

            {token && showRestart && serverId !== null && (
                <button
                    className={`awg-restart${restarting ? ' spinning' : ''}`}
                    aria-label="Перезапустить AWG"
                    title="Перезапустить AWG-интерфейс"
                    onClick={restartAwg}
                    disabled={restarting}
                >
                    <IcoRefresh /> {restarting ? 'Перезапуск…' : 'Перезапустить AWG'}
                </button>
            )}

            {!token ? (
                <div className="login-screen">
                    <div className="login-card">
                        <span className="logo-name">{brand.brand}</span>
                        <p className="login-sub">Войдите, чтобы продолжить</p>
                        <input
                            className="field"
                            placeholder="Логин"
                            value={loginUser}
                            onChange={e => setLoginUser(e.target.value)}
                            autoFocus
                        />
                        <input
                            className="field"
                            type="password"
                            placeholder="Пароль"
                            value={loginPass}
                            onChange={e => setLoginPass(e.target.value)}
                            onKeyDown={e => e.key === 'Enter' && login()}
                        />
                        <button className="btn btn--primary btn--full" onClick={login}>
                            Войти
                        </button>
                        {statusText.startsWith('offline:') && (
                            <p className="login-error">{statusLabel}</p>
                        )}
                    </div>
                </div>
            ) : (
                <>
                    {/* Top bar — видим только на мобильном */}
                    <header className="top-bar">
                        <button
                            className="hamburger-btn"
                            aria-label="Открыть меню"
                            onClick={() => setDrawerOpen(true)}
                        >
                            <IcoMenu />
                        </button>
                        <span className="top-bar-title">{tab.label}</span>
                    </header>

                    {/* Затемнение под drawer */}
                    <div
                        className={`drawer-scrim${drawerOpen ? ' drawer-scrim--visible' : ''}`}
                        onClick={() => setDrawerOpen(false)}
                    />

                    <div className="layout">
                        <aside
                            className={`sidebar${drawerOpen ? ' sidebar--open' : ''}`}
                            onTouchStart={handleTouchStart}
                            onTouchMove={handleTouchMove}
                            onTouchEnd={handleTouchEnd}
                        >
                            <div className="sidebar-logo">
                                <span className="logo-name">{brand.brand}</span>
                                <span className="sidebar-version">
                                    {[brand.channel, brand.version].filter(Boolean).join(' ')}
                                </span>
                            </div>
                            <hr className="drawer-divider" />
                            {TABS.map(t => (
                                <div
                                    key={t.id}
                                    className={`nav-item${t.id === activeTab ? ' nav-item--active' : ''}`}
                                    onClick={() => { setActiveTab(t.id); setDrawerOpen(false); }}
                                >
                                    <t.Icon /> {t.label}
                                </div>
                            ))}

                            <div className="sidebar-spacer" />

                            <button className="btn btn--danger" onClick={logout}>
                                <IcoLogout /> Выйти
                            </button>
                        </aside>

                        <main className="main">
                            <h2 className="page-title">{tab.label}</h2>
                            {/* Общий хром над вкладкой — App владеет им сам, вкладки про него не знают */}
                            {showServerBar && (
                                <ServerBar
                                    nodes={nodes}
                                    serverId={serverId}
                                    hubEnabled={hubEnabled}
                                    onSelect={pickServer}
                                    onAdd={() => setModal({})}
                                    onJoin={n => setModal({ node: n })}
                                    onDelete={deleteNode}
                                    onRestartAwg={restartAwg}
                                    restarting={restarting}
                                    showRestart={showRestart}
                                />
                            )}
                            {serverId === null && showServerBar ? (
                                <p className="login-sub">
                                    Серверов пока нет. Добавьте ноду: она сама подключится к этой панели.
                                </p>
                            ) : (
                                // key: при смене сервера вкладка монтируется заново и читает данные уже с ним
                                <tab.Page key={`${tab.id}:${serverId}`} token={token} showMsg={showMsg} />
                            )}
                        </main>
                    </div>
                </>
            )}

            {modal && (
                <AddServerModal
                    token={token}
                    node={modal.node}
                    onClose={() => setModal(null)}
                    onChanged={() => { loadNodes(token).catch(() => { /* следующий тик */ }); }}
                    showMsg={showMsg}
                />
            )}

            {msg && <div className="snack">{msg}</div>}
        </div>
    );
}