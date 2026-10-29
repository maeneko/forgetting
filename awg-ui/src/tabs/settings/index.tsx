// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useLayoutEffect, useCallback, useRef } from 'react';
import axios from 'axios';
import { apiFetch, copyText, timeAgo, BRAND_FALLBACK, SETTINGS_SECTION_KEY, type Brand, type NodeInfo, type PageProps } from '../../lib/shared';
import { useTheme, setThemeMode, type ThemeMode } from '../../lib/theme';
import { IcoCheck, IcoCopy, IcoRefresh, IcoTrash, IcoPlus } from '../../components/icons';
import ApiKeysPage from './apikeys';
import AuditPage from './audit';
import MigratePage from './migrate';
import PrometheusInstall from './prometheus';
import ServerSettings from './server';
import './settings.css';

// Вкладка «Настройки»: настройки самой панели, не конкретного сервера (те — в меню серверов).
// Разделы — по вкладкам MD3 (primary tabs) сверху, виден один:
//   Оформление — тема (хранится в браузере, lib/theme).
//   Сервер — деление канала и блокировка торрентов сервера из шапки (./server; туда ведёт и шестерёнка
//     в меню серверов).
//   API-ключи, Аудит, Перенос — бывшие отдельные вкладки меню; живут в подпапках и
//     рендерятся здесь целиком (реестр tabs/index.ts их не видит — он ищет на уровень выше).
//   Prometheus — установка встроенного Prometheus (./prometheus) и токен «только метрики» для GET /metrics (/ui/metrics-token): выпуск,
//     перевыпуск, отзыв. Значение показывается один раз, в БД — только хэш.
//   О панели — имя, канал, версия и серверы.
interface MetricsToken {
    exists:     boolean;
    prefix?:    string;
    created_at?: number;
    last_used?: number | null;
    per_peer:   boolean;    // пер-пировые серии (METRICS_PEERS=on в .env)
}

const SECTIONS = [
    { id: 'look',    label: 'Оформление' },
    { id: 'server',  label: 'Сервер' },
    { id: 'apikeys', label: 'API-ключи' },
    { id: 'prom',    label: 'Prometheus' },
    { id: 'audit',   label: 'Аудит' },
    { id: 'migrate', label: 'Перенос' },
    { id: 'about',   label: 'О панели' },
] as const;
type SectionId = (typeof SECTIONS)[number]['id'];

// Открытый раздел помнится в браузере — чтобы «Аудит» или «Перенос» не искать заново
// (туда же пишет openTab из lib/shared, когда другая вкладка ведёт в нужный раздел)
const indexOf = (id: SectionId) => SECTIONS.findIndex(t => t.id === id);
function savedSection(): SectionId {
    try {
        const v = localStorage.getItem(SETTINGS_SECTION_KEY);
        if (SECTIONS.some(t => t.id === v)) return v as SectionId;
    } catch { /* не критично */ }
    return 'look';
}

