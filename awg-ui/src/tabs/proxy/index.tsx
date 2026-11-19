// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type PageProps, type MTProtoStatus } from '../../lib/shared';
import { qrDataUrl } from '../../lib/qr';
import { IcoPlus, IcoPower, IcoQR, IcoTrash } from '../../components/icons';
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

    const portN = Number(port);
    const portValid = Number.isInteger(portN) && portN >= 1 && portN <= 65535;
    const hostOk = !hostProblem(host);
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
    const stateLabel = !mst ? 'проверка…' : running ? 'работает' : installed ? 'остановлен' : 'не установлен';
    const powerLabel = running ? (doing === 'stop' ? 'Выключение…' : 'Выключить')
        : installed ? (doing === 'up' ? 'Включение…' : 'Включить')
        : (doing === 'up' ? 'Установка…' : 'Установить');
    const changed = running && (portN !== mst?.port || host.trim() !== (mst?.host ?? ''));
    const fw = mst?.firewall;
    const fwWarn = !!fw && (fw.backend === 'none' || fw.port_open === false);

    return (
        <div className="proxy-layout">
            <section className="proxy-card proxy-card--about">
                <h3 className="ss-section">Telegram-прокси</h3>
                <p className="st-hint">
                    Поднимает прокси для Telegram прямо на этом сервере — для доступа к Telegram там, где он
                    заблокирован. Пользователям раздаётся ссылка: они нажимают её, и Telegram подключается сам.
                    Прокси работает независимо от VPN и канал не делит.
                </p>
            </section>

            <section className="proxy-card">
                <div className="proxy-card-head">
                    <h3 className="ss-section">Управление</h3>
                    {!unsupported && (
                        <div className="proxy-card-power">
                            <span className={`chip ${running ? 'chip--online' : 'chip--offline'}`}>
                                <span className="chip-dot" />
                                {stateLabel}
                            </span>
                            {installed && (
                                <button className="btn" onClick={rotate} disabled={busy || !hostOk}>
                                    {doing === 'rotate' ? 'Меняем…' : 'Сменить секрет'}
                                </button>
                            )}
                            {!!mst?.binary && (
                                <button
                                    className={`btn ${running ? 'btn--outline' : 'btn--primary'}`}
                                    onClick={() => { if (!mst) return; if (running) void stop(); else void up(); }}
                                    disabled={!mst || busy || (!running && (!portValid || !hostOk))}
                                >
                                    {installed ? <IcoPower /> : <IcoPlus />}
                                    {powerLabel}
                                </button>
                            )}
                        </div>
                    )}
                </div>

                {unsupported ? (
                    <p className="st-hint">
                        Этот сервер без awg-ctrl: прокси живёт только на ноде или relay.
                    </p>
                ) : (
                    <>
                        <label className="ss-setting">
                            <span className="ss-setting-text">
                                <span className="ss-setting-name">Порт</span>
                                <span className="st-hint">
                                    TCP-порт, на котором прокси слушает из интернета. Обычно 443 — такой порт не режут
                                    по «подозрительности». Занятый порт панель не примет.
                                </span>
                            </span>
                            <input className="field" type="number" inputMode="numeric" min={1} max={65535}
                                value={port} disabled={busy} onChange={e => setPort(e.target.value)} />
                        </label>

                        <label className="ss-setting">
                            <span className="ss-setting-text">
                                <span className="ss-setting-name">Домен для FakeTLS</span>
                                <span className="st-hint">
                                    Домен должен быть доступен клиентам в их стране. Пустое — сервер подставит
                                    значение по умолчанию.
                                </span>
                                {hostErr && (
                                    <span className="st-hint" role="alert" style={{ color: 'var(--error)' }}>
                                        {hostErr}
                                    </span>
                                )}
                            </span>
                            <input className="field" type="text" inputMode="url" placeholder="storage.googleapis.com"
                                value={host} disabled={busy} spellCheck={false} autoComplete="off"
                                aria-invalid={!hostOk}
                                onChange={e => { setHost(e.target.value); if (hostErr) setHostErr(''); }} />
                        </label>

                        {mst && (
                            <div className="proxy-components">
                                <div className="proxy-component">
                                    <span className="proxy-component-text">
                                        <span className="ss-setting-name">Компонент прокси</span>
                                        <span className="st-hint">
                                            {mst.binary ? 'установлен' : 'не установлен — без него прокси не запустится'}
                                        </span>
                                    </span>
                                    {!mst.binary ? (
                                        <button className="btn btn--primary" onClick={installMtg} disabled={busy}>
                                            <IcoPlus />
                                            {doing === 'install-mtg' ? 'Установка…' : 'Установить компонент'}
                                        </button>
                                    ) : mst.managed && (
                                        <button className="btn btn--danger" onClick={removeMtg} disabled={busy}>
                                            <IcoTrash />
                                            {doing === 'remove-mtg' ? 'Удаление…' : 'Удалить компонент'}
                                        </button>
                                    )}
                                </div>
                                {installed && (
                                    <div className="proxy-component">
                                        <span className="proxy-component-text">
                                            <span className="ss-setting-name">Прокси-сервис</span>
                                            <span className="st-hint">
                                                {running ? `работает на порту ${mst.port}` : `остановлен, порт ${mst.port}`}
                                            </span>
                                        </span>
                                        <button className="btn btn--danger" onClick={removeService} disabled={busy}>
                                            <IcoTrash />
                                            {doing === 'remove-service' ? 'Удаление…' : 'Удалить прокси'}
                                        </button>
                                    </div>
                                )}
                            </div>
                        )}

                        {changed && (
                            <div className="proxy-actions">
                                <button className="btn btn--tonal" onClick={up} disabled={busy || !portValid || !hostOk}>
                                    Применить
                                </button>
                            </div>
                        )}

                        {mst && (mst.error || link || linkReason) && (
                            <div className="ss-status">
                                {/* Ошибку показываем всегда, когда она есть: «бинарник стоит, сервис упал» —
                                    ровно тот случай, который раньше скрывался. */}
                                {mst.error && (
                                    <span className="st-hint" style={{ color: 'var(--error)' }}>{mst.error}</span>
                                )}
                                {link ? (
                                    <>
                                        <span className="ss-mono">{link}</span>
                                        <button className="btn" disabled={busy}
                                            onClick={() => copyText(link).then(() => showMsg('Ссылка скопирована'))}>
                                            Копировать
                                        </button>
                                        <button className="btn" disabled={busy} title="Показать QR-код" onClick={showQR}>
                                            <IcoQR /> QR
                                        </button>
                                    </>
                                ) : linkReason ? (
                                    <span className="st-hint">{linkReason}</span>
                                ) : null}
                            </div>
                        )}

                        {fwWarn && (
                            <p className="st-hint" role="note" style={{ color: 'var(--tertiary)' }}>
                                Порт не открыт в фаерволе — клиенты не подключатся
                                {fw?.error ? `: ${fw.error}` : ''}
                            </p>
                        )}

                        {error && <p className="login-error">{error}</p>}
                    </>
                )}
            </section>

            {qr && (
                <div className="qr-backdrop" onClick={() => setQr(null)}>
                    <div className="qr-card" role="dialog" aria-modal="true" aria-labelledby="proxy-qr-title"
                        onClick={e => e.stopPropagation()}>
                        <p className="qr-name" id="proxy-qr-title">Telegram-прокси</p>
                        <img className="qr-img" src={qr.dataUrl} alt="QR-код ссылки на прокси"
                            title="Нажми, чтобы скопировать ссылку"
                            onClick={() => copyText(qr.link).then(() => showMsg('Ссылка скопирована'))} />
                        <p className="qr-hint">Отсканируй в Telegram · нажми на QR, чтобы скопировать ссылку</p>
                        <div className="qr-actions">
                            <button className="btn btn--text" onClick={() => setQr(null)}>Закрыть</button>
                            <button className="btn btn--tonal"
                                onClick={() => copyText(qr.link).then(() => showMsg('Ссылка скопирована'))}>
                                Копировать ссылку
                            </button>
                        </div>
                    </div>
                </div>
            )}
        </div>
    );
}
