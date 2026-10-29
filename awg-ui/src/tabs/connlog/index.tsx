// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback, useMemo } from 'react';
import { apiFetch, bytes, timeAgo, type PageProps } from '../../lib/shared';
import { IcoCheck, IcoRefresh } from '../../components/icons';
import './connlog.css';

// Вкладка «Подключения»: журнал VPN выбранного сервера. Ведёт его awg-ctrl (internal/ctrl/connlog.go)
// по handshake и счётчикам WireGuard раз в 30 с, панель подставляет имена ключей и устройств sen://
// (/ui/connlog). Время отключения — последняя активность, точность около 30 с.
interface Who { pub_key: string; name: string; owner: string; ip: string; key?: string | null; device?: string | null; foreign?: boolean }
interface Ev extends Who { id: number; at: number; event: 'connect' | 'disconnect'; duration?: number | null; rx?: number | null; tx?: number | null }
interface Peer extends Who { online: boolean; last_seen: number | null }
interface Log { events: Ev[]; peers: Peer[]; since: number | null; keep_days: number }

const errText = (e: any): string => e?.response?.data?.error ?? 'Не удалось получить журнал';

// Кто это: пользователь — по имени, устройство sen:// — ключ и устройство.
function who(w: Who): { title: string; sub: string } {
    if (w.name) return { title: w.name, sub: 'пользователь' };
    if (w.foreign) return { title: 'Устройство другой панели', sub: w.owner };
    if (w.owner) {
        return { title: w.device || 'Удалённое устройство', sub: w.key ? `мастер-ключ «${w.key}»` : 'мастер-ключ удалён' };
    }
    return { title: `ключ ${w.pub_key.slice(0, 10)}…`, sub: 'не из панели' };
}

const pad = (n: number) => String(n).padStart(2, '0');
function when(ts: number): string {
    const d = new Date(ts * 1000), now = new Date();
    const time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    if (d.toDateString() === now.toDateString()) return `сегодня ${time}`;
    const y = new Date(now); y.setDate(now.getDate() - 1);
    if (d.toDateString() === y.toDateString()) return `вчера ${time}`;
    return `${pad(d.getDate())}.${pad(d.getMonth() + 1)}${d.getFullYear() === now.getFullYear() ? '' : '.' + d.getFullYear()} ${time}`;
}
function duration(s: number): string {
    if (s < 60) return `${s} сек`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m} мин`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h} ч ${m % 60} мин`;
    return `${Math.floor(h / 24)} дн ${h % 24} ч`;
}

