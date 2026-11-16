// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type PageProps, type MTProtoStatus } from '../../lib/shared';
import { qrDataUrl } from '../../lib/qr';
import { IcoPlus, IcoPower, IcoQR, IcoTrash } from '../../components/icons';
import './proxy.css';

// Telegram-прокси (MTProto, mtg) на сервере из шапки. Панель поднимает его через awg-ctrl ноды
// (обычный /api-прокси с X-Server-Id, при смене сервера вкладка перемонтируется): awg-ctrl пишет
// конфиг и systemd-юнит, запускает сервис и отдаёт ссылку t.me/proxy?… Секрет и порт переживают
// перезапуски; «Выключить» их сохраняет — включение заново вернёт ту же ссылку.
export default function ProxyPage({ token, showMsg }: PageProps) {
    const [mst, setMst]          = useState<MTProtoStatus | null>(null);
    const [port, setPort]        = useState('');
    const [doing, setDoing]      = useState('');
    const [error, setError]      = useState('');
    const [unsupported, setUnsupported] = useState(false);
    const [qr, setQr]            = useState<{ dataUrl: string; link: string } | null>(null);

    const errText = (e: unknown, fallback: string) =>
        axios.isAxiosError(e) ? e.response?.data?.error ?? fallback : fallback;

    const fill = (s: MTProtoStatus) => {
        setMst(s);
        setPort(s.port ? String(s.port) : '443');
    };

    useEffect(() => {
        let alive = true;
        apiFetch('GET', '/api/mtproto', token)
            .then(s => { if (alive) fill(s as MTProtoStatus); })
            .catch(e => {
                if (!alive) return;
                if (axios.isAxiosError(e) && e.response?.status === 404) setUnsupported(true);
                else setError(errText(e, 'Не удалось получить статус прокси'));
            });
        return () => { alive = false; };
    }, [token]);

    const act = async (fn: () => Promise<MTProtoStatus>, done: string, action: string) => {
        setDoing(action); setError('');
        try {
            fill(await fn());
            showMsg(done);
        } catch (e) { setError(errText(e, 'Не удалось выполнить действие')); }
        finally { setDoing(''); }
    };

    const up = () => act(
        () => apiFetch('POST', '/api/mtproto', token, { port: Number(port) || 443 }) as Promise<MTProtoStatus>,
        'Прокси поднят', 'up');
    const rotate = () => act(
        () => apiFetch('POST', '/api/mtproto/rotate', token) as Promise<MTProtoStatus>,
        'Секрет изменён — старая ссылка больше не работает', 'rotate');
    const stop = () => act(
        () => apiFetch('DELETE', '/api/mtproto', token) as Promise<MTProtoStatus>,
        'Прокси остановлен', 'stop');
    const installMtg = () => act(
        () => apiFetch('POST', '/api/mtproto/mtg', token) as Promise<MTProtoStatus>,
        'Компонент прокси установлен', 'install-mtg');
    const removeMtg = () => {
        if (!confirm('Удалить компонент прокси? Прокси будет остановлен, компонент удалён. Порт и секрет останутся — после установки прокси можно включить снова.')) return;
        void act(() => apiFetch('DELETE', '/api/mtproto/mtg', token) as Promise<MTProtoStatus>,
            'Компонент прокси удалён', 'remove-mtg');
    };
    const removeService = () => {
        if (!confirm('Удалить прокси? Сервис, конфиг, порт и секрет будут удалены, ссылка перестанет работать. Компонент прокси останется.')) return;
        void act(() => apiFetch('DELETE', '/api/mtproto/service', token) as Promise<MTProtoStatus>,
            'Прокси удалён', 'remove-service');
    };
    const showQR = () => {
        if (!mst?.link) return;
        try { setQr({ dataUrl: qrDataUrl(mst.link, 'H'), link: mst.link }); }
        catch { showMsg('Не удалось сгенерировать QR-код'); }
    };

    const portN = Number(port);
    const portValid = Number.isInteger(portN) && portN >= 1 && portN <= 65535;
    const installed = !!mst && mst.port > 0;
    const running = !!mst?.running;
    const busy = doing !== '';
    const stateLabel = !mst ? 'проверка…' : running ? 'работает' : installed ? 'остановлен' : 'не установлен';
    const powerLabel = running ? (doing === 'stop' ? 'Выключение…' : 'Выключить')
        : installed ? (doing === 'up' ? 'Включение…' : 'Включить')
        : (doing === 'up' ? 'Установка…' : 'Установить');

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
                                <button className="btn" onClick={rotate} disabled={busy}>
                                    {doing === 'rotate' ? 'Меняем…' : 'Сменить секрет'}
                                </button>
                            )}
                            {!!mst?.binary && (
                                <button
                                    className={`btn ${running ? 'btn--outline' : 'btn--primary'}`}
                                    onClick={() => { if (!mst) return; if (running) void stop(); else void up(); }}
                                    disabled={!mst || busy || (!running && !portValid)}
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
                        Этот сервер не умеет поднимать Telegram-прокси: обновите его (install.sh → «Обновить»).
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

                        {mst?.running && portN !== mst.port && (
                            <div className="proxy-actions">
                                <button className="btn btn--tonal" onClick={up} disabled={busy || !portValid}>
                                    Применить порт
                                </button>
                            </div>
                        )}

                        {mst && ((mst.error && mst.binary) || mst.link) && (
                            <div className="ss-status">
                                {mst.error && mst.binary && <span className="st-hint">{mst.error}</span>}
                                {mst.link && (
                                    <>
                                        <span className="ss-mono">{mst.link}</span>
                                        <button className="btn" disabled={busy}
                                            onClick={() => copyText(mst.link!).then(() => showMsg('Ссылка скопирована'))}>
                                            Копировать
                                        </button>
                                        <button className="btn" disabled={busy} title="Показать QR-код" onClick={showQR}>
                                            <IcoQR /> QR
                                        </button>
                                    </>
                                )}
                            </div>
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
