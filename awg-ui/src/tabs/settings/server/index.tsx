// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type PageProps, type MTProtoStatus, type ShapingStatus, type TorrentStatus } from '../../../lib/shared';
import './server.css';

// Раздел «Сервер» вкладки «Настройки»: деление канала между устройствами (CAKE) и блокировка BitTorrent
// для сервера из шапки (при его смене вкладка перемонтируется). Сами настройки живут в awg-ctrl этого
// сервера — запрос идёт обычным прокси с X-Server-Id, так что ядро и ноды настраиваются одинаково.
// Ниже — Telegram-прокси (MTProto): карточка с собственным жизненным циклом, кнопкой «Сохранить»
// шейпинга она не связана.
export default function ServerSettings({ token, showMsg }: PageProps) {
    const [st, setSt]           = useState<ShapingStatus | null>(null);
    const [enabled, setEnabled] = useState(false);
    const [auto, setAuto]       = useState(true);
    const [down, setDown]       = useState('');
    const [up, setUp]           = useState('');
    const [busy, setBusy]       = useState(false);
    const [error, setError]     = useState('');
    const [unsupported, setUnsupported] = useState(false);
    const [tst, setTst]           = useState<TorrentStatus | null>(null);
    const [tEnabled, setTEnabled] = useState(false);
    const [tUnsupported, setTUnsupported] = useState(false);

    const fill = (s: ShapingStatus) => {
        setSt(s);
        setEnabled(s.settings.enabled); setAuto(s.settings.auto);
        setDown(s.settings.downMbit ? String(s.settings.downMbit) : '');
        setUp(s.settings.upMbit ? String(s.settings.upMbit) : '');
    };

    const errText = (e: unknown, fallback: string) =>
        axios.isAxiosError(e) ? e.response?.data?.error ?? fallback : fallback;

    const fillTorrent = (s: TorrentStatus) => { setTst(s); setTEnabled(s.settings.enabled); };

    useEffect(() => {
        let alive = true;
        apiFetch('GET', '/api/torrent', token)
            .then(s => { if (alive) fillTorrent(s as TorrentStatus); })
            .catch(e => {
                if (!alive) return;
                if (axios.isAxiosError(e) && e.response?.status === 404) setTUnsupported(true);
                else setError(errText(e, 'Не удалось загрузить настройки'));
            });
        return () => { alive = false; };
    }, [token]);

    useEffect(() => {
        let alive = true;
        apiFetch('GET', '/api/shaping', token)
            .then(s => { if (alive) fill(s as ShapingStatus); })
            .catch(e => {
                if (!alive) return;
                // Старый awg-ctrl не знает этот маршрут — его надо обновить, а не «починить» здесь.
                if (axios.isAxiosError(e) && e.response?.status === 404) setUnsupported(true);
                else setError(errText(e, 'Не удалось загрузить настройки'));
            });
        return () => { alive = false; };
    }, [token]);

    const downN = Number(down), upN = Number(up);
    const valid = !enabled || (Number.isInteger(downN) && downN >= 1 && Number.isInteger(upN) && upN >= 1);

    // Отправляем только изменённое: лишний PUT /api/shaping заново ставил бы tc на интерфейс.
    const shapingDirty = !!st && (enabled !== st.settings.enabled || auto !== st.settings.auto
        || (Number(down) || 0) !== st.settings.downMbit || (Number(up) || 0) !== st.settings.upMbit);
    const torrentDirty = !!tst && tEnabled !== tst.settings.enabled;

    const save = async () => {
        setBusy(true); setError('');
        const done: string[] = [];
        try {
            if (shapingDirty) {
                const s = await apiFetch('PUT', '/api/shaping', token, {
                    enabled, auto, downMbit: Number(down) || 0, upMbit: Number(up) || 0,
                });
                fill(s as ShapingStatus);
                done.push(enabled ? 'деление канала включено' : 'деление канала выключено');
            }
            if (torrentDirty) {
                const s = await apiFetch('PUT', '/api/torrent', token, { enabled: tEnabled });
                fillTorrent(s as TorrentStatus);
                done.push(tEnabled ? 'торренты блокируются' : 'блокировка торрентов выключена');
            }
            if (done.length) showMsg(done.join(', ').replace(/^./, c => c.toUpperCase()));
        } catch (e) { setError(errText(e, 'Не удалось сохранить')); }
        finally { setBusy(false); }
    };

    return (
        <>
            <section className="st-section ss-card">
                <h3 className="ss-section">Деление канала</h3>

                {unsupported ? (
                    <p className="st-hint">
                        Этот сервер не поддерживает деление канала: обновите его (install.sh → «Обновить»).
                    </p>
                ) : (
                    <>
                        <p className="st-hint">
                            Канал делится поровну между устройствами, но не простаивает: кто берёт мало, получает всё
                            нужное, остальное достаётся тем, кто качает много. Звонки и игры не ждут за чужими закачками.
                        </p>

                        <label className="ss-setting">
                            <span className="ss-setting-text">
                                <span className="ss-setting-name">Честное деление между устройствами</span>
                            </span>
                            <input type="checkbox" checked={enabled} disabled={!st || busy} onChange={e => setEnabled(e.target.checked)} />
                        </label>

                        <div className="ss-grid">
                            <label className="ss-field">
                                <span className="ss-setting-name">Загрузка, Мбит/с</span>
                                <input className="field" type="number" inputMode="numeric" min={1} max={100000}
                                    value={down} disabled={!st || busy} onChange={e => setDown(e.target.value)} />
                            </label>
                            <label className="ss-field">
                                <span className="ss-setting-name">Отдача, Мбит/с</span>
                                <input className="field" type="number" inputMode="numeric" min={1} max={100000}
                                    value={up} disabled={!st || busy} onChange={e => setUp(e.target.value)} />
                            </label>
                        </div>
                        <p className="st-hint">
                            Укажите около 90–95% реального канала сервера. Если задать больше, очередь копится у провайдера
                            и деление перестаёт работать.
                        </p>

                        <label className="ss-setting">
                            <span className="ss-setting-text">
                                <span className="ss-setting-name">Снижать приоритет тяжёлых</span>
                                <span className="st-hint">
                                    Когда канал занят, устройства, которые долго качают заметно больше своей доли, временно
                                    уходят в фоновый класс. Работает на загрузку; свободный канал они по-прежнему занимают.
                                </span>
                            </span>
                            <input type="checkbox" checked={auto} disabled={!st || busy || !enabled} onChange={e => setAuto(e.target.checked)} />
                        </label>

                        {st?.settings.enabled && (
                            <div className="ss-status">
                                <span className={`chip ${st.active ? (st.congested ? 'chip--warm' : 'chip--online') : 'chip--offline'}`}>
                                    <span className="chip-dot" />
                                    {!st.active ? 'не применено' : st.congested ? 'канал занят' : 'канал свободен'}
                                </span>
                                <span className="ss-mono">
                                    ↓ {st.rateDown} · ↑ {st.rateUp} Мбит/с · устройств {st.devices}
                                </span>
                                {st.heavy.length > 0 && (
                                    <span className="ss-mono">в фоне: {st.heavy.join(', ')}</span>
                                )}
                            </div>
                        )}
                    </>
                )}

            </section>

            <section className="st-section ss-card">
                <h3 className="ss-section">Торренты</h3>
                {tUnsupported ? (
                    <p className="st-hint">
                        Этот сервер не умеет блокировать торренты: обновите его (install.sh → «Обновить»).
                    </p>
                ) : (
                    <>
                        <label className="ss-setting">
                            <span className="ss-setting-text">
                                <span className="ss-setting-name">Блокировать BitTorrent</span>
                                <span className="st-hint">
                                    Режет поиск пиров (DHT), трекеры и начало торрент-соединений — как блокировка в 3x-ui.
                                    Клиент с шифрованием и HTTPS-трекерами может частично её обойти, но без DHT и открытых
                                    трекеров большинство торрентов не стартуют. Остальной трафик не режется.
                                </span>
                                <span className="st-hint">
                                    {enabled
                                        ? 'Кто пытается качать, на 10 минут после последней попытки уходит в фоновый приоритет: уступает остальным, когда канал занят.'
                                        : 'Включите деление канала — тогда кто пытается качать, на 10 минут уйдёт ещё и в фоновый приоритет.'}
                                </span>
                            </span>
                            <input type="checkbox" checked={tEnabled} disabled={!tst || busy} onChange={e => setTEnabled(e.target.checked)} />
                        </label>

                        {tst?.settings.enabled && (
                            <div className="ss-status">
                                <span className={`chip ${tst.active ? 'chip--online' : 'chip--offline'}`}>
                                    <span className="chip-dot" />{tst.active ? 'работает' : 'не применено'}
                                </span>
                                <span className="ss-mono">заблокировано попыток: {tst.hits.toLocaleString('ru-RU')}</span>
                                {tst.active && !tst.tcp && (
                                    <span className="st-hint">
                                        TCP не проверяется: на сервере старый nftables. DHT, трекеры и uTP блокируются.
                                    </span>
                                )}
                                {tst.error && <span className="st-hint">{tst.error}</span>}
                                {tst.seen.length > 0 && (
                                    <>
                                        <span className="st-hint">Пытались качать за последние 10 минут:</span>
                                        <span className="ss-seen">
                                            {tst.seen.map(p => (
                                                <span key={p.ip} className="chip chip--error">
                                                    <span className="chip-dot" />{p.name ? `${p.name} · ${p.ip}` : p.ip}
                                                </span>
                                            ))}
                                        </span>
                                    </>
                                )}
                            </div>
                        )}
                    </>
                )}

            </section>

            {error && <p className="login-error">{error}</p>}

            {!(unsupported && tUnsupported) && (
                <div className="ss-actions">
                    <button className="btn btn--primary" onClick={save} disabled={busy || !valid || !(shapingDirty || torrentDirty)}>
                        Сохранить
                    </button>
                </div>
            )}

            <MTProtoCard token={token} showMsg={showMsg} />
        </>
    );
}

// Telegram-прокси (MTProto, mtg) на этом сервере. Панель поднимает его через awg-ctrl ноды
// (тот же X-Server-Id-прокси): awg-ctrl пишет конфиг и systemd-юнит, запускает сервис и отдаёт
// ссылку t.me/proxy?… Секрет и порт переживают перезапуски; «Остановить» их сохраняет — подъём
// заново вернёт ту же ссылку. У карточки свой busy/error: с шейпингом она не связана.
function MTProtoCard({ token, showMsg }: PageProps) {
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
            <section className="st-section ss-card">
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
        <section className="st-section ss-card">
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

            <div className="ss-actions">
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
                            <span className="ss-mono ss-seen">{mst.link}</span>
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