export default function ConnLogPage({ token, showMsg }: PageProps) {
    const [view, setView]     = useState<'events' | 'peers'>('events');
    const [log, setLog]       = useState<Log | null>(null);
    const [more, setMore]     = useState<Ev[]>([]);     // подгруженные страницы старее первой
    const [done, setDone]     = useState(false);        // старее событий нет
    const [err, setErr]       = useState('');
    const [busy, setBusy]     = useState(false);
    const [query, setQuery]   = useState('');

    // Первая страница: обновляем раз в 30 с — журнал пишется с тем же шагом.
    const load = useCallback(async () => {
        try {
            const l = await apiFetch('GET', '/ui/connlog?limit=200', token) as Log;
            setLog(l); setErr('');
            if (l.events.length < 200) setDone(true);
        } catch (e) { setErr(errText(e)); }
    }, [token]);
    useEffect(() => {
        void load();
        const t = setInterval(() => { void load(); }, 30000);
        return () => clearInterval(t);
    }, [load]);

    const all = useMemo(() => {
        const first = log?.events ?? [];
        const seen = new Set(first.map(e => e.id));
        return [...first, ...more.filter(e => !seen.has(e.id))];
    }, [log, more]);

    const loadMore = async () => {
        const last = all[all.length - 1];
        if (!last) return;
        setBusy(true);
        try {
            const l = await apiFetch('GET', `/ui/connlog?limit=200&before=${last.id}`, token) as Log;
            setMore(m => [...m, ...l.events]);
            if (l.events.length < 200) setDone(true);
        } catch (e) { showMsg(errText(e)); }
        finally { setBusy(false); }
    };

    const q = query.trim().toLowerCase();
    const match = (w: Who) => !q || [w.name, w.owner, w.ip, w.key ?? '', w.device ?? '', w.pub_key]
        .some(s => s.toLowerCase().includes(q));

    const events = all.filter(match);
    const peers = (log?.peers ?? []).filter(match).sort((a, b) =>
        Number(b.online) - Number(a.online) || (b.last_seen ?? 0) - (a.last_seen ?? 0));
    const onlineNow = (log?.peers ?? []).filter(p => p.online).length;

    return (
        <div className="cl">
            <div className="cl-toolbar">
                <div className="segmented cl-views" role="radiogroup" aria-label="Вид">
                    <button type="button" role="radio" aria-checked={view === 'events'} className="segmented-item"
                        onClick={() => setView('events')}>{view === 'events' && <IcoCheck />}События</button>
                    <button type="button" role="radio" aria-checked={view === 'peers'} className="segmented-item"
                        onClick={() => setView('peers')}>{view === 'peers' && <IcoCheck />}Кто когда был</button>
                </div>
                <input className="field cl-search" type="search" placeholder="Имя, ключ, устройство или IP"
                    aria-label="Поиск" value={query} onChange={e => setQuery(e.target.value)} />
                <button className="btn btn--tonal" onClick={() => void load()} aria-label="Обновить"><IcoRefresh /> Обновить</button>
            </div>

            {err && <p className="login-error cl-err">{err}</p>}
            {!log && !err && <p className="cl-hint">Загрузка…</p>}

            {log && (
                <p className="cl-hint">
                    Сейчас онлайн: <b>{onlineNow}</b> из {log.peers.length}.
                    {' '}{log.since ? <>Журнал ведётся с {when(log.since)}, хранится {log.keep_days} дней.</> : 'Журнал пуст: события появятся при первом подключении.'}
                    {' '}Время отключения — последняя активность, точность около 30 секунд.
                </p>
            )}

            {log && view === 'events' && (
                <ul className="cl-list">
                    {events.length === 0 && <li className="cl-empty">{q ? 'Ничего не найдено' : 'Событий пока нет'}</li>}
                    {events.map(e => {
                        const w = who(e);
                        const on = e.event === 'connect';
                        return (
                            <li className="cl-row" key={e.id}>
                                <span className="cl-time">{when(e.at)}</span>
                                <span className={`chip chip--${on ? 'online' : 'offline'} cl-chip`}>
                                    <span className="chip-dot" />{on ? 'подключился' : 'отключился'}
                                </span>
                                <span className="cl-who">
                                    <b>{w.title}</b>
                                    <span className="cl-sub">{w.sub}{e.ip && <> · {e.ip}</>}</span>
                                </span>
                                {!on && (
                                    <span className="cl-session">
                                        {e.duration != null && <>сессия {duration(e.duration)}</>}
                                        {/* rx/tx — со стороны сервера: то, что сервер принял, клиент отправил */}
                                        {(e.tx || e.rx) ? <> · ↓ {bytes(e.tx ?? 0)} ↑ {bytes(e.rx ?? 0)}</> : null}
                                    </span>
                                )}
                            </li>
                        );
                    })}
                    {!done && !q && all.length > 0 && (
                        <li className="cl-more">
                            <button className="btn btn--tonal" onClick={() => void loadMore()} disabled={busy}>
                                {busy ? 'Загрузка…' : 'Показать раньше'}
                            </button>
                        </li>
                    )}
                </ul>
            )}

            {log && view === 'peers' && (
                <ul className="cl-list">
                    {peers.length === 0 && <li className="cl-empty">{q ? 'Ничего не найдено' : 'На сервере нет пользователей'}</li>}
                    {peers.map(p => {
                        const w = who(p);
                        return (
                            <li className="cl-row" key={p.pub_key}>
                                <span className="cl-who">
                                    <b>{w.title}</b>
                                    <span className="cl-sub">{w.sub}{p.ip && <> · {p.ip}</>}</span>
                                </span>
                                {p.online ? (
                                    <span className="chip chip--online cl-chip"><span className="chip-dot" />онлайн</span>
                                ) : p.last_seen ? (
                                    <span className="cl-seen">был {when(p.last_seen)} <span className="cl-sub">({timeAgo(p.last_seen)} назад)</span></span>
                                ) : (
                                    <span className="chip chip--offline cl-chip"><span className="chip-dot" />не подключался{log.since ? ` с ${when(log.since)}` : ''}</span>
                                )}
                            </li>
                        );
                    })}
                </ul>
            )}
        </div>
    );
}
