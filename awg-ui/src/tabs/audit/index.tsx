// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback } from 'react';
import { apiFetch, timeAgo, type NodeInfo, type PageProps } from '../../lib/shared';
import { IcoRefresh, IcoTrash } from '../../components/icons';
import { serverName } from '../../components/ServerPicker';
import './audit.css';

// Вкладка «Аудит»: core периодически обходит все серверы и ищет «мёртвых» пиров — живых на сервере,
// хотя панель про них не знает (мастер-ключ удалён, а пир остался). Снимает только подтверждённых
// (мёртвых два прохода подряд): автоматически, если включено в настройках, или кнопкой.
// Отчёт и очистку считает awg-ui/sub.ts (/ui/masterkeys/audit*).
interface AuditPeer { pub_key: string; owner?: string; key?: string | null; online: boolean; lastHandshake: number; seen_before?: boolean }
interface AuditEntry {
    server_id: number; status: 'ok' | 'offline' | 'error'; ghosts_checked: boolean;
    ghosts: AuditPeer[]; orphans: AuditPeer[]; foreign: number; blocked?: number;
}
interface Settings { clean: boolean; every_h: number; max: number }
interface Cleaned { at: number; server_id: number; pub_key: string; kind: 'ghost' | 'orphan'; key?: string | null; auto: boolean }
interface AuditData { enabled: boolean; settings: Settings; report: { at: number; servers: AuditEntry[] } | null; cleaned: Cleaned[] }

const PERIODS = [1, 3, 6, 12, 24];

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

