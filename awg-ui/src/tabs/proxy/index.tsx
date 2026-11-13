// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type PageProps, type MTProtoStatus } from '../../lib/shared';
import './proxy.css';

// Telegram-прокси (MTProto, mtg) на сервере из шапки. Панель поднимает его через awg-ctrl ноды
// (обычный /api-прокси с X-Server-Id, при смене сервера вкладка перемонтируется): awg-ctrl пишет
// конфиг и systemd-юнит, запускает сервис и отдаёт ссылку t.me/proxy?… Секрет и порт переживают
// перезапуски; «Остановить» их сохраняет — подъём заново вернёт ту же ссылку.
export default function ProxyPage({ token, showMsg }: PageProps) {
    const [mst, setMst]          = useState<MTProtoStatus | null>(null);
    const [port, setPort]        = useState('');
    const [busy, setBusy]        = useState(false);
    const [error, setError]      = useState('');
    const [unsupported, setUnsupported] = useState(false);

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

    const act = async (fn: () => Promise<MTProtoStatus>, done: string) => {
        setBusy(true); setError('');
        try {
            fill(await fn());
            showMsg(done);
        } catch (e) { setError(errText(e, 'Не удалось выполнить действие')); }
        finally { setBusy(false); }
    };

    const up = () => act(
        () => apiFetch('POST', '/api/mtproto', token, { port: Number(port) || 443 }) as Promise<MTProtoStatus>,
        'Прокси поднят');
    const rotate = () => act(
        () => apiFetch('POST', '/api/mtproto/rotate', token) as Promise<MTProtoStatus>,
        'Секрет изменён — старая ссылка больше не работает');
    const stop = () => act(
        () => apiFetch('DELETE', '/api/mtproto', token) as Promise<MTProtoStatus>,
        'Прокси остановлен');

    if (unsupported) {
        return (
            <section className="proxy-card">
                <h3 className="ss-section">Telegram-прокси</h3>
                <p className="st-hint">
                    Этот сервер не умеет поднимать Telegram-прокси: обновите его (install.sh → «Обновить»).
                </p>
            </section>
        );
    }

    const portN = Number(port);
    const portValid = Number.isInteger(portN) && portN >= 1 && portN <= 65535;
    const noBinary = !!mst && mst.port > 0 && !mst.binary;

    return (
        <section className="proxy-card">
            <h3 className="ss-section">Telegram-прокси</h3>
            <p className="st-hint">
                Поднимает MTProto-прокси (mtg) прямо на этом сервере — для доступа к Telegram там, где он
                заблокирован. Пользователям раздаётся ссылка: они нажимают её, и Telegram подключается сам.
                Прокси работает независимо от VPN и канал не делит.
            </p>

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

            <div className="proxy-actions">
                <button className="btn btn--primary" onClick={up} disabled={busy || !portValid}>
                    {mst?.port ? 'Применить' : 'Поднять прокси'}
                </button>
                {!!mst?.port && (
                    <>
                        <button className="btn" onClick={rotate} disabled={busy}>Сменить секрет</button>
                        <button className="btn" onClick={stop} disabled={busy}>Остановить</button>
                    </>
                )}
            </div>

            {mst && (
                <div className="ss-status">
                    <span className={`chip ${mst.running ? 'chip--online' : 'chip--offline'}`}>
                        <span className="chip-dot" />
                        {mst.running ? 'работает' : mst.port ? 'остановлен' : 'не поднят'}
                    </span>
                    {noBinary && (
                        <span className="st-hint">
                            Бинарник mtg не найден на сервере — установите его и повторите. Скачайте с
                            github.com/9seconds/mtg и положите в /usr/local/bin/mtg.
                        </span>
                    )}
                    {mst.error && <span className="st-hint">{mst.error}</span>}
                    {mst.link && (
                        <>
                            <span className="ss-mono">{mst.link}</span>
                            <button className="btn" disabled={busy}
                                onClick={() => copyText(mst.link!).then(() => showMsg('Ссылка скопирована'))}>
                                Копировать
                            </button>
                        </>
                    )}
                </div>
            )}

            {error && <p className="login-error">{error}</p>}
        </section>
    );
}
