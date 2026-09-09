// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useRef, useCallback } from 'react';
import axios from 'axios';
import './App.css';
import { TOKEN_KEY, THEME_KEY } from './lib/shared';
import { APP_NAME, APP_VERSION } from './lib/branding';
import { IcoMenu, IcoLogout, IcoSun, IcoMoon } from './components/icons';
import { TABS } from './tabs';

// Оболочка приложения: логин, тема, drawer/сайдбар, общая сессия (token,
// снэкбар) и переключение вкладок. Контент каждой вкладки приходит из реестра
// TABS — App про конкретные вкладки ничего не знает.
export default function App() {
    const [token, setToken]           = useState(() => localStorage.getItem(TOKEN_KEY) ?? '');
    const [loginUser, setLoginUser]   = useState('');
    const [loginPass, setLoginPass]   = useState('');
    const [loginError, setLoginError] = useState('');
    const [msg, setMsg]               = useState('');
    const [drawerOpen, setDrawerOpen] = useState(false);
    const [activeTab, setActiveTab]   = useState(() => TABS[0]?.id ?? '');
    const [theme, setTheme]           = useState<'light' | 'dark'>(() =>
        localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light');
    const touchStartX                 = useRef(0);
    const touchCurrentX               = useRef(0);

    const showMsg = useCallback((text: string) => {
        setMsg(text);
        setTimeout(() => setMsg(''), 3000);
    }, []);

    // Проверяет, жив ли токен. Данные конкретных вкладок грузят сами вкладки.
    const startSession = useCallback(async (tok: string) => {
        await axios.get('/api/session', { headers: { Authorization: `Bearer ${tok}` } });
    }, []);

    const login = useCallback(async () => {
        try {
            const { data: auth } = await axios.post('/login', { user: loginUser, pass: loginPass });
            const tok = auth.token as string;
            await startSession(tok);
            localStorage.setItem(TOKEN_KEY, tok);
            setLoginError('');
            setToken(tok);
        } catch (e) {
            if (axios.isAxiosError(e) && e.response?.status === 401) {
                showMsg('Неверный логин или пароль');
            } else if (axios.isAxiosError(e) && e.response?.status === 429) {
                showMsg(e.response.data?.error ?? 'Слишком много попыток');
            } else {
                setLoginError('Сервер недоступен');
            }
        }
    }, [loginUser, loginPass, startSession, showMsg]);

    const logout = useCallback(async () => {
        try {
            await axios.post('/logout', null, { headers: { Authorization: `Bearer ${token}` } });
        } catch { /* токен уже мог истечь */ }
        localStorage.removeItem(TOKEN_KEY);
        setToken('');
        setDrawerOpen(false);
    }, [token]);

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

    // Вкладок может не быть вовсе — тогда вместо контента показываем заглушку.
    const tab = TABS.find(t => t.id === activeTab) ?? TABS[0] ?? null;

    return (
        <div className="app">
            <button
                className="theme-toggle"
                aria-label={theme === 'dark' ? 'Светлая тема' : 'Тёмная тема'}
                onClick={() => setTheme(t => (t === 'dark' ? 'light' : 'dark'))}
            >
                {theme === 'dark' ? <IcoSun /> : <IcoMoon />}
            </button>

            {!token ? (
                <div className="login-screen">
                    <div className="login-card">
                        <span className="logo-name">{APP_NAME}</span>
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
                        {loginError && <p className="login-error">{loginError}</p>}
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
                        <span className="top-bar-title">{tab?.label ?? APP_NAME}</span>
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
                                <span className="logo-name">{APP_NAME}</span>
                                <span className="sidebar-version">{APP_VERSION}</span>
                            </div>
                            <hr className="drawer-divider" />
                            {TABS.map(t => (
                                <div
                                    key={t.id}
                                    className={`nav-item${t.id === tab?.id ? ' nav-item--active' : ''}`}
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
                            <h2 className="page-title">{tab?.label ?? APP_NAME}</h2>
                            {tab
                                ? <tab.Page token={token} showMsg={showMsg} />
                                : <p className="placeholder">
                                      Вкладок пока нет — добавь папку в <code>src/tabs/</code>
                                  </p>}
                        </main>
                    </div>
                </>
            )}

            {msg && <div className="snack">{msg}</div>}
        </div>
    );
}