export default function AuditPage({ token, showMsg }: PageProps) {
    const [audit, setAudit] = useState<AuditData | null>(null);
    const [nodes, setNodes] = useState<NodeInfo[]>([]);
    const [busy, setBusy]   = useState(false);
    const [maxEdit, setMaxEdit] = useState('');

    const load = useCallback(async () => {
        try {
            setAudit(await apiFetch('GET', '/ui/masterkeys/audit', token));
            apiFetch('GET', '/ui/nodes', token).then(r => setNodes(r.nodes ?? [])).catch(() => {});
        } catch { showMsg('Ошибка загрузки аудита'); }
    }, [token, showMsg]);

    useEffect(() => { if (token) load(); }, [token, load]);

    const call = useCallback(async (method: string, path: string, ok: string, body?: object) => {
        setBusy(true);
        try { setAudit(await apiFetch(method, `/ui/masterkeys/audit${path}`, token, body)); showMsg(ok); }
        catch (e) { showMsg(errText(e)); }
        finally { setBusy(false); }
    }, [token, showMsg]);
    const run = () => call('POST', '', 'Аудит выполнен');
    const save = (patch: Partial<Settings>) => call('PUT', '/settings', 'Настройки сохранены', patch);

    if (!audit) return null;
    if (!audit.enabled) return <p className="au-empty">Аудит выключен на этой панели: его проводит только core.</p>;

    const { settings } = audit;
    const servers = audit.report?.servers ?? [];
    const dead = servers.reduce((n, e) => n + e.ghosts.length + e.orphans.length, 0);
    const bad = servers.filter(e => e.status !== 'ok');
    const confirmed = servers.reduce((n, e) => n + [...e.ghosts, ...e.orphans].filter(p => p.seen_before).length, 0);
    const blocked = servers.filter(e => e.blocked);
    const rows = servers.flatMap(e => [
        ...e.ghosts.map(p => ({ e, p, why: 'Есть в ядре, у awg-ctrl записи нет' })),
        ...e.orphans.map(p => ({ e, p, why: 'Панели не нужен, не снят' })),
    ]);
    const state = (p: AuditPeer) => (
        <span className={`chip chip--${p.online ? 'error' : 'offline'}`}>
            <span className="chip-dot" />{p.online ? 'Онлайн' : p.lastHandshake ? `был ${timeAgo(p.lastHandshake)} назад` : 'не подключался'}
        </span>
    );
    const confirm = (p: AuditPeer) => (
        <span className="au-hint">{p.seen_before ? 'подтверждён' : 'найден впервые — снимется, если останется и в следующий раз'}</span>
    );

    return (
        <>
            <section className="au-head">
                <span className={`chip chip--${!audit.report ? 'offline' : dead ? 'error' : 'online'}`}>
                    <span className="chip-dot" />
                    {!audit.report ? 'Аудит ещё не проводился' : dead ? `Мёртвых пиров: ${dead}` : 'Мёртвых пиров нет'}
                </span>
                {audit.report && (
                    <span className="au-hint">
                        Проверено {timeAgo(audit.report.at)} назад · серверов: {servers.length} · раз в {settings.every_h} ч
                    </span>
                )}
                <div className="au-btns">
                    {confirmed > 0 && (
                        <button className="btn btn--tonal" disabled={busy}
                            onClick={() => window.confirm(`Снять с серверов подтверждённых пиров: ${confirmed}?`) && call('POST', '/clean', 'Очистка выполнена')}>
                            <IcoTrash /> Снять подтверждённых ({confirmed})
                        </button>
                    )}
                    <button className="btn btn--tonal" disabled={busy} onClick={run}>
                        <IcoRefresh /> {busy ? 'Работаем…' : 'Проверить сейчас'}
                    </button>
                </div>
            </section>

            <section className="au-box">
                <h4 className="au-box-title">Настройки</h4>
                <label className="au-setting">
                    <span className="au-setting-text">
                        <span className="au-setting-name">Снимать мёртвых автоматически</span>
                        <span className="au-hint">Только тех, кто мёртв два прохода подряд. Ручной проход ничего не снимает.</span>
                    </span>
                    <input type="checkbox" checked={settings.clean} disabled={busy} onChange={e => save({ clean: e.target.checked })} />
                </label>
                <div className="au-setting">
                    <span className="au-setting-text">
                        <label className="au-setting-name" htmlFor="au-every">Период проверки</label>
                        <span className="au-hint">Как часто core обходит серверы</span>
                    </span>
                    <select id="au-every" className="field au-select" value={settings.every_h} disabled={busy}
                        onChange={e => save({ every_h: Number(e.target.value) })}>
                        {[...new Set([...PERIODS, settings.every_h])].sort((a, b) => a - b).map(h => <option key={h} value={h}>{h} ч</option>)}
                    </select>
                </div>
                <div className="au-setting">
                    <span className="au-setting-text">
                        <label className="au-setting-name" htmlFor="au-max">Предел за проход</label>
                        <span className="au-hint">Если на сервере мёртвых больше — автоочистка ничего не снимает и только сообщает (защита от сбоя)</span>
                    </span>
                    <span className="au-setting-ctl">
                        <input id="au-max" className="field au-max" type="number" min={1} max={1000}
                            value={maxEdit || String(settings.max)} onChange={e => setMaxEdit(e.target.value)} />
                        <button className="btn btn--tonal" disabled={busy || !maxEdit || Number(maxEdit) === settings.max}
                            onClick={() => save({ max: Number(maxEdit) }).then(() => setMaxEdit(''))}>Сохранить</button>
                    </span>
                </div>
            </section>

            {bad.length > 0 && (
                <p className="au-notice">
                    Не проверены: {bad.map(e => `${serverName(nodes, e.server_id)} (${e.status === 'offline' ? 'не в сети' : 'ошибка'})`).join(', ')}
                </p>
            )}
            {blocked.length > 0 && (
                <p className="au-notice">
                    Автоочистка пропустила: {blocked.map(e => `${serverName(nodes, e.server_id)} (${e.blocked} пиров при пределе ${settings.max})`).join(', ')}. Проверьте список и снимите вручную.
                </p>
            )}
            {servers.some(e => e.status === 'ok' && !e.ghosts_checked) && (
                <p className="au-notice">Часть серверов на старой версии awg-ctrl: пиров, которые есть только в ядре, там не видно.</p>
            )}

            {rows.length > 0 && (
                <>
                    <div className="table-card au-table-card">
                        <table>
                            <thead><tr><th>Сервер</th><th>Публичный ключ</th><th>Причина</th><th>Мастер-ключ</th><th>Состояние</th></tr></thead>
                            <tbody>
                                {rows.map(({ e, p, why }) => (
                                    <tr key={`${e.server_id}:${p.pub_key}`}>
                                        <td>{serverName(nodes, e.server_id)}</td>
                                        <td className="td-mono"><code className="au-pub" title={p.pub_key}>{p.pub_key}</code></td>
                                        <td>{why}</td>
                                        <td>{p.key ?? '—'}</td>
                                        <td>{state(p)}<br />{confirm(p)}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                    <ul className="au-cards">
                        {rows.map(({ e, p, why }) => (
                            <li className="au-card" key={`${e.server_id}:${p.pub_key}`}>
                                <div className="au-card-top"><b>{serverName(nodes, e.server_id)}</b>{state(p)}</div>
                                <code className="au-pub" title={p.pub_key}>{p.pub_key}</code>
                                <span className="au-hint">{why}{p.key ? ` · ${p.key}` : ''}</span>
                                {confirm(p)}
                            </li>
                        ))}
                    </ul>
                </>
            )}
            {audit.cleaned.length > 0 && (
                <section className="au-box">
                    <h4 className="au-box-title">Снято</h4>
                    <ul className="au-log">
                        {audit.cleaned.slice(0, 20).map(c => (
                            <li key={`${c.at}:${c.pub_key}`}>
                                <span className="au-hint">{timeAgo(c.at)} назад</span>
                                <span>{serverName(nodes, c.server_id)}</span>
                                <code className="au-pub" title={c.pub_key}>{c.pub_key}</code>
                                <span className="au-hint">{c.kind === 'ghost' ? 'был только в ядре' : 'лишний у awg-ctrl'}{c.key ? ` · ${c.key}` : ''} · {c.auto ? 'автоматически' : 'вручную'}</span>
                            </li>
                        ))}
                    </ul>
                </section>
            )}
            {audit.report && rows.length === 0 && <p className="au-empty">Всё чисто: лишних пиров на серверах нет.</p>}
        </>
    );
}