// Фрагмент prometheus.yml для внешнего Prometheus. Адрес — от страницы: в нём уже секретный путь
// панели (UI_PREFIX) или подпапка nginx, поэтому конфиг верен в обоих случаях.
function scrapeConfig(tok: string): string {
    const u = new URL('metrics', location.href.split(/[?#]/)[0]);
    return [
        'scrape_configs:',
        '  - job_name: vpn-panel',
        `    scheme: ${u.protocol.replace(':', '')}`,
        `    metrics_path: ${u.pathname}`,
        '    authorization:',
        `      credentials: ${tok}`,
        '    static_configs:',
        `      - targets: ['${u.host}']`,
    ].join('\n');
}

const THEMES: { id: ThemeMode; label: string }[] = [
    { id: 'light',  label: 'Светлая' },
    { id: 'dark',   label: 'Тёмная' },
    { id: 'system', label: 'Как в системе' },
];

export default function SettingsPage({ token, showMsg }: PageProps) {
    const { mode } = useTheme();
    const [metrics, setMetrics] = useState<MetricsToken | null>(null);
    const [created, setCreated] = useState<string | null>(null);   // открытый токен — один раз после выпуска
    const [busy, setBusy]       = useState(false);
    const [brand, setBrand]     = useState<Brand>({ brand: BRAND_FALLBACK, channel: '', version: '' });
    const [nodes, setNodes]     = useState<NodeInfo[]>([]);
    // dir — откуда въезжает раздел: 1 — справа (вкладка правее прежней), -1 — слева, 0 — первый показ
    const [view, setView]       = useState<{ section: SectionId; dir: number }>(() => ({ section: savedSection(), dir: 0 }));
    const section = view.section;
    const go = (id: SectionId) => setView(v => v.section === id ? v : { section: id, dir: Math.sign(indexOf(id) - indexOf(v.section)) });
    const tabRefs   = useRef<Partial<Record<SectionId, HTMLButtonElement | null>>>({});
    const indicator = useRef<HTMLSpanElement>(null);
    const centered  = useRef(false);   // первый показ — без анимации прокрутки вкладок

    // Индикатор — одна полоса под активной вкладкой: переезжает к ней, а не появляется заново
    useLayoutEffect(() => {
        const place = () => {
            const b = tabRefs.current[section], el = indicator.current;
            if (b && el) { el.style.left = `${b.offsetLeft}px`; el.style.width = `${b.offsetWidth}px`; }
        };
        place();
        // На узком экране вкладки прокручиваются — выбранную ставим по центру полосы (у краёв — насколько
        // позволяет прокрутка). Двигаем саму полосу, а не scrollIntoView: тот прокрутил бы и страницу.
        const b = tabRefs.current[section], bar = b?.parentElement;
        if (b && bar && bar.scrollWidth > bar.clientWidth) {
            const still = !centered.current || matchMedia('(prefers-reduced-motion: reduce)').matches;
            bar.scrollTo({ left: b.offsetLeft - (bar.clientWidth - b.offsetWidth) / 2, behavior: still ? 'auto' : 'smooth' });
        }
        centered.current = true;
        try { localStorage.setItem(SETTINGS_SECTION_KEY, section); } catch { /* не критично */ }
        window.addEventListener('resize', place);
        return () => window.removeEventListener('resize', place);
    }, [section]);

    // openTab('settings', раздел) при уже открытых настройках: раздел записан в localStorage — переключаемся на него
    useEffect(() => {
        const h = (e: Event) => { if ((e as CustomEvent).detail === 'settings') go(savedSection()); };
        window.addEventListener('awg:open-tab', h);
        return () => window.removeEventListener('awg:open-tab', h);
    }, []);

    // Стрелки, Home/End — по вкладкам (MD3 / WAI-ARIA tabs)
    const onTabKey = (e: React.KeyboardEvent) => {
        const i = indexOf(section);
        const j = e.key === 'ArrowRight' ? (i + 1) % SECTIONS.length
                : e.key === 'ArrowLeft'  ? (i + SECTIONS.length - 1) % SECTIONS.length
                : e.key === 'Home' ? 0 : e.key === 'End' ? SECTIONS.length - 1 : -1;
        if (j < 0) return;
        e.preventDefault();
        go(SECTIONS[j].id);
        tabRefs.current[SECTIONS[j].id]?.focus();
    };

    // Адрес /metrics рядом с панелью — с учётом префикса, под которым она открыта
    const metricsUrl = new URL('metrics', location.href.split(/[?#]/)[0]).href;

    const load = useCallback(async () => {
        try {
            setMetrics(await apiFetch('GET', '/ui/metrics-token', token));
        } catch {
            showMsg('Ошибка загрузки настроек');
        }
        axios.get('/ui/brand').then(({ data }) => setBrand({
            brand: data.brand || BRAND_FALLBACK, channel: data.channel || '', version: data.version || '',
        })).catch(() => {});
        apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
    }, [token, showMsg]);

    useEffect(() => { if (token) load(); }, [token, load]);

    const issue = useCallback(async () => {
        if (metrics?.exists && !confirm('Перевыпустить токен? Старый сразу перестанет работать — Prometheus нужно будет перенастроить.')) return;
        setBusy(true);
        try {
            const r = await apiFetch('POST', '/ui/metrics-token', token);
            setCreated(r.token);
            setMetrics(r);
        } catch (e: any) {
            showMsg(e?.response?.data?.error ?? 'Ошибка запроса');
        } finally {
            setBusy(false);
        }
    }, [metrics, token, showMsg]);

    const revoke = useCallback(async () => {
        if (!confirm('Отозвать токен? Prometheus перестанет получать метрики панели.')) return;
        setBusy(true);
        try {
            setMetrics(await apiFetch('DELETE', '/ui/metrics-token', token));
            showMsg('Токен отозван');
        } catch (e: any) {
            showMsg(e?.response?.data?.error ?? 'Ошибка запроса');
        } finally {
            setBusy(false);
        }
    }, [token, showMsg]);

    const online = nodes.filter(n => n.online).length;

    return (
        <>
            <div className="st-tabs" role="tablist" aria-label="Разделы настроек" onKeyDown={onTabKey}>
                {SECTIONS.map(t => (
                    <button key={t.id} type="button" role="tab" id={`st-tab-${t.id}`} className="st-tab"
                        aria-selected={section === t.id} aria-controls={`st-panel-${t.id}`}
                        tabIndex={section === t.id ? 0 : -1}
                        ref={el => { tabRefs.current[t.id] = el; }} onClick={() => go(t.id)}>
                        {t.label}
                    </button>
                ))}
                <span className="st-tabs-indicator" ref={indicator} aria-hidden="true" />
            </div>

            {/* Раздел пересоздаётся при смене (key) и въезжает со стороны выбранной вкладки */}
            <div key={section} className="st-view" role="tabpanel" id={`st-panel-${section}`} aria-labelledby={`st-tab-${section}`}
                style={{ '--st-dir': view.dir } as React.CSSProperties}>
            {section === 'look' && (
            <section className="st-section">
                <div className="st-row">
                    <span className="st-row-text">
                        <span className="st-row-name">Тема</span>
                        <span className="st-hint">Запоминается в этом браузере</span>
                    </span>
                    <div className="segmented" role="radiogroup" aria-label="Тема">
                        {THEMES.map(t => (
                            <button key={t.id} type="button" role="radio" aria-checked={mode === t.id}
                                className="segmented-item" onClick={() => setThemeMode(t.id)}>
                                {mode === t.id && <IcoCheck />}{t.label}
                            </button>
                        ))}
                    </div>
                </div>
            </section>
            )}

            {section === 'prom' && <PrometheusInstall token={token} showMsg={showMsg} />}

            {section === 'prom' && (
            <section className="st-section">
                <p className="st-hint st-lead">
                    Метрики панели и всех серверов отдаются по адресу <code className="st-code">{metricsUrl}</code> только
                    с токеном: Prometheus передаёт его заголовком <code className="st-code">Authorization: Bearer</code>.
                    Токен открывает только метрики — ни панель, ни API.
                </p>
                <div className="st-row">
                    <span className="st-row-text">
                        <span className="st-row-name">Токен метрик</span>
                        {metrics?.exists ? (
                            <span className="st-hint">
                                <code className="st-code">{metrics.prefix}</code> · выпущен {new Date((metrics.created_at ?? 0) * 1000).toLocaleDateString()}
                                {' · '}{metrics.last_used ? `скрейп ${timeAgo(metrics.last_used)} назад` : 'ещё не использовался'}
                            </span>
                        ) : (
                            <span className="st-hint">{metrics ? 'Не выпущен — /metrics закрыт' : 'Загрузка…'}</span>
                        )}
                    </span>
                    {metrics && (
                        <span className="st-row-ctl">
                            {metrics.exists && (
                                <button className="btn btn--danger" disabled={busy} onClick={revoke}><IcoTrash /> Отозвать</button>
                            )}
                            <button className={`btn ${metrics.exists ? 'btn--tonal' : 'btn--primary'}`} disabled={busy} onClick={issue}>
                                {metrics.exists ? <><IcoRefresh /> Перевыпустить</> : <><IcoPlus /> Выпустить</>}
                            </button>
                        </span>
                    )}
                </div>
                {metrics && (
                    <div className="st-row">
                        <span className="st-row-text">
                            <span className="st-row-name">Метрики по пользователям</span>
                            <span className="st-hint">
                                {metrics.per_peer
                                    ? 'Включены: серии по каждому пиру с внутренним IP и именем'
                                    : 'Выключены. Включаются строкой METRICS_PEERS=on в .env панели'}
                            </span>
                        </span>
                        <span className={`chip chip--${metrics.per_peer ? 'online' : 'offline'}`}>
                            <span className="chip-dot" />{metrics.per_peer ? 'Вкл' : 'Выкл'}
                        </span>
                    </div>
                )}
            </section>
            )}

            {section === 'server'  && <ServerSettings token={token} showMsg={showMsg} />}
            {section === 'apikeys' && <ApiKeysPage token={token} showMsg={showMsg} />}
            {section === 'audit'   && <AuditPage   token={token} showMsg={showMsg} />}
            {section === 'migrate' && <MigratePage token={token} showMsg={showMsg} />}

            {section === 'about' && (
            <section className="st-section">
                <dl className="st-facts">
                    <div><dt>Панель</dt><dd>{brand.brand}</dd></div>
                    <div><dt>Версия</dt><dd>{[brand.channel, brand.version].filter(Boolean).join(' ') || '—'}</dd></div>
                    <div><dt>Серверы</dt><dd>{nodes.length ? `${nodes.length}, на связи ${online}` : '—'}</dd></div>
                </dl>
            </section>
            )}
            </div>

            {created && (
                <div className="qr-backdrop" onClick={() => setCreated(null)}>
                    <div className="qr-card" role="dialog" aria-modal="true" aria-labelledby="st-new-title" onClick={e => e.stopPropagation()}>
                        <p className="qr-name" id="st-new-title">Токен метрик выпущен</p>
                        <p className="qr-hint">
                            Скопируй его сейчас — он показывается один раз. Готовый фрагмент для prometheus.yml:
                        </p>
                        <pre className="st-secret st-yml">{scrapeConfig(created)}</pre>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setCreated(null)}>Закрыть</button>
                            <button className="btn btn--tonal" onClick={async () => {
                                await copyText(created);
                                showMsg('Токен скопирован');
                            }}><IcoCopy /> Токен</button>
                            <button className="btn btn--primary" onClick={async () => {
                                await copyText(scrapeConfig(created));
                                showMsg('Фрагмент prometheus.yml скопирован');
                            }}><IcoCopy /> Фрагмент</button>
                        </div>
                    </div>
                </div>
            )}
        </>
    );
}
