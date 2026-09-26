// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { type NodeInfo, genLabel } from '../lib/shared';
import { IcoRefresh, IcoPlus, IcoTrash, IcoCopy } from './icons';

// Сервер-бар: карточки серверов (core-локальный и ноды) + «Добавить сервер» и
// (на мобильной) перезапуск AWG под ними. Общий для вкладок — показывает
// серверы, между которыми переключается содержимое вкладки (пиры / абоненты).
export default function ServerBar({
    nodes, serverId, hubEnabled, onSelect, onAdd, onJoin, onDelete,
    onRestartAwg, restarting, showRestart = true,
}: {
    nodes: NodeInfo[];
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
                                <>
                                    <span className="server-card-ip">{h.ip}</span>
                                    {(h.module || h.tools) ? (
                                        <span
                                            className="tip-wrap server-gen-wrap"
                                            data-tip={`модуль ${h.module || '?'} · tools ${h.tools || '?'}`}
                                        >
                                            <span className="chip chip--offline server-gen-chip">{genLabel(h.gen)}</span>
                                        </span>
                                    ) : (
                                        <span className="chip chip--offline server-gen-chip">{genLabel(h.gen)}</span>
                                    )}
                                </>
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
