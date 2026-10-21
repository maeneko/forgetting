// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback } from 'react';
import { apiFetch, copyText, type PageProps } from '../../lib/shared';
import { IcoCopy } from '../../components/icons';
import './migrate.css';

// Вкладка «Перенос»: панель уезжает с сервера «панель + VPN» на отдельную панель без VPN, а
// старый сервер остаётся её нодой #0. Новая панель выдаёт одноразовый код, старая по нему сама
// отправляет туда базу и ключи подписки — у пользователей не меняется ничего. Логика — в
// internal/ui/migrate.go (/ui/migrate*), порядок — docs/node-protocol.md, «Перенос панели».
type MoveState = '' | 'sending' | 'moved' | 'uncertain' | 'imported';
// Новая панель после приёма: кто уже знает её адрес — тогда старый сервер можно выключать.
interface Retire {
    since:        number;
    home:         string | null;                     // адрес хаба для нод; null — не задан SUB_HOST
    old:          { exists: boolean; online: boolean };
    devices:      { total: number; fresh: number };  // fresh — получили конфиг после переноса
    unused_links: number;                            // мастер-ключи без устройств: в ссылке старый адрес
    nodes:        { id: number; name: string; online: boolean; ok: boolean }[];
}
interface MigrateInfo {
    state:   MoveState;
    error:   string | null;
    target:  string | null;                          // старая: адрес новой панели
    code:    { code: string; expires_in: number } | null;
    export?: string | null;                          // null — можно, строка — почему нельзя
    accept?: string | null;
    retire?: Retire | null;
}

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

