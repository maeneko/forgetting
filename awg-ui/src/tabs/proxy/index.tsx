// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useMemo, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type PageProps, type MTProtoStatus } from '../../lib/shared';
import { qrDataUrl } from '../../lib/qr';
import { IcoPlus, IcoPower, IcoQR, IcoTrash, IcoCopy, IcoShield, IcoTelegram } from '../../components/icons';
import './proxy.css';

// Поля свежего awg-ctrl. Старый бэкенд их не отдаёт, поэтому все необязательные:
// без host/firewall/link_reason вкладка должна работать как раньше.
interface FirewallState {
    backend:   'ufw' | 'firewalld' | 'iptables' | 'none';
    port_open: boolean;
    error?:    string;
}
type Mst = MTProtoStatus & {
    host?:        string;          // FakeTLS-домен, который сейчас в конфиге
    link_reason?: string;          // почему link пуст: «прокси остановлен» / «нужен бинарник mtg»
    firewall?:    FirewallState;
    installed?:   boolean;         // прокси-сервис установлен (не выводим из port > 0)
};

// Домен FakeTLS: латинские буквы/цифры/точки/дефисы, минимум одна точка; пустой — ок
// (сервер подставит значение по умолчанию).
const HOST_LABEL = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/;
const hostProblem = (raw: string): string => {
    const h = raw.trim();
    if (!h) return '';
    if (h.length > 253) return 'Слишком длинный домен (больше 253 символов)';
    const labels = h.split('.');
    if (labels.length < 2) return 'Нужен домен с точкой, например storage.googleapis.com';
    if (!labels.every(l => HOST_LABEL.test(l))) return 'Только латинские буквы, цифры, точки и дефисы';
    return '';
};

// Статус сервиса: цвет — только акцент, состояние объясняет текст под заголовком.
type Tone = 'online' | 'offline' | 'error' | 'warm';
interface StateView { label: string; tone: Tone }

