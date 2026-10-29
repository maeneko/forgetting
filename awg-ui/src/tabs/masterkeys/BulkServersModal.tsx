// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useMemo } from 'react';
import { apiFetch, genLabel, type NodeInfo } from '../../lib/shared';

// «Серверы для всех»: один набор правок на все мастер-ключи (POST /ui/masterkeys/servers).
// У каждого сервера — «у всех», «ни у кого» или «как есть» (если он стоит только у части ключей).
// Перед применением сервер считает, сколько ключей изменится и какие ключи API потеряют доступ к
// части из них (ключ виден API-ключу, только пока все его серверы в области).
type Pick = 'all' | 'none' | 'keep';
interface Preview { changed: number; total: number; hidden: { id: number; label: string; keys: number }[] }

const errText = (e: any): string => e?.response?.data?.error ?? 'Ошибка запроса';

export default function BulkServersModal({ token, nodes, keys, defaults, onClose, onDone }: {
    token: string;
    nodes: NodeInfo[];
    keys: { servers: number[] }[];               // живые мастер-ключи
    defaults: number[] | null;
    onClose: () => void;
    onDone: (changed: number) => void;
}) {
    const total = keys.length;
    const count = (id: number) => keys.filter(k => k.servers.includes(id)).length;
    const initial = (id: number): Pick => { const n = count(id); return n === total && total > 0 ? 'all' : n === 0 ? 'none' : 'keep'; };
    const [picks, setPicks] = useState<Record<number, Pick>>(() => Object.fromEntries(nodes.map(n => [n.id, initial(n.id)])));
    const [forNew, setForNew] = useState(true);
    const [preview, setPreview] = useState<Preview | null>(null);
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);

    const { add, remove } = useMemo(() => ({
        add:    nodes.filter(n => picks[n.id] === 'all'  && count(n.id) < total).map(n => n.id),
        remove: nodes.filter(n => picks[n.id] === 'none' && count(n.id) > 0).map(n => n.id),
    }), [picks, nodes, keys]);
    const all = nodes.filter(n => picks[n.id] === 'all').map(n => n.id);
    const sameDefaults = JSON.stringify([...all].sort()) === JSON.stringify([...(defaults ?? [])].sort());
    const body = { add, remove, ...(forNew && !sameDefaults ? { defaults: all.length ? all : null } : {}) };
    const nothing = !add.length && !remove.length && !('defaults' in body);

    useEffect(() => {
        if (!add.length && !remove.length) { setPreview(null); setError(''); return; }
        let live = true;
        apiFetch('POST', '/ui/masterkeys/servers', token, { add, remove, dry_run: true })
            .then(p => { if (live) { setPreview(p); setError(''); } })
            .catch(e => { if (live) { setPreview(null); setError(errText(e)); } });
        return () => { live = false; };
    }, [token, add, remove]);

    useEffect(() => {
        const h = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [onClose]);

    const apply = async () => {
        setBusy(true);
        try { const r = await apiFetch('POST', '/ui/masterkeys/servers', token, body); onDone(r.changed ?? 0); }
        catch (e) { setError(errText(e)); }
        finally { setBusy(false); }
    };

    const option = (id: number, p: Pick, label: string) => (
        <button type="button" className={`bs-opt${picks[id] === p ? ' bs-opt--on' : ''}`} aria-pressed={picks[id] === p}
            onClick={() => setPicks(prev => ({ ...prev, [id]: p }))}>{label}</button>
    );

    return (
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal bs-modal" role="dialog" aria-modal="true" aria-label="Серверы для всех ключей" onClick={e => e.stopPropagation()}>
                <h3 className="node-modal-title">Серверы для всех ключей</h3>
                <p className="node-modal-hint">
                    Применится ко всем мастер-ключам сразу ({total}). Пользователям ничего делать не нужно: приложение получит
                    новый набор серверов при следующем обновлении подписки, ссылки и ключи остаются прежними.
                </p>
                <div className="bs-list">
                    {nodes.map(n => {
                        const c = count(n.id), partial = c > 0 && c < total;
                        return (
                            <div className="bs-row" key={n.id}>
                                <span className={`chip chip--${n.online ? 'online' : 'offline'} server-pick-dot`} title={n.online ? 'В сети' : 'Нет связи'}>
                                    <span className="chip-dot" />
                                </span>
                                <span className="bs-name">
                                    <span className="server-pick-name">{n.health?.server || n.name}</span>
                                    <span className="bs-sub">
                                        {n.health?.ip && <code className="server-pick-ip">{n.health.ip}</code>}
                                        {n.health?.gen && <span>{genLabel(n.health.gen)}</span>}
                                        <span>сейчас у {c} из {total}</span>
                                    </span>
                                </span>
                                <span className="bs-opts" role="group" aria-label={`Сервер ${n.name}`}>
                                    {option(n.id, 'all', 'У всех')}
                                    {partial && option(n.id, 'keep', 'Как есть')}
                                    {option(n.id, 'none', 'Ни у кого')}
                                </span>
                            </div>
                        );
                    })}
                </div>
                <label className="bs-check">
                    <input type="checkbox" checked={forNew} onChange={e => setForNew(e.target.checked)} />
                    <span>Новым ключам — серверы «У всех»{all.length ? '' : ' (не выбрано ни одного — как раньше: все подключённые)'}</span>
                </label>
                {preview && (
                    <p className="node-modal-hint">Изменится ключей: {preview.changed} из {preview.total}.</p>
                )}
                {preview?.hidden.map(h => (
                    <p className="mk-notice" key={h.id}>
                        Ключ API «{h.label}» перестанет видеть {h.keys} мастер-ключ(ей): у них появится сервер вне его области.
                        Расширьте его область в «Настройках», на вкладке «API-ключи», если он ими управляет.
                    </p>
                ))}
                {error && <p className="login-error">{error}</p>}
                <div className="node-modal-actions">
                    <button className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button className="btn btn--primary" disabled={busy || nothing || !!error} onClick={apply}>
                        {busy ? 'Применяем…' : 'Применить'}
                    </button>
                </div>
            </div>
        </div>
    );
}