export default function MigratePage({ token, showMsg }: PageProps) {
    const [info, setInfo] = useState<MigrateInfo | null>(null);
    const [code, setCode] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    const load = useCallback(async () => {
        try { setInfo(await apiFetch('GET', '/ui/migrate', token)); }
        catch { /* панель перезапускается — следующий опрос покажет */ }
    }, [token]);

    useEffect(() => { if (token) load(); }, [token, load]);

    // Пока висит код — ждём, когда старая панель пришлёт архив; после приёма панель
    // перезапускается, и страница перечитывается уже с перенесённой базой.
    const waiting = !!info?.code || info?.state === 'imported' || !!info?.retire;
    useEffect(() => {
        if (!waiting) return;
        const t = setInterval(load, 3000);
        return () => clearInterval(t);
    }, [waiting, load]);
    useEffect(() => {
        if (info?.state !== 'imported' || info.error) return;
        const t = setTimeout(() => window.location.reload(), 8000);
        return () => clearTimeout(t);
    }, [info?.state, info?.error]);

    const call = async (method: string, path: string, body?: object) => {
        setBusy(true); setError('');
        try { setInfo(await apiFetch(method, `/ui/migrate${path}`, token, body)); }
        catch (e) { setError(errText(e)); }
        finally { setBusy(false); }
    };
    const send = async () => {
        if (!window.confirm('Перенести панель? Здесь она остановится, а этот сервер станет нодой новой панели.')) return;
        setBusy(true); setError('');
        try {
            await apiFetch('POST', '/ui/migrate/export', token, { code: code.trim() });
            await load();
        } catch (e) { setError(errText(e)); await load(); }
        finally { setBusy(false); }
    };

    if (!info) return null;

    if (info.state === 'moved') return (
        <section className="mv-box">
            <span className="chip chip--online mv-self"><span className="chip-dot" />Панель перенесена</span>
            <p className="mv-text">
                Новая панель: <a href={info.target ?? '#'}>{info.target}</a>. Через несколько секунд этот сервер станет её
                нодой #0 и будет пересылать туда прежние порты: ссылки sen://, ноды и внешний API продолжат работать,
                а этот адрес откроет уже новую панель. Вход — логином и паролем новой панели.
            </p>
            {info.target && <a className="btn btn--primary mv-self" href={info.target}>Открыть новую панель</a>}
        </section>
    );

    if (info.state === 'imported') return (
        <section className="mv-box">
            <span className={`chip chip--${info.error ? 'error' : 'online'} mv-self`}><span className="chip-dot" />
                {info.error ? 'Приём прерван' : 'Панель принята'}</span>
            <p className="mv-text">
                {info.error ?? 'Панель перезапускается с перенесённой базой — страница обновится сама. Старый сервер появится среди серверов как «#0», когда подключится.'}
            </p>
        </section>
    );

    const r = info.retire;
    const retireBox = r && (() => {
        const devicesOk = r.devices.fresh >= r.devices.total;
        const nodesOk = r.nodes.every(n => n.ok);
        const ready = devicesOk && nodesOk && !!r.home;
        const chip = (ok: boolean, text: string, offline = false) => (
            <span className={`chip chip--${ok ? 'online' : offline ? 'offline' : 'warm'}`}><span className="chip-dot" />{text}</span>
        );
        return (
            <section className="mv-box" key="retire">
                <h4 className="mv-title">Отключение старого сервера</h4>
                <p className="mv-text">
                    {r.old.exists
                        ? <>Старый сервер — здесь это сервер «#0» — пока пересылает сюда запросы по старым адресам. Выключать его
                            можно, когда новый адрес панели узнают все клиенты и ноды: мастер-ключи при этом остаются прежними.</>
                        : <>Сервер «#0» удалён из панели. Если старая машина ещё работает, её можно выключать.</>}
                </p>
                <div className="mv-checks">
                    <div className="mv-check">
                        {chip(devicesOk, `${r.devices.fresh} из ${r.devices.total}`)}
                        <span>устройств получили новый адрес{!devicesOk && ' — остальные получат при следующем обновлении подписки'}</span>
                    </div>
                    {r.nodes.map(n => (
                        <div className="mv-check" key={n.id}>
                            {chip(n.ok, n.ok ? 'знает адрес' : n.online ? 'ещё нет' : 'не в сети', !n.ok && !n.online)}
                            <span>нода «{n.name}»{!n.ok && n.online && ' — обновите на ней Forgetting'}</span>
                        </div>
                    ))}
                    {r.unused_links > 0 && (
                        <div className="mv-check">
                            {chip(false, String(r.unused_links), true)}
                            <span>мастер-ключей ещё ни разу не открыты: в выданных ссылках старый адрес. Скопируйте их ссылки на
                                вкладке «Мастер-ключи» заново — новые уже с адресом этой панели, ключ тот же</span>
                        </div>
                    )}
                </div>
                {!r.home && <p className="mv-notice">Не задан SUB_HOST — ноды не узнают адрес панели. Задайте его в cli.env и перезапустите панель.</p>}
                <ol className="mv-steps">
                    <li>Новый VPN-сервер: «Добавить сервер», затем отметьте его в мастер-ключах — клиенты получат его в конфиге сами.</li>
                    <li>Дождитесь, пока всё выше станет зелёным{ready && ' — уже'}.</li>
                    <li>Удалите сервер «#0» и выключите старую машину.</li>
                </ol>
                {!r.old.exists && (
                    <button className="btn btn--outline mv-self" disabled={busy} onClick={() => call('DELETE', '/retire')}>Готово, скрыть</button>
                )}
            </section>
        );
    })();

    const canExport = info.export === null, canAccept = info.accept === null;
    const exportBox = (
        <section className="mv-box" key="export">
            <h4 className="mv-title">Перенести эту панель на другой сервер</h4>
            {canExport ? (
                <>
                    <p className="mv-text">
                        Панель уедет на отдельный сервер без VPN, а этот сервер останется VPN — нодой новой панели.
                        Пользователям ничего менять не нужно: ссылки sen:// и vpn://, ключи и конфиги остаются прежними.
                    </p>
                    <ol className="mv-steps">
                        <li>На новом сервере установите Forgetting с ролью «только панель».</li>
                        <li>Там на вкладке «Перенос» нажмите «Получить код».</li>
                        <li>Вставьте код сюда и нажмите «Перенести».</li>
                    </ol>
                    {info.state === 'uncertain' ? (
                        <>
                            <p className="mv-notice">{info.error}</p>
                            <button className="btn btn--outline mv-self" disabled={busy} onClick={() => call('POST', '/cancel')}>
                                Отменить перенос
                            </button>
                        </>
                    ) : (
                        <div className="mv-row">
                            <input className="field" placeholder="awgmove://…" value={code} disabled={busy}
                                onChange={e => setCode(e.target.value)} />
                            <button className="btn btn--primary" disabled={busy || !code.trim().startsWith('awgmove://')} onClick={send}>
                                {busy ? 'Переносим…' : 'Перенести'}
                            </button>
                        </div>
                    )}
                </>
            ) : <p className="mv-text mv-muted">{info.export}</p>}
        </section>
    );
    const acceptBox = (
        <section className="mv-box" key="accept">
            <h4 className="mv-title">Принять панель с другого сервера</h4>
            {canAccept ? (
                info.code ? (
                    <>
                        <p className="mv-text">
                            Вставьте код на вкладке «Перенос» старой панели. Он одноразовый и действует
                            ещё {Math.max(1, Math.round(info.code.expires_in / 60))} мин. Архив придёт зашифрованным ключом из кода.
                        </p>
                        <code className="node-modal-cmd">{info.code.code}</code>
                        <div className="mv-row mv-row--end">
                            <button className="btn btn--tonal" onClick={() => copyText(info.code!.code).then(() => showMsg('Код скопирован'))}>
                                <IcoCopy /> Скопировать
                            </button>
                            <button className="btn btn--outline" disabled={busy} onClick={() => call('DELETE', '/code')}>Отозвать</button>
                        </div>
                        <p className="mv-text mv-muted">Ждём архив от старой панели…</p>
                    </>
                ) : (
                    <>
                        <p className="mv-text">
                            Эта панель примет базу, ключи подписки и серверы старой, а старый сервер подключится к ней
                            нодой #0. Логин и пароль останутся этой панели.
                        </p>
                        <button className="btn btn--primary mv-self" disabled={busy}
                            onClick={() => call('POST', '/code', { host: window.location.hostname })}>
                            Получить код
                        </button>
                    </>
                )
            ) : <p className="mv-text mv-muted">{info.accept}</p>}
        </section>
    );

    return (
        <>
            {/* Доступное действие — первым. */}
            {retireBox}
            {canAccept && !canExport ? [acceptBox, exportBox] : [exportBox, acceptBox]}
            {error && <p className="mv-notice">{error}</p>}
        </>
    );
}
