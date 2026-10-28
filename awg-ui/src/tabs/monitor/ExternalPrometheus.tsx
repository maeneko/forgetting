// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback } from 'react';
import { apiFetch, copyText, timeAgo } from '../../lib/shared';
import { IcoCopy, IcoTrash } from '../../components/icons';

// Внешний Prometheus: свой сервер мониторинга скрейпит /metrics панели по токену «только метрики»
// (/ui/metrics-token; он не открывает ни панель, ни /api/v1). Адрес берём от страницы: в нём уже
// секретный путь панели (UI_PREFIX) или подпапка nginx, поэтому конфиг верен в обоих случаях.
interface View { exists: boolean; prefix?: string; created_at?: number; last_used?: number | null; per_peer: boolean }

function scrapeConfig(tok: string): string {
    const u = new URL('metrics', window.location.href);
    return [
        'scrape_configs:',
        '  - job_name: vpn-panel',
        `    scheme: ${u.protocol.replace(':', '')}`,
        `    metrics_path: ${u.pathname}`,
        '    authorization:',
        `      credentials: ${tok}`,
        '    static_configs:',
        `      - targets: ['${u.host}']`,
    ].join('\n');
}

export default function ExternalPrometheus({ token, showMsg }: { token: string; showMsg: (t: string) => void }) {
    const [view, setView]   = useState<View | null>(null);
    const [fresh, setFresh] = useState('');     // токен показываем один раз — сразу после выпуска
    const [busy, setBusy]   = useState(false);

    const load = useCallback(async () => {
        try { setView(await apiFetch('GET', '/ui/metrics-token', token) as View); } catch { /* карточка необязательна */ }
    }, [token]);
    useEffect(() => { void load(); }, [load]);

    const issue = async () => {
        if (view?.exists && !confirm('Выпустить новый токен? Prometheus со старым перестанет получать метрики.')) return;
        setBusy(true);
        try { const r = await apiFetch('POST', '/ui/metrics-token', token); setFresh(r.token); setView(r); }
        catch (e: any) { showMsg(e?.response?.data?.error ?? 'Не удалось выпустить токен'); }
        finally { setBusy(false); }
    };
    const revoke = async () => {
        if (!confirm('Отозвать токен? Внешний Prometheus перестанет получать метрики.')) return;
        setBusy(true);
        try { setView(await apiFetch('DELETE', '/ui/metrics-token', token)); setFresh(''); }
        catch (e: any) { showMsg(e?.response?.data?.error ?? 'Не удалось отозвать токен'); }
        finally { setBusy(false); }
    };

    if (!view) return null;
    const plain = window.location.protocol === 'http:';
    return (
        <section className="mon-card" aria-labelledby="mon-external">
            <div className="mon-head">
                <h3 className="mon-title" id="mon-external">Внешний Prometheus</h3>
                <span className={`chip chip--${view.exists ? 'online' : 'offline'}`}>
                    <span className="chip-dot" />{view.exists ? 'токен выдан' : 'не подключён'}
                </span>
            </div>
            <p className="mon-hint">
                Свой Prometheus может забирать метрики панели и всех серверов по отдельному токену — он открывает только
                /metrics, не панель и не API. Адрес в конфиге — с секретным путём панели, без него сервер отвечает 404.
                {view.exists && <> Токен <b>{view.prefix}</b>, последний скрейп — {view.last_used ? timeAgo(view.last_used) + ' назад' : 'ещё не было'}.</>}
            </p>
            {plain && (
                <p className="mon-hint">
                    Панель открыта по HTTP: токен и метрики пойдут по сети открытым текстом и видны DPI. Скрейпьте через
                    nginx с TLS или по VPN-туннелю.
                </p>
            )}

            {fresh && (
                <>
                    <p className="mon-hint">Токен показывается один раз. Готовый фрагмент для prometheus.yml:</p>
                    <pre className="mon-code">{scrapeConfig(fresh)}</pre>
                    <div className="mon-actions">
                        <button className="btn btn--tonal" onClick={() => copyText(scrapeConfig(fresh)).then(() => showMsg('Конфиг скопирован'))}>
                            <IcoCopy /> Скопировать конфиг
                        </button>
                    </div>
                </>
            )}

            <div className="mon-actions">
                <button className={`btn ${view.exists ? 'btn--tonal' : 'btn--primary'}`} onClick={issue} disabled={busy}>
                    {view.exists ? 'Перевыпустить токен' : 'Выдать токен'}
                </button>
                {view.exists && (
                    <button className="btn btn--danger" onClick={revoke} disabled={busy}>
                        <IcoTrash /> Отозвать
                    </button>
                )}
            </div>
        </section>
    );
}
