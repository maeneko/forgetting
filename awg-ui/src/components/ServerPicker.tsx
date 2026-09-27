// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { type NodeInfo, genLabel } from '../lib/shared';

// Выбор набора серверов галочками: серверы мастер-ключа, видимость API-ключа. Набор не
// бывает пустым — последний отмеченный сервер снять нельзя. onChange получает новый набор
// и сервер, который переключили.
export default function ServerPicker({ nodes, selected, onChange }: {
    nodes: NodeInfo[];
    selected: number[];
    onChange: (next: number[], node: NodeInfo, added: boolean) => void;
}) {
    return (
        <>
            {nodes.map(n => {
                const on = selected.includes(n.id);
                const last = on && selected.length === 1;
                const h = n.health;
                return (
                    <label className={`server-pick${last ? ' server-pick--locked' : ''}`} key={n.id}>
                        <input type="checkbox" checked={on} disabled={last}
                            onChange={() => onChange(on ? selected.filter(id => id !== n.id) : [...selected, n.id], n, !on)} />
                        <span className={`chip chip--${n.online ? 'online' : 'offline'} server-pick-dot`} title={n.online ? 'В сети' : 'Нет связи'}>
                            <span className="chip-dot" />
                        </span>
                        <span className="server-pick-name">{h?.server || n.name}</span>
                        {h?.ip && <code className="server-pick-ip">{h.ip}</code>}
                        {h?.gen && <span className="server-pick-gen">{genLabel(h.gen)}</span>}
                    </label>
                );
            })}
        </>
    );
}

// Имя сервера для подписей: как в сервер-баре, иначе «#id».
export const serverName = (nodes: NodeInfo[], id: number): string => {
    const n = nodes.find(x => x.id === id);
    return n ? n.health?.server || n.name : `#${id}`;
};
