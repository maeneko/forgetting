// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useRef } from 'react';
import { type NodeInfo, genLabel, serversLabel, MIN_NODE_VERSION } from '../lib/shared';
import { IcoChevronDown } from './icons';

// Выбор одного сервера внутри страницы: чьи графики показывать в «Мониторе».
// Выглядит как выбор сервера в шапке страницы: кнопка с текущим сервером (точка связи, имя, IP,
// «N серверов», стрелка) и под ней меню-список; выбранная строка залита primary-container. Выбор
// локальный: сервер в шапке панели он не меняет. Серверы без связи видны, но недоступны (на них
// ничего не поставить) — кроме pickOffline: история на графиках у них есть. all — первой строкой
// «Все серверы» (ALL_SERVERS).
// Меню раскрывается от правого края кнопки, как в шапке.
export const ALL_SERVERS = -1;

export default function ServerSelect({ nodes, value, onChange, name, all, pickOffline }: {
    nodes: NodeInfo[];
    value: number;
    onChange: (id: number) => void;
    name: string;           // id меню (aria-controls)
    all?: boolean;
    pickOffline?: boolean;
}) {
    const [open, setOpen] = useState(false);
    const ref = useRef<HTMLDivElement>(null);

    // Закрывается кликом мимо и по Escape — как меню серверов в шапке
    useEffect(() => {
        if (!open) return;
        const down = (e: MouseEvent) => { if (!ref.current?.contains(e.target as Node)) setOpen(false); };
        const key  = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
        document.addEventListener('mousedown', down);
        document.addEventListener('keydown', key);
        return () => { document.removeEventListener('mousedown', down); document.removeEventListener('keydown', key); };
    }, [open]);

    const online  = nodes.filter(n => n.online).length;
    const current = nodes.find(n => n.id === value) ?? null;
    const isAll   = value === ALL_SERVERS;
    const pick = (id: number) => { setOpen(false); if (id !== value) onChange(id); };
    const sub = (n: NodeInfo) => n.pending ? 'ждёт подключения' : n.outdated ? `нужно обновить до ${MIN_NODE_VERSION}` : !n.online ? 'нет связи' : n.health?.ip ?? '';

    return (
        <div className="server-select" ref={ref}>
            <button type="button" className={`server-current${open ? ' server-current--open' : ''}`}
                aria-expanded={open} aria-controls={name} onClick={() => setOpen(o => !o)}>
                <span className={`status-dot${(isAll ? online > 0 : current?.online) ? ' status-dot--online' : ''}`}
                    role="img" aria-label={(isAll ? online > 0 : current?.online) ? 'на связи' : 'нет связи'} />
                <span className="server-current-info">
                    <span className="server-card-name">
                        {isAll ? 'Все серверы' : current ? (current.health?.server || current.name) : 'Сервер не выбран'}
                    </span>
                    <span className="server-card-ip">{isAll ? `на связи ${online} из ${nodes.length}` : current ? sub(current) : ''}</span>
                </span>
                <span className="server-current-count">{serversLabel(nodes.length)}</span>
                <span className="server-current-caret"><IcoChevronDown /></span>
            </button>
            {open && (
                <div className="server-menu" id={name} role="listbox" aria-label="Серверы">
                    {all && (
                        <button type="button" role="option" aria-selected={isAll}
                            className={`server-option${isAll ? ' server-option--active' : ''}`} onClick={() => pick(ALL_SERVERS)}>
                            <span className="status-dot server-option-nodot" aria-hidden="true" />{/* место точки — текст в один столбец */}
                            <span className="server-current-info">
                                <span className="server-card-name">Все серверы</span>
                                <span className="server-option-sub">{serversLabel(nodes.length)} · на связи {online}</span>
                            </span>
                        </button>
                    )}
                    {nodes.map(n => {
                        const off = !n.online && !pickOffline;
                        return (
                            <button key={n.id} type="button" role="option" aria-selected={n.id === value} disabled={off}
                                className={`server-option${n.id === value ? ' server-option--active' : ''}`} onClick={() => pick(n.id)}>
                                <span className={`status-dot${n.online ? ' status-dot--online' : ''}`} role="img"
                                    aria-label={n.online ? 'на связи' : 'нет связи'} />
                                <span className="server-current-info">
                                    <span className="server-card-name">{n.health?.server || n.name}</span>
                                    <span className="server-card-ip">{sub(n)}</span>
                                </span>
                                {n.health?.gen && <span className="server-option-sub">{genLabel(n.health.gen)}</span>}
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}
