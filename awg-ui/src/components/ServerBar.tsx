// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { type NodeInfo, genLabel, cmpVersion, MIN_NODE_VERSION } from '../lib/shared';
import { IcoRefresh, IcoPlus, IcoTrash, IcoCopy, IcoSettings } from './icons';

// Сервер-бар: карточки серверов (core-локальный и ноды) + «Добавить сервер» и
// (на мобильной) перезапуск AWG под ними. Общий для вкладок — показывает
// серверы, между которыми переключается содержимое вкладки (пиры / абоненты).
// Версия Forgetting на сервере, три состояния: совпадает с панелью — нейтральный чип; нода отстаёт
// от панели (но не старше MIN_NODE_VERSION) или новее неё — жёлтый, стоит обновить; старше
// MIN_NODE_VERSION или не сообщает версию (агент старше 0.3.4) — красный: хаб такую ноду не подключает.
function versionChip(n: NodeInfo, panel: string | null) {
    if (n.pending) return null;
    let tip = '', level: '' | 'warn' | 'bad' = '';
    if (!n.version) {
        level = 'bad';
        tip = `Версия неизвестна: нода старше 0.3.4, панель с ней не работает — обнови её до ${MIN_NODE_VERSION} (install.sh → «Обновить»)`;
    } else if (!n.local && cmpVersion(n.version, MIN_NODE_VERSION) < 0) {
        level = 'bad';
        tip = `Слишком старая версия: панель с ней не работает, нужна не ниже ${MIN_NODE_VERSION} — обнови ноду (install.sh → «Обновить»)`;
    } else if (panel && !n.local) {
        const d = cmpVersion(n.version, panel);
        if (d < 0) { level = 'warn'; tip = `Устарела: панель на ${panel} — обнови ноду (install.sh → «Обновить»)`; }
        else if (d > 0) { level = 'warn'; tip = `Нода новее панели (${panel}) — обнови панель`; }
    }
    const chip = (
        <span className={`chip server-ver-chip${level ? ` server-ver-chip--${level}` : ''}`}>
            {n.version ? `v${n.version}` : 'v?.?.?'}
        </span>
    );
    return tip ? <span className="tip-wrap server-gen-wrap" data-tip={tip}>{chip}</span> : chip;
}

export default function ServerBar({
    nodes, serverId, hubEnabled, panelVersion, onSelect, onAdd, onJoin, onDelete, onSettings,
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
    onSettings: (n: NodeInfo) => void;
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
                        <span
                            className={`status-dot${n.online ? ' status-dot--online' : ''}`}
                            role="img"
                            aria-label={n.online ? 'на связи' : 'нет связи'}
                        />
                        <div className="server-card-info">
                            <span className="server-card-name">{h?.server || n.name}</span>
                            {n.pending ? (
                                <span className="server-card-ip">ждёт подключения</span>
                            ) : n.outdated ? (
                                <span className="server-card-ip server-card-ip--bad">нужно обновить до {MIN_NODE_VERSION}</span>
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
                        {/* Настройки сервера: у ноды, которая ещё не подключалась, настраивать нечего */}
                        {!n.pending && n.online && (
                            <button
                                className="btn-icon btn-icon--neutral"
                                aria-label={`Настройки сервера ${n.name}`}
                                onClick={e => { e.stopPropagation(); onSettings(n); }}
                            >
                                <IcoSettings />
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
