// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback, useRef } from 'react';
import { apiFetch, copyText, getServerId, announcePrometheus, type PageProps } from '../../../lib/shared';
import { IcoCopy, IcoPlus, IcoTrash } from '../../../components/icons';
import './prometheus.css';

// Установка встроенного Prometheus (раздел «Prometheus» вкладки «Настройки»; графики — во вкладке «Монитор»).
// Кнопка просит awg-ctrl сервера поставить его (`awg-ctrl monitor install`, root; сама панель пакетов
// не ставит). У панели без VPN он ставится на ноду, и панель читает его через её агента — на сервер,
// выбранный в шапке (при смене сервера вкладка перемонтируется). У панели со своим VPN цель всегда
// «этот сервер».
export interface MonitorStatus {
    ready: boolean; can_install: boolean; command: string;
    host: { id: number; name: string } | null;   // Prometheus на ноде (или будет поставлен туда); null — на этом сервере
    job: { action: 'install' | 'remove'; running: boolean; exit: number | null; log: string } | null;   // последняя операция
}

// Прогресс операции из строк «N/M  шаг», которые печатает сам установщик (cli/src/monitor.ts). Шаг N считаем
// выполненным наполовину: так полоска не стоит на нуле, пока идёт долгий apt, и не обещает больше, чем известно.
const STEP = /^(\d+)\/(\d+)\s+(.+)$/;
const SETTLE_MS = 90_000;
function JobProgress({ job, starting }: { job: MonitorStatus['job']; starting: boolean }) {
    let step: [number, number, string] | null = null;
    for (const l of job?.log.split('\n') ?? []) { const m = STEP.exec(l.trim()); if (m) step = [Number(m[1]), Number(m[2]), m[3]]; }
    const failed = !!job && job.exit !== null && job.exit !== 0;
    const done   = !!job && job.exit === 0;
    const pct    = done ? 100 : step ? ((step[0] - 0.5) / step[1]) * 100 : 0;
    const label  = failed ? `Не удалось (код ${job!.exit})`
        : done ? (job!.action === 'install' ? 'Готово' : 'Удалено')
        : step ? `Шаг ${step[0]} из ${step[1]}: ${step[2]}` : 'Запуск…';
    return (
        <div className="prom-progress" aria-live="polite">
            <span className={`prom-pbar${failed ? ' prom-pbar--bad' : done ? ' prom-pbar--ok' : !step && (starting || job?.running) ? ' prom-pbar--wait' : ''}`}
                role="progressbar" aria-label="Ход операции" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
                <i style={{ width: `${pct}%` }} />
            </span>
            <span className="st-hint">{label}</span>
        </div>
    );
}

