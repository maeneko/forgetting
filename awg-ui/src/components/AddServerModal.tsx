// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import axios from 'axios';
import { apiFetch, copyText, type NodeInfo } from '../lib/shared';
import { IcoCopy } from './icons';

interface Joined { id: number; name: string; join: string; expires_in: number }

// Добавление ноды: имя → одноразовая join-строка, которую выполняют на самой ноде
// (`awg-ctrl join …`). С `node` открывается сразу за новой строкой для уже
// созданной, но ещё не подключавшейся ноды.
export default function AddServerModal({ token, node, onClose, onChanged, showMsg }: {
    token:     string;
    node?:     NodeInfo;
    onClose:   () => void;
    onChanged: () => void;
    showMsg:   (text: string) => void;
}) {
    const [name, setName]     = useState('');
    const [busy, setBusy]     = useState(false);
    const [result, setResult] = useState<Joined | null>(null);
    const [error, setError]   = useState('');

    const call = async (fn: () => Promise<Joined>) => {
        setBusy(true); setError('');
        try { setResult(await fn()); onChanged(); }
        catch (e) { setError(axios.isAxiosError(e) ? e.response?.data?.error ?? 'Не удалось' : 'Не удалось'); }
        finally { setBusy(false); }
    };
    const create = () => call(() =>
        apiFetch('POST', '/ui/nodes', token, { name: name.trim(), host: window.location.hostname }));

    useEffect(() => {
        if (node) void call(() => apiFetch('POST', `/ui/nodes/${node.id}/join`, token, { host: window.location.hostname }));
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    useEffect(() => {
        const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        document.addEventListener('keydown', h);
        return () => document.removeEventListener('keydown', h);
    }, [onClose]);

    const command = result ? `awg-ctrl join '${result.join}'` : '';

    return (
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal" role="dialog" aria-modal="true" aria-label="Добавить сервер" onClick={e => e.stopPropagation()}>
                <h3 className="node-modal-title">{node ? `Подключение: ${node.name}` : 'Добавить сервер'}</h3>

                {!node && !result && (
                    <>
                        <p className="node-modal-hint">
                            Нода — обычный сервер с установленным Forgetting. Она сама подключится к этой панели,
                            открывать на ней порты не нужно.
                        </p>
                        <input
                            className="field"
                            placeholder="Название сервера"
                            value={name}
                            maxLength={40}
                            autoFocus
                            onChange={e => setName(e.target.value)}
                            onKeyDown={e => e.key === 'Enter' && name.trim() && !busy && create()}
                        />
                    </>
                )}

                {result && (
                    <>
                        <p className="node-modal-hint">
                            Выполните на сервере «{result.name}» от root. Ссылка одноразовая и действует
                            {' '}{Math.round(result.expires_in / 60)} мин.
                        </p>
                        <code className="node-modal-cmd">{command}</code>
                        <p className="node-modal-hint">
                            Канал шифруется TLS с проверкой отпечатка сертификата этой панели: подменить ноду
                            или панель по дороге не выйдет.
                        </p>
                    </>
                )}

                {error && <p className="login-error">{error}</p>}

                <div className="node-modal-actions">
                    {result ? (
                        <button className="btn btn--tonal" onClick={() => copyText(command).then(() => showMsg('Команда скопирована'))}>
                            <IcoCopy /> Скопировать
                        </button>
                    ) : !node && (
                        <button className="btn btn--primary" onClick={create} disabled={busy || !name.trim()}>
                            Создать
                        </button>
                    )}
                    <button className="btn btn--outline" onClick={onClose}>{result ? 'Готово' : 'Отмена'}</button>
                </div>
            </div>
        </div>
    );
}
