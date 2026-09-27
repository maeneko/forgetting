// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { type NodeInfo, genLabel, cmpVersion } from '../lib/shared';
import { IcoRefresh, IcoPlus, IcoTrash, IcoCopy } from './icons';

// Сервер-бар: карточки серверов (core-локальный и ноды) + «Добавить сервер» и
// (на мобильной) перезапуск AWG под ними. Общий для вкладок — показывает
// серверы, между которыми переключается содержимое вкладки (пиры / абоненты).
// Версия Forgetting на сервере. Нода, которая отстаёт от панели или не сообщает версию
// (агент старше 0.3.4), подсвечена: её стоит обновить — install.sh, «Обновить».
function versionChip(n: NodeInfo, panel: string | null) {
    if (n.pending) return null;
    let tip = '', warn = false;
    if (!n.version) {
        warn = true;
        tip = 'Версия неизвестна: нода старше 0.3.4 — обнови её (install.sh → «Обновить»)';
    } else if (panel && !n.local) {
        const d = cmpVersion(n.version, panel);
        if (d < 0) { warn = true; tip = `Панель на ${panel} — обнови ноду (install.sh → «Обновить»)`; }
        else if (d > 0) { warn = true; tip = `Нода новее панели (${panel}) — обнови панель`; }
    }
    const chip = (
        <span className={`chip server-ver-chip${warn ? ' server-ver-chip--warn' : ''}`}>
            {n.version ? `v${n.version}` : 'v?.?.?'}
        </span>
    );
    return tip ? <span className="tip-wrap server-gen-wrap" data-tip={tip}>{chip}</span> : chip;
}

export default function ServerBar({
    nodes, serverId, hubEnabled, panelVersion, onSelect, onAdd, onJoin, onDelete,
    onRestartAwg, restarting, showRestart = true,
}: {
    nodes: NodeInfo[];
    panelVersion: string | null;
    serverId: number | null;
    hubEnabled: boolean;
    onSelect: (id: number) => void;
    onAdd: () => void;
    onJoin: (n: NodeInfo) => void;
    onDelete: (n: NodeInfo) => void;
    onRestartAwg: () => void;
    restarting: boolean;
    showRestart?: boolean;
}) {
    return (
        <div className="server-bar">
            {nodes.map(n => {
                const h = n.health;
                const active = n.id === serverId;
                return (
                    <div
                        key={n.id}
                        className={`server-card server-card--${active ? 'active' : 'inactive'}`}
                        role="button"
                        tabIndex={0}
                        aria-pressed={active}
                        onClick={() => onSelect(n.id)}
                        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(n.id); } }}
                    >
                        {/* toggle — виден на мобильном вместо чипа */}
                        <div className={`server-toggle${n.online ? ' server-toggle--on' : ''}`} aria-hidden="true">
                            <div className="server-toggle-thumb" />
                        </div>
                        {/* чип онлайн/офлайн — виден на десктопе */}
                        <span className={`chip chip--${n.online ? 'online' : 'offline'} server-online-chip`}>
                            <span className="chip-dot" />
                        </span>
                        <div className="server-card-info">
                            <span className="server-card-name">{h?.server || n.name}</span>
                            {n.pending ? (
                                <span className="server-card-ip">ждёт подключения</span>
                            ) : !n.online ? (
                                <span className="server-card-ip">нет связи</span>
                            ) : h && (
                                <span className="server-card-ip">{h.ip}</span>
                            )}
                            {/* Плашки: поколение AWG (только на связи) и версия Forgetting (известна и без связи) */}
                            {!n.pending && (
                                <span className="server-chips">
                                    {h && n.online && ((h.module || h.tools) ? (
                                        <span
                                            className="tip-wrap server-gen-wrap"
                                            data-tip={`модуль ${h.module || '?'} · tools ${h.tools || '?'}`}
                                        >
                                            <span className="chip chip--offline server-gen-chip">{genLabel(h.gen)}</span>
                                        </span>
                                    ) : (
                                        <span className="chip chip--offline server-gen-chip">{genLabel(h.gen)}</span>
                                    ))}
                                    {versionChip(n, panelVersion)}
                                </span>
                            )}
                        </div>
                        {h && n.online && <span className="server-card-peers">{h.peers} peers</span>}
                        {n.pending && (
                            <button
                                className="btn-icon btn-icon--neutral"
                                aria-label="Новая ссылка подключения"
                                onClick={e => { e.stopPropagation(); onJoin(n); }}
                            >
                                <IcoCopy />
                            </button>
                        )}
                        {!n.local && (
                            <button
                                className="btn-icon btn-icon--danger"
                                aria-label={`Удалить сервер ${n.name}`}
                                onClick={e => { e.stopPropagation(); onDelete(n); }}
                            >
                                <IcoTrash />
                            </button>
                        )}
                    </div>
                );
            })}
            <span
                className="tip-wrap"
                data-tip={hubEnabled ? '' : 'Приём нод выключен: задайте NODE_PORT'}
            >
                <button className="btn server-add" onClick={onAdd} disabled={!hubEnabled}>
                    <IcoPlus /> Добавить сервер
                </button>
            </span>
            {/* Перезапуск AWG — только на мобильной (на десктопе кнопка в углу). */}
            {showRestart && serverId !== null && (
                <button
                    className={`btn btn--tonal awg-restart-mobile${restarting ? ' spinning' : ''}`}
                    onClick={onRestartAwg}
                    disabled={restarting}
                >
                    <IcoRefresh /> {restarting ? 'Перезапуск…' : 'Перезапустить AWG'}
                </button>
            )}
        </div>
    );
}
