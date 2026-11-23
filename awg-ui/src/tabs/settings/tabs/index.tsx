// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState, useEffect, useCallback, useRef } from 'react';
import {
    fetchEnabledTabs, saveEnabledTabs, announceTabs, OPTIONAL_TAB_IDS,
    type OptionalTabId,
} from '../../../lib/shared';
import { IcoUsers, IcoKey, IcoGear, IcoHistory, IcoRelay, IcoPulse, IcoTelegram } from '../../../components/icons';
import './tabs.css';

// Раздел «Настройки → Вкладки»: какие вкладки показывать в меню панели.
// «Пользователи», «Мастер-ключи» и «Настройки» — база, она есть всегда; здесь строки-напоминания
// с тихой пометкой «Всегда» (не «мёртвый» тумблер). «Журнал», «Релеи», «Монитор» и «Прокси» —
// дополнительный функционал: выключенная вкладка исчезает и из сайдбара, и из мобильного
// меню (App читает тот же список). Список общий для всех операторов и живёт на сервере (GET/PUT /ui/tabs).
//
// Переключатель срабатывает сразу (оптимистично): меню перестраивается, не дожидаясь ответа.
// Не сохранилось — возвращаем как было и показываем одну тихую строку с «Повторить»; удачное
// сохранение не подтверждаем: снэкбар на каждое переключение — шум.

interface BaseRow { id: string; name: string; Icon: () => JSX.Element }
interface OptRow { id: OptionalTabId; name: string; hint: string; Icon: () => JSX.Element }

// Строки перечислены явно, а не собраны из реестра вкладок: здесь нужны человеческие описания,
// а порядок — тот, в котором оператор их знает.
const BASE: BaseRow[] = [
    { id: 'users',      name: 'Пользователи', Icon: IcoUsers },
    { id: 'masterkeys', name: 'Мастер-ключи', Icon: IcoKey },
    { id: 'settings',   name: 'Настройки',    Icon: IcoGear },
];
const OPTIONAL: OptRow[] = [
    { id: 'connlog', name: 'Журнал', hint: 'Журнал подключений выбранного сервера', Icon: IcoHistory },
    { id: 'relays',  name: 'Релеи',   hint: 'Пересылка портов между серверами',      Icon: IcoRelay },
    { id: 'monitor', name: 'Монитор', hint: 'Живые метрики и графики, требует Prometheus', Icon: IcoPulse },
    { id: 'proxy',   name: 'Прокси',  hint: 'Telegram MTProto-прокси на ноде',        Icon: IcoTelegram },
];

// Что не удалось — от этого зависит «Повторить»: перечитать список или ещё раз записать его.
type Failed = { kind: 'load' } | { kind: 'save'; list: OptionalTabId[] };

function Switch({ on, disabled, label, onChange }: {
    on: boolean;
    disabled?: boolean;
    label: string;
    onChange?: (on: boolean) => void;
}) {
    return (
        <button
            type="button"
            role="switch"
            aria-checked={on}
            aria-label={label}
            className="tabs-switch"
            disabled={disabled}
            onClick={() => onChange?.(!on)}
        >
            {/* Дорожка и бегунок — отдельными слоями: кнопка — тач-цель (44×32, на тач 48×44),
                а рисуется внутри дорожка 40×24 */}
            <span className="tabs-track" aria-hidden="true"><span className="tabs-thumb" /></span>
        </button>
    );
}