export default function PrometheusInstall({ token, showMsg }: PageProps) {
    const target = getServerId() ?? 0;   // куда ставить (панель без VPN) — сервер из шапки
    const [status, setStatus]   = useState<MonitorStatus | null>(null);
    const [pending, setPending] = useState<'install' | 'remove' | null>(null);     // операция запущена отсюда
    const alive = useRef(true);
    // StrictMode монтирует эффект дважды: флаг надо вернуть в true, иначе все ответы отбрасываются
    useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);

    const loadStatus = useCallback(async () => {
        try { const s = await apiFetch('GET', `/ui/monitor?server=${target}`, token) as MonitorStatus; announcePrometheus(s.ready); if (alive.current) setStatus(s); return s; }
        catch { return null; }
    }, [token, target]);

    useEffect(() => { void loadStatus(); }, [loadStatus]);

    // Пока идёт установка или удаление — следим за ходом: раз в 2 с читаем статус и лог. Операция
    // закончилась, а панель ещё не видит результата (на ноде агент перезапускается и переподключается) —
    // следим дальше, пока ready не совпадёт с ней, но не дольше SETTLE_MS: иначе «Готово» висит рядом
    // с «не подключён», а пункт «Монитор» ждёт минутного опроса App.
    const [settle, setSettle] = useState<number | null>(null);   // до какого момента ждём ready
    useEffect(() => {
        if (!pending && !status?.job?.running && settle === null) return;
        const t = setInterval(async () => {
            const s = await loadStatus();
            if (!s?.job || s.job.running) return;   // агент ноды переподключается — job пока не виден
            setPending(null);
            const waiting = s.job.exit === 0 && s.ready !== (s.job.action === 'install');
            setSettle(d => waiting && (d === null || Date.now() < d) ? d ?? Date.now() + SETTLE_MS : null);
        }, 2000);
        return () => clearInterval(t);
    }, [pending, status?.job?.running, settle, loadStatus]);

    const run = async (action: 'install' | 'remove') => {
        if (action === 'remove' && !confirm('Удалить Prometheus? Он будет остановлен, а накопленные графики (до 31 дня) сотрутся. Панель продолжит отдавать /metrics.')) return;
        setPending(action);
        try { await apiFetch('POST', `/ui/monitor/${action}?server=${target}`, token); await loadStatus(); }
        catch (e: any) { setPending(null); showMsg(e?.response?.data?.error ?? (action === 'install' ? 'Не удалось запустить установку' : 'Не удалось запустить удаление')); }
    };

    if (!status) return <section className="st-section"><p className="st-hint">Загрузка…</p></section>;

    const job      = status.job;
    const working  = !!pending || !!job?.running || settle !== null;
    const doing    = pending ?? (job?.running ? job.action : null);
    const showJob  = !!pending || (!!job && (job.running || job.exit !== null));
    const failed   = !!job && job.exit !== null && job.exit !== 0;
    // «Этот сервер» путал: в шапке может быть выбрана нода, а Prometheus у панели один и стоит не там
    const where    = status.host ? <>на сервере <b>{status.host.name}</b></> : 'на сервере панели';
    const command  = status.ready ? 'awg-ctrl monitor remove' : status.command;

    return (
        <section className="st-section">
            <div className="st-row">
                <span className="st-row-text">
                    <span className="st-row-name">Встроенный Prometheus</span>
                    <span className="st-hint">
                        {status.ready
                            ? <>Один на всю панель, собирает метрики всех серверов. Стоит {where}{status.host && ' (панель читает его через агента ноды)'}, слушает только 127.0.0.1:9090, графики хранит 31 день.</>
                            : <>Один на всю панель, соберёт метрики всех серверов. Панель поставит его {where} и сама подключит к своим метрикам: графики «Монитора» появятся через минуту-две. Наружу он не открывается.</>}
                    </span>
                </span>
                <span className="st-row-ctl">
                    <span className={`chip chip--${status.ready ? 'online' : 'offline'}`}>
                        <span className="chip-dot" />{doing === 'install' ? 'ставится' : doing === 'remove' ? 'удаляется' : settle !== null ? (status.ready ? 'отключается' : 'подключается') : status.ready ? 'работает' : 'не подключён'}
                    </span>
                    {status.can_install && (status.ready ? (
                        <button className="btn btn--danger" onClick={() => run('remove')} disabled={working}>
                            <IcoTrash /> {doing === 'remove' ? 'Удаляем…' : 'Удалить'}
                        </button>
                    ) : (
                        <button className="btn btn--primary" onClick={() => run('install')} disabled={working}>
                            <IcoPlus /> {doing === 'install' ? 'Устанавливаем…' : 'Установить'}
                        </button>
                    ))}
                </span>
            </div>

            {showJob && <JobProgress job={job} starting={!!pending} />}

            {!status.can_install && (
                <div className="st-row">
                    <span className="st-row-text">
                        <span className="st-hint">
                            {status.ready ? 'Удаляется на сервере с Prometheus от root:'
                                : 'На выбранном сервере установка кнопкой недоступна (нет связи или старый awg-ctrl): выберите другой сервер в шапке или выполните на этом от root:'}
                        </span>
                        <code className="prom-code">{command}</code>
                    </span>
                    <button className="btn btn--tonal" onClick={() => copyText(command).then(() => showMsg('Команда скопирована'))}>
                        <IcoCopy /> Скопировать
                    </button>
                </div>
            )}

            {/* Полный лог — только при ошибке: оператору нужна причина, а не поток строк apt */}
            {failed && (
                <>
                    <pre className="prom-log">{job!.log.trim()}</pre>
                    <p className="login-error">
                        {job!.action === 'install' ? 'Установка' : 'Удаление'} не удалась. Если дело в правах или пакетах, выполните вручную: <b>{`awg-ctrl monitor ${job!.action}`}</b>
                    </p>
                </>
            )}
        </section>
    );
}