// Telegram-прокси (MTProto, mtg) на сервере из шапки. Панель поднимает его через awg-ctrl ноды
// (обычный /api-прокси с X-Server-Id, при смене сервера вкладка перемонтируется): awg-ctrl пишет
// конфиг и systemd-юнит, запускает сервис и отдаёт ссылку t.me/proxy?… Секрет и порт переживают
// перезапуски; «Выключить» их сохраняет — включение заново вернёт ту же ссылку.
//
// Успех не выводится из HTTP 200: awg-ctrl отвечает 200 и при упавшем сервисе (error = journal tail),
// поэтому «Прокси поднят» показываем только когда в ответе running. Ссылка/QR — только при непустом
// link; иначе строка с link_reason — «нерабочая» ссылка хуже, чем честное «остановлен».
export default function ProxyPage({ token, showMsg }: PageProps) {
    const [mst, setMst]          = useState<Mst | null>(null);
    const [port, setPort]        = useState('');
    const [host, setHost]        = useState('');
    const [hostErr, setHostErr]  = useState('');
    const [doing, setDoing]      = useState('');
    const [error, setError]      = useState('');
    const [unsupported, setUnsupported] = useState(false);
    const [qr, setQr]            = useState<{ dataUrl: string; link: string } | null>(null);

    const errText = (e: unknown, fallback: string) =>
        axios.isAxiosError(e) ? e.response?.data?.error ?? fallback : fallback;

    const fill = (s: Mst) => {
        setMst(s);
        setPort(s.port ? String(s.port) : '443');
        // Старый бэкенд host не отдаёт — тогда не трогаем то, что ввёл пользователь.
        if (typeof s.host === 'string') setHost(s.host);
    };

    useEffect(() => {
        let alive = true;
        apiFetch('GET', '/api/mtproto', token)
            .then(s => { if (alive) fill(s as Mst); })
            .catch(e => {
                if (!alive) return;
                if (axios.isAxiosError(e) && e.response?.status === 404) setUnsupported(true);
                else setError(errText(e, 'Не удалось получить статус прокси'));
            });
        return () => { alive = false; };
    }, [token]);

    const act = async (fn: () => Promise<Mst>, done: string, action: string) => {
        setDoing(action); setError(''); setHostErr('');
        try {
            const s = await fn();
            fill(s);
            // Включение и смена секрета считаются успехом только при running: иначе показываем
            // текст ошибки сервера (тост + блок) и остаёмся в состоянии «остановлен».
            if ((action === 'up' || action === 'rotate') && !s.running) {
                const why = s.error || 'Прокси не запустился';
                setError(why);
                showMsg(why);
                return;
            }
            showMsg(done);
        } catch (e) {
            const msg = errText(e, 'Не удалось выполнить действие');
            // 400 от бэкенда приходит как «host: …» — показываем прямо у поля домена.
            if (/^host\b/i.test(msg)) setHostErr(msg);
            else setError(msg);
        } finally { setDoing(''); }
    };

    const hostBody = () => {
        const h = host.trim();
        return h ? { host: h } : {};
    };
    const up = () => {
        const bad = hostProblem(host);
        if (bad) { setHostErr(bad); return; }
        return act(
            () => apiFetch('POST', '/api/mtproto', token,
                { port: Number(port) || 443, ...hostBody() }) as Promise<Mst>,
            'Прокси поднят', 'up');
    };
    const rotate = () => {
        const bad = hostProblem(host);
        if (bad) { setHostErr(bad); return; }
        return act(
            () => apiFetch('POST', '/api/mtproto/rotate', token, hostBody()) as Promise<Mst>,
            'Секрет изменён — старая ссылка больше не работает', 'rotate');
    };
    const stop = () => act(
        () => apiFetch('DELETE', '/api/mtproto', token) as Promise<Mst>,
        'Прокси остановлен', 'stop');
    const installMtg = () => act(
        () => apiFetch('POST', '/api/mtproto/mtg', token) as Promise<Mst>,
        'Компонент прокси установлен', 'install-mtg');
    const removeMtg = () => {
        if (!confirm('Удалить компонент прокси? Прокси будет остановлен, компонент удалён. Порт и секрет останутся — после установки прокси можно включить снова.')) return;
        void act(() => apiFetch('DELETE', '/api/mtproto/mtg', token) as Promise<Mst>,
            'Компонент прокси удалён', 'remove-mtg');
    };
    const removeService = () => {
        if (!confirm('Удалить прокси? Сервис, конфиг, порт и секрет будут удалены, ссылка перестанет работать. Компонент прокси останется.')) return;
        void act(() => apiFetch('DELETE', '/api/mtproto/service', token) as Promise<Mst>,
            'Прокси удалён', 'remove-service');
    };
    const showQR = () => {
        if (!mst?.link) return;
        try { setQr({ dataUrl: qrDataUrl(mst.link, 'H'), link: mst.link }); }
        catch { showMsg('Не удалось сгенерировать QR-код'); }
    };
    const copyLink = (text: string) =>
        copyText(text).then(() => showMsg('Ссылка скопирована'));

    const portN = Number(port);
    const portValid = Number.isInteger(portN) && portN >= 1 && portN <= 65535;
    const hostBad = hostProblem(host);
    const hostOk = !hostBad;
    const hostShownErr = hostErr || hostBad;
    // installed — из ответа API; port > 0 оставлен запасным вариантом для старого бэкенда,
    // где поля installed ещё нет.
    const installed = !!mst && (mst.installed ?? mst.port > 0);
    const running = !!mst?.running;
    const busy = doing !== '';
    const link = mst?.link ?? '';
    // Старый бэкенд не присылает link_reason — выводим ту же фразу сами, чтобы пустое место
    // не выглядело как «ссылки нет, всё хорошо».
    const linkReason = link ? '' : (mst?.link_reason
        ?? (running ? '' : mst?.binary ? 'прокси остановлен' : 'нужен бинарник mtg'));
    const fw = mst?.firewall;
    const fwWarn = !!fw && (fw.backend === 'none' || fw.port_open === false);
    const changed = !!mst && (portN !== mst.port || host.trim() !== (mst.host ?? ''));

    const state: StateView | null = !mst ? null
        : running ? { label: 'работает', tone: 'online' }
        : mst.error ? { label: 'ошибка запуска', tone: 'error' }
        : mst.binary ? { label: 'остановлен', tone: 'offline' }
        : { label: 'нет компонента', tone: 'warm' };

    const powerLabel = running ? (doing === 'stop' ? 'Выключение…' : 'Выключить')
        : installed ? (doing === 'up' ? 'Включение…' : 'Включить')
        : (doing === 'up' ? 'Запуск…' : 'Запустить прокси');

    const stateDesc = !mst
        ? 'Не удалось получить состояние прокси.'
        : running
            ? `Слушает TCP-порт ${mst.port}. Домен FakeTLS — ${mst.host || 'по умолчанию'}.`
            : mst.error
                ? 'Сервис не поднялся при запуске. Вывод журнала — ниже; исправьте настройки и включите снова.'
                : mst.binary
                    ? installed
                        ? `Сервис остановлен. Порт ${mst.port} и секрет сохранены — при включении ссылка останется прежней.`
                        : 'Компонент установлен, сервис ещё не создан. Запустите прокси — ссылка появится здесь.'
                    : 'Компонент mtg не установлен: без него прокси не запустится.';

    // Пустой порт — ошибка только при попытке применить; поле сразу объясняет правило.
    const portErr = portValid ? '' : 'Порт — целое число от 1 до 65535.';
    const applyHint = busy ? 'Дождитесь завершения действия.'
        : !portValid ? 'Проверьте порт: целое число от 1 до 65535.'
        : !hostOk ? 'Проверьте домен FakeTLS.'
        : !mst?.binary ? 'Сначала установите компонент прокси.'
        : !running ? (installed
            ? 'Прокси остановлен — настройки применятся при включении.'
            : 'Запустите прокси — настройки применятся при запуске.')
        : !changed ? 'Изменений нет.'
        : 'Панель перезапустит сервис с новыми настройками.';
    const canApply = running && changed && portValid && hostOk && !busy;

    const rotateHint = !installed
        ? 'Сначала запустите прокси — менять пока нечего.'
        : !hostOk
            ? 'Исправьте домен FakeTLS в настройках.'
            : 'Смена секрета отключит старые ссылки у клиентов.';

    const emptyLinkHint = !mst?.binary
        ? 'Установите компонент и включите прокси — ссылка появится здесь.'
        : running ? 'Сервис работает, но ссылку собрать не удалось. Проверьте порт и домен.'
        : 'Включите прокси — ссылка появится здесь.';

    // QR рисуется один раз на ссылку: картинка всегда светлая, поэтому в тёмной теме
    // не перекрашивается и не «прыгает».
    const qrSrc = useMemo(() => {
        if (!link) return '';
        try { return qrDataUrl(link, 'H'); } catch { return ''; }
    }, [link]);

    if (unsupported) {
        return (
            <div className="proxy-page">
                <section className="proxy-hero">
                    <span className="proxy-hero-glyph" aria-hidden="true"><IcoTelegram /></span>
                    <div className="proxy-hero-text">
                        <div className="proxy-hero-head">
                            <h3 className="proxy-hero-title">Telegram-прокси</h3>
                            <span className="chip chip--offline proxy-state-chip">
                                <span className="chip-dot" />недоступно
                            </span>
                        </div>
                        <p className="proxy-hero-desc">
                            Этот сервер без awg-ctrl: прокси живёт только на ноде или relay.
                        </p>
                    </div>
                </section>
            </div>
        );
    }

    const loading = !mst && !error;

    return (
        <div className="proxy-page">
            <p className="proxy-lead">
                Прокси для Telegram живёт на этом сервере и не делит канал с VPN. Клиенты подключаются
                по ссылке — параметры приложение подставит само.
            </p>

            {loading ? (
                <section className="proxy-hero proxy-hero--loading" aria-busy="true" aria-live="polite">
                    <span className="proxy-sr">Загрузка состояния прокси…</span>
                    <span className="proxy-skel proxy-skel--glyph" aria-hidden="true" />
                    <div className="proxy-hero-text">
                        <span className="proxy-skel proxy-skel--title" aria-hidden="true" />
                        <span className="proxy-skel proxy-skel--line" aria-hidden="true" />
                    </div>
                </section>
            ) : (
                <section className="proxy-hero">
                    <span className="proxy-hero-glyph" aria-hidden="true"><IcoTelegram /></span>
                    <div className="proxy-hero-text">
                        <div className="proxy-hero-head">
                            <h3 className="proxy-hero-title">Telegram-прокси</h3>
                            {state ? (
                                <span className={`chip chip--${state.tone} proxy-state-chip`}>
                                    <span className="chip-dot" />
                                    {state.label}
                                </span>
                            ) : (
                                <span className="chip chip--error proxy-state-chip">
                                    <span className="chip-dot" />нет данных
                                </span>
                            )}
                            {installed && fwWarn && (
                                <span className="chip chip--warm proxy-state-chip">
                                    <span className="chip-dot" />порт закрыт
                                </span>
                            )}
                        </div>
                        <p className="proxy-hero-desc">{stateDesc}</p>

                        {mst?.error && (
                            <div className="proxy-log-wrap">
                                <span className="proxy-log-label">Журнал запуска</span>
                                <pre className="proxy-log" tabIndex={0} role="region"
                                    aria-label="Вывод журнала запуска прокси">{mst.error}</pre>
                            </div>
                        )}

                        {error && <p className="proxy-alert" role="alert">{error}</p>}
                    </div>

                    {mst && (
                        <div className="proxy-hero-actions">
                            {mst.binary ? (
                                <button
                                    className={`btn ${running ? 'btn--outline' : 'btn--primary'}`}
                                    onClick={() => { if (running) void stop(); else void up(); }}
                                    disabled={busy || (!running && (!portValid || !hostOk))}
                                    title={!running && (!portValid || !hostOk)
                                        ? 'Проверьте порт и домен в настройках'
                                        : undefined}
                                >
                                    {installed ? <IcoPower /> : <IcoPlus />}
                                    {powerLabel}
                                </button>
                            ) : (
                                <button className="btn btn--primary" onClick={installMtg} disabled={busy}>
                                    <IcoPlus />
                                    {doing === 'install-mtg' ? 'Установка…' : 'Установить компонент'}
                                </button>
                            )}
                        </div>
                    )}
                </section>
            )}

            {mst && (
                <>
                    <div className="proxy-grid">
                        <section className="proxy-card proxy-card--settings" aria-labelledby="proxy-settings-title">
                            <header className="proxy-card-head">
                                <h3 className="proxy-card-title" id="proxy-settings-title">Настройки</h3>
                                <p className="proxy-card-sub">Порт и домен FakeTLS</p>
                            </header>

                            <div className="proxy-form">
                                <div className="proxy-field">
                                    <label className="proxy-field-label" htmlFor="proxy-port">Порт</label>
                                    <input
                                        id="proxy-port"
                                        className="field proxy-port-input"
                                        type="number"
                                        inputMode="numeric"
                                        min={1}
                                        max={65535}
                                        value={port}
                                        disabled={busy}
                                        aria-invalid={!portValid}
                                        aria-describedby={`proxy-port-hint${portErr ? ' proxy-port-err' : ''}`}
                                        onChange={e => setPort(e.target.value)}
                                    />
                                    <p className="proxy-field-hint" id="proxy-port-hint">
                                        TCP-порт для клиентов. Обычно 443. Занятый порт сервер не примет.
                                    </p>
                                    {portErr && (
                                        <p className="proxy-field-error" id="proxy-port-err" role="alert">{portErr}</p>
                                    )}

                                    {fwWarn && installed && (
                                        <div className="proxy-fw" role="note">
                                            <span className="proxy-fw-glyph" aria-hidden="true"><IcoShield /></span>
                                            <div className="proxy-fw-text">
                                                <p className="proxy-fw-title">Порт не открыт в фаерволе</p>
                                                <p className="proxy-fw-desc">
                                                    Клиенты не подключатся.
                                                    {' '}{fw?.error
                                                        ? `Причина: ${fw.error}`
                                                        : `Откройте порт ${mst.port} в фаерволе сервера.`}
                                                </p>
                                            </div>
                                        </div>
                                    )}
                                </div>

                                <div className="proxy-field">
                                    <label className="proxy-field-label" htmlFor="proxy-host">Домен для FakeTLS</label>
                                    <input
                                        id="proxy-host"
                                        className="field proxy-host-input"
                                        type="text"
                                        inputMode="url"
                                        placeholder="storage.googleapis.com"
                                        value={host}
                                        disabled={busy}
                                        spellCheck={false}
                                        autoComplete="off"
                                        aria-invalid={!hostOk || !!hostErr}
                                        aria-describedby={`proxy-host-hint${hostShownErr ? ' proxy-host-err' : ''}`}
                                        onChange={e => { setHost(e.target.value); if (hostErr) setHostErr(''); }}
                                    />
                                    <p className="proxy-field-hint" id="proxy-host-hint">
                                        Домен, который увидят клиенты при подключении. Должен открываться из их
                                        страны. Пустое — сервер подставит значение по умолчанию.
                                    </p>
                                    {hostShownErr && (
                                        <p className="proxy-field-error" id="proxy-host-err" role="alert">{hostShownErr}</p>
                                    )}
                                </div>
                            </div>

                            <footer className="proxy-form-foot">
                                <p className="proxy-form-note">{applyHint}</p>
                                <button className="btn btn--tonal" onClick={up} disabled={!canApply}>
                                    Применить
                                </button>
                            </footer>
                        </section>

                        <section className="proxy-card proxy-card--link" aria-labelledby="proxy-link-title">
                            <header className="proxy-card-head">
                                <h3 className="proxy-card-title" id="proxy-link-title">Ссылка для клиентов</h3>
                                <p className="proxy-card-sub">Открывается в Telegram</p>
                            </header>

                            {link ? (
                                <div className="proxy-link">
                                    <div className="proxy-link-main">
                                        <code className="proxy-link-value">{link}</code>
                                        <div className="proxy-link-actions">
                                            <button className="btn btn--tonal" disabled={busy}
                                                onClick={() => copyLink(link)}>
                                                <IcoCopy /> Скопировать
                                            </button>
                                            <button className="btn btn--outline" disabled={busy}
                                                onClick={showQR} title="Открыть QR-код крупнее">
                                                <IcoQR /> Крупнее
                                            </button>
                                        </div>
                                        <p className="proxy-link-hint">
                                            Ссылка работает, пока запущен прокси. Новый секрет — в «Управлении».
                                        </p>
                                    </div>

                                    <figure className="proxy-qr">
                                        {qrSrc
                                            ? <img className="proxy-qr-img" src={qrSrc} alt="QR-код ссылки на прокси" />
                                            : <span className="proxy-qr-fail">QR не построился</span>}
                                        <figcaption className="proxy-qr-cap">Отсканируйте в Telegram</figcaption>
                                    </figure>
                                </div>
                            ) : (
                                <div className="proxy-link-empty">
                                    <span className="proxy-link-empty-glyph" aria-hidden="true"><IcoQR /></span>
                                    <p className="proxy-link-empty-title">Ссылки пока нет</p>
                                    <p className="proxy-link-empty-desc">{emptyLinkHint}</p>
                                    {linkReason && (
                                        <p className="proxy-link-empty-reason">Причина: {linkReason}</p>
                                    )}
                                </div>
                            )}
                        </section>
                    </div>

                    {mst.binary && (
                        <section className="proxy-card proxy-card--control" aria-labelledby="proxy-control-title">
                            <header className="proxy-card-head">
                                <h3 className="proxy-card-title" id="proxy-control-title">Управление</h3>
                                <p className="proxy-card-sub">Секрет, компонент и сервис</p>
                            </header>

                            <div className="proxy-rows">
                                <div className="proxy-row">
                                    <div className="proxy-row-text">
                                        <span className="proxy-row-name">Секрет ссылки</span>
                                        <span className="proxy-row-hint">{rotateHint}</span>
                                    </div>
                                    <button className="btn btn--tonal" onClick={rotate}
                                        disabled={busy || !installed || !hostOk}>
                                        {doing === 'rotate' ? 'Меняем…' : 'Сменить секрет'}
                                    </button>
                                </div>

                                <div className="proxy-row">
                                    <div className="proxy-row-text">
                                        <span className="proxy-row-name">Компонент mtg</span>
                                        <span className="proxy-row-hint">Установлен на сервере.</span>
                                    </div>
                                    {mst.managed ? (
                                        <button className="btn btn--danger" onClick={removeMtg} disabled={busy}>
                                            <IcoTrash />
                                            {doing === 'remove-mtg' ? 'Удаление…' : 'Удалить компонент'}
                                        </button>
                                    ) : (
                                        <span className="proxy-row-note">Установлен вне панели — удалить нельзя.</span>
                                    )}
                                </div>

                                <div className="proxy-row">
                                    <div className="proxy-row-text">
                                        <span className="proxy-row-name">Прокси-сервис</span>
                                        <span className="proxy-row-hint">
                                            {running
                                                ? `Работает на порту ${mst.port}.`
                                                : installed
                                                    ? `Остановлен, порт ${mst.port}.`
                                                    : 'Ещё не создан.'}
                                        </span>
                                    </div>
                                    {installed ? (
                                        <button className="btn btn--danger" onClick={removeService} disabled={busy}>
                                            <IcoTrash />
                                            {doing === 'remove-service' ? 'Удаление…' : 'Удалить прокси'}
                                        </button>
                                    ) : (
                                        <span className="proxy-row-note">Создастся при запуске.</span>
                                    )}
                                </div>
                            </div>
                        </section>
                    )}
                </>
            )}

            {qr && (
                <div className="qr-backdrop" onClick={() => setQr(null)}>
                    <div className="qr-card proxy-qr-dialog" role="dialog" aria-modal="true"
                        aria-labelledby="proxy-qr-title" onClick={e => e.stopPropagation()}>
                        <p className="qr-name" id="proxy-qr-title">Telegram-прокси</p>
                        <button className="proxy-qr-shot" onClick={() => copyLink(qr.link)}
                            aria-label="Скопировать ссылку на прокси">
                            <img className="qr-img" src={qr.dataUrl} alt="" />
                        </button>
                        <p className="qr-hint">Отсканируйте в Telegram. Нажмите на код, чтобы скопировать ссылку.</p>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setQr(null)}>Закрыть</button>
                            <button className="btn btn--tonal" onClick={() => copyLink(qr.link)}>
                                <IcoCopy /> Скопировать ссылку
                            </button>
                        </div>
                    </div>
                </div>
            )}
        </div>
    );
}