export default function TabsSection({ token }: { token: string }) {
    const [enabled, setEnabled] = useState<OptionalTabId[] | null>(null);   // null — ещё загружаем
    const [saving, setSaving]   = useState(false);
    const [error, setError]     = useState('');
    const [failed, setFailed]   = useState<Failed | null>(null);
    const savingRef             = useRef(false);   // для слушателя awg:tabs: свои же события не применяем
    // Запросы уходят строго по очереди, а применяется ответ только последнего: два быстрых
    // переключения не должны ни потеряться (первое ещё в пути), ни переставиться местами на сервере.
    const queue                 = useRef<Promise<void>>(Promise.resolve());
    const seq                   = useRef(0);

    const load = useCallback(async () => {
        setEnabled(null); setError(''); setFailed(null);
        try {
            setEnabled(await fetchEnabledTabs(token));   // обычно уже в кэше App — без запроса
        } catch {
            // Не смогли прочитать — показываем всё и оставляем переключатели рабочими: пусть оператор
            // попробует сохранить (PUT запишет полный список) или нажмёт «Повторить».
            setEnabled([...OPTIONAL_TAB_IDS]);
            setError('Не удалось загрузить список вкладок — показаны все');
            setFailed({ kind: 'load' });
        }
    }, [token]);

    useEffect(() => { void load(); }, [load]);

    // Список поменяли в другом окне, и App перечитал его при возврате в окно — не показываем старьё.
    useEffect(() => {
        const h = (e: Event) => {
            if (!savingRef.current) setEnabled((e as CustomEvent<OptionalTabId[]>).detail);
        };
        window.addEventListener('awg:tabs', h);
        return () => window.removeEventListener('awg:tabs', h);
    }, []);

    // Записать список. next — что хотим, prev — к чему вернуться, если сервер откажет.
    // Переключатель и меню меняются сразу; ответ применяется, только если это по-прежнему
    // последнее намерение оператора (seq) — иначе более свежее уже показано.
    const commit = useCallback((next: OptionalTabId[], prev: OptionalTabId[]) => {
        const my = ++seq.current;
        savingRef.current = true;
        setSaving(true);
        setEnabled(next);
        setError('');
        setFailed(null);
        announceTabs(next);                        // меню перестраивается сразу, вместе с переключателем
        queue.current = queue.current.then(async () => {
            try {
                const saved = await saveEnabledTabs(token, next);
                if (my !== seq.current) return;
                setEnabled(saved);
                announceTabs(saved);
            } catch (e: any) {
                if (my !== seq.current) return;
                setEnabled(prev);
                announceTabs(prev);                // откат: вкладка возвращается в меню
                setError(e?.response?.data?.error
                    ? `Не удалось сохранить: ${e.response.data.error}`
                    : 'Не удалось сохранить — переключатель возвращён назад');
                setFailed({ kind: 'save', list: next });
            } finally {
                if (my === seq.current) { savingRef.current = false; setSaving(false); }
            }
        });
    }, [token]);

    const toggle = (id: OptionalTabId, on: boolean) => {
        if (!enabled) return;
        const next = OPTIONAL_TAB_IDS.filter(x => (x === id ? on : enabled.includes(x)));
        commit(next, enabled);
    };

    const retry = () => {
        if (!failed || saving) return;
        if (failed.kind === 'load') void load();
        else commit(failed.list, enabled ?? [...OPTIONAL_TAB_IDS]);
    };
    const loading = enabled === null;

    return (
        <div className="tabs-body">
            <p className="st-hint st-lead tabs-lead">
                Базовые разделы показываются всегда, дополнительные можно выключить.
            </p>

            <section className="st-section tabs-group" aria-labelledby="tabs-base">
                <div className="tabs-group-head">
                    <span className="tabs-group-name" id="tabs-base">Базовые</span>
                </div>
                {BASE.map(({ id, name, Icon }) => (
                    <div className="st-row tabs-row tabs-row--base" key={id}>
                        <span className="tabs-row-icon" aria-hidden="true"><Icon /></span>
                        <span className="st-row-text">
                            <span className="st-row-name">{name}</span>
                        </span>
                        <span className="st-row-ctl">
                            {/* Не тумблер: выключить нельзя, а «мёртвый» переключатель читался бы как
                                сломанный. Компактная пометка + подсказка — и строка не занимает высоту. */}
                            <span className="tip-wrap tip-wrap--end tabs-base-tip" data-tip="Базовый раздел — не отключается">
                                <span className="tabs-base-mark">Всегда</span>
                            </span>
                        </span>
                    </div>
                ))}
            </section>

            <section className="st-section tabs-group" aria-labelledby="tabs-extra" aria-busy={loading || saving}>
                <div className="tabs-group-head">
                    <span className="tabs-group-name" id="tabs-extra">Дополнительно</span>
                    {loading && <span className="tabs-group-note" role="status">Загрузка…</span>}
                </div>
                {OPTIONAL.map(({ id, name, hint, Icon }) => (
                    <div className="st-row tabs-row" key={id}>
                        <span className="tabs-row-icon" aria-hidden="true"><Icon /></span>
                        <span className="st-row-text">
                            <span className="st-row-name">{name}</span>
                            <span className="st-hint">{hint}</span>
                        </span>
                        <span className="st-row-ctl">
                            <Switch
                                on={!loading && enabled.includes(id)}
                                disabled={loading}
                                label={name}
                                onChange={on => toggle(id, on)}
                            />
                        </span>
                    </div>
                ))}
                {error && (
                    <div className="tabs-error" role="alert">
                        <span className="tabs-error-text">{error}</span>
                        <button type="button" className="btn btn--text" onClick={retry} disabled={saving}>
                            Повторить
                        </button>
                    </div>
                )}
            </section>
        </div>
    );
}
