// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
//
// Своё поле даты и времени вместо нативного datetime-local: системный пикер Chrome
// (два спиннера часов и минут) выпадает из панели. Здесь — кнопка с датой и поповер
// с календарём (неделя с понедельника), временем и быстрыми сроками.
// Значение — та же строка, что у input[type=datetime-local]: «ГГГГ-ММ-ДДТчч:мм»,
// пустая строка — «без ограничения».
import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';

const IcoCalendar = () => (
    <svg width="18" height="18" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true">
        <path d="M200-80q-33 0-56.5-23.5T120-160v-560q0-33 23.5-56.5T200-800h40v-40q0-17 11.5-28.5T280-880q17 0 28.5 11.5T320-840v40h320v-40q0-17 11.5-28.5T680-880q17 0 28.5 11.5T720-840v40h40q33 0 56.5 23.5T840-720v560q0 33-23.5 56.5T760-80H200Zm0-80h560v-400H200v400Zm0-480h560v-80H200v80Zm280 240q-17 0-28.5-11.5T440-440q0-17 11.5-28.5T480-480q17 0 28.5 11.5T520-440q0 17-11.5 28.5T480-400Zm-188.5-11.5Q280-423 280-440t11.5-28.5Q303-480 320-480t28.5 11.5Q360-457 360-440t-11.5 28.5Q337-400 320-400t-28.5-11.5ZM640-400q-17 0-28.5-11.5T600-440q0-17 11.5-28.5T640-480q17 0 28.5 11.5T680-440q0 17-11.5 28.5T640-400ZM480-240q-17 0-28.5-11.5T440-280q0-17 11.5-28.5T480-320q17 0 28.5 11.5T520-280q0 17-11.5 28.5T480-240Zm-188.5-11.5Q280-263 280-280t11.5-28.5Q303-320 320-320t28.5 11.5Q360-297 360-280t-11.5 28.5Q337-240 320-240t-28.5-11.5ZM640-240q-17 0-28.5-11.5T600-280q0-17 11.5-28.5T640-320q17 0 28.5 11.5T680-280q0 17-11.5 28.5T640-240Z" />
    </svg>
);
const IcoChevronLeft = () => (
    <svg width="20" height="20" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true">
        <path d="m432-480 156 156q11 11 11 28t-11 28q-11 11-28 11t-28-11L348-452q-6-6-8.5-13t-2.5-15q0-8 2.5-15t8.5-13l184-184q11-11 28-11t28 11q11 11 11 28t-11 28L432-480Z" />
    </svg>
);
const IcoChevronRight = () => (
    <svg width="20" height="20" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true">
        <path d="M504-480 348-636q-11-11-11-28t11-28q11-11 28-11t28 11l184 184q6 6 8.5 13t2.5 15q0 8-2.5 15t-8.5 13L404-268q-11 11-28 11t-28-11q-11-11-11-28t11-28l156-156Z" />
    </svg>
);

const MONTHS = ['январь', 'февраль', 'март', 'апрель', 'май', 'июнь', 'июль', 'август', 'сентябрь', 'октябрь', 'ноябрь', 'декабрь'];
const MONTHS_GEN = ['января', 'февраля', 'марта', 'апреля', 'мая', 'июня', 'июля', 'августа', 'сентября', 'октября', 'ноября', 'декабря'];
const WEEKDAYS = ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс'];
const WEEKDAYS_FULL = ['понедельник', 'вторник', 'среда', 'четверг', 'пятница', 'суббота', 'воскресенье'];

const pad2 = (n: number) => String(n).padStart(2, '0');
const dayKey = (d: Date) => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
const addDays = (d: Date, n: number) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
const addMonths = (d: Date, n: number) => {
    const y = d.getFullYear(), m = d.getMonth() + n;
    const last = new Date(y, m + 1, 0).getDate();          // 31 января + месяц → 28/29 февраля
    return new Date(y, m, Math.min(d.getDate(), last), d.getHours(), d.getMinutes());
};
const dowMon = (d: Date) => (d.getDay() + 6) % 7;           // понедельник = 0
const startOfWeek = (d: Date) => addDays(d, -dowMon(d));
const endOfWeek = (d: Date) => addDays(startOfWeek(d), 6);
// Минуты идут шагом 5: время «сейчас» округляем вверх, чтобы срок не оказался уже истёкшим.
const ceil5 = (d: Date) => {
    const t = new Date(d);
    t.setSeconds(0, 0);
    t.setMinutes(t.getMinutes() + ((5 - (t.getMinutes() % 5)) % 5));
    return t;
};
const sameDay = (a: Date | null, b: Date | null) => !!a && !!b && dayKey(a) === dayKey(b);

const toValue = (d: Date, h: number, m: number) => `${dayKey(d)}T${pad2(h)}:${pad2(m)}`;

function parseValue(v: string): { date: Date; h: number; m: number } | null {
    const g = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(v);
    if (!g) return null;
    const date = new Date(+g[1], +g[2] - 1, +g[3]);
    if (Number.isNaN(date.getTime())) return null;
    return { date, h: Math.min(23, +g[4]), m: Math.min(59, +g[5]) };
}
// «21.11.2026 14:30» — как у limitDate, но без обращения к shared: формат поля.
const formatValue = (p: { date: Date; h: number; m: number }) =>
    `${pad2(p.date.getDate())}.${pad2(p.date.getMonth() + 1)}.${p.date.getFullYear()} ${pad2(p.h)}:${pad2(p.m)}`;

export interface DateFieldProps {
    id?: string;
    value: string;
    onChange: (v: string) => void;
    invalid?: boolean;
    describedBy?: string;
    onOpenChange?: (open: boolean) => void;
}

// Поле-кнопка с датой; открывает поповер (на телефоне — нижнюю панель).
export default function DateField({ id, value, onChange, invalid, describedBy, onOpenChange }: DateFieldProps) {
    const [open, setOpen] = useState(false);
    const fieldRef = useRef<HTMLButtonElement>(null);
    const parsed = parseValue(value);

    const toggle = (next: boolean) => { setOpen(next); onOpenChange?.(next); };
    const close = () => { toggle(false); fieldRef.current?.focus({ preventScroll: true }); };

    return (
        <>
            <button
                type="button"
                id={id}
                ref={fieldRef}
                className={`dt-field${invalid ? ' dt-field--invalid' : ''}${parsed ? '' : ' dt-field--empty'}`}
                aria-haspopup="dialog"
                aria-expanded={open}
                aria-invalid={invalid || undefined}
                aria-describedby={describedBy}
                onClick={() => toggle(!open)}
            >
                <span className="dt-field-text">{parsed ? formatValue(parsed) : 'Бессрочно'}</span>
                <IcoCalendar />
            </button>
            {open && (
                <Picker
                    anchor={fieldRef}
                    value={value}
                    onCommit={v => { onChange(v); close(); }}
                    onClose={close}
                />
            )}
        </>
    );
}

interface PickerProps {
    anchor: { current: HTMLButtonElement | null };
    value: string;
    onCommit: (v: string) => void;
    onClose: () => void;
}

function Picker({ anchor, value, onCommit, onClose }: PickerProps) {
    const today = useMemo(() => startOfDay(new Date()), []);
    const init = useMemo(() => parseValue(value), [value]);
    const rounded = useMemo(() => ceil5(new Date()), []);

    const [sel, setSel] = useState<Date | null>(() => (init ? startOfDay(init.date) : null));
    const [active, setActive] = useState<Date>(() => (init ? startOfDay(init.date) : today));
    const [hour, setHour] = useState(() => (init ? init.h : rounded.getHours()));
    const [minute, setMinute] = useState(() => (init ? init.m : rounded.getMinutes()));
    const [view, setView] = useState<Date>(() => {
        const d = init ? init.date : today;
        return new Date(d.getFullYear(), d.getMonth(), 1);
    });

    const popRef = useRef<HTMLDivElement>(null);
    const gridRef = useRef<HTMLDivElement>(null);
    const naturalRef = useRef(0);
    const [pos, setPos] = useState({ top: 0, left: 0, maxH: 0 });
    const monthId = useId();

    // Поповер в портале: диалог лимитов прокручивается (overflow-y: auto) и обрезал бы его.
    // Внизу может не хватить места — тогда поповер либо уезжает вверх, либо поджимается
    // по высоте (середина прокручивается), но поле над ним не накрывает.
    useLayoutEffect(() => {
        const place = () => {
            const r = anchor.current?.getBoundingClientRect();
            const pop = popRef.current;
            if (!r || !pop || window.matchMedia('(max-width: 599px)').matches) return;
            if (!naturalRef.current) naturalRef.current = pop.offsetHeight;
            const h = naturalRef.current;
            const w = pop.offsetWidth;
            const left = Math.min(Math.max(8, r.left), Math.max(8, window.innerWidth - w - 8));
            const below = window.innerHeight - r.bottom - 16;
            const above = r.top - 16;
            if (h <= below || below >= above) {
                setPos({ top: r.bottom + 8, left, maxH: h <= below ? 0 : Math.max(240, below) });
            } else {
                setPos({ top: Math.max(8, r.top - Math.min(h, above) - 8), left, maxH: Math.max(240, above) });
            }
        };
        place();
        window.addEventListener('resize', place);
        document.addEventListener('scroll', place, true);
        return () => {
            window.removeEventListener('resize', place);
            document.removeEventListener('scroll', place, true);
        };
    }, [anchor]);

    // Фокус — на активный день: стрелки и PageUp/PageDown работают сразу после открытия.
    useEffect(() => {
        gridRef.current?.querySelector<HTMLButtonElement>(`[data-day="${dayKey(active)}"]`)
            ?.focus({ preventScroll: true });
    }, [active]);

    // Клик мимо закрывает календарь, но не диалог под ним: первый клик после закрытия
    // гасим, иначе он попадёт в подложку диалога (или в кнопку под поповером).
    useEffect(() => {
        let swallow: ((e: MouseEvent) => void) | null = null;
        const onDown = (e: MouseEvent) => {
            const t = e.target as Node;
            if (popRef.current?.contains(t) || anchor.current?.contains(t)) return;
            onClose();
            swallow = (ev: MouseEvent) => { ev.preventDefault(); ev.stopPropagation(); };
            document.addEventListener('click', swallow, true);
            setTimeout(() => {
                if (swallow) document.removeEventListener('click', swallow, true);
                swallow = null;
            }, 400);
        };
        document.addEventListener('mousedown', onDown, true);
        return () => {
            document.removeEventListener('mousedown', onDown, true);
            if (swallow) document.removeEventListener('click', swallow, true);
        };
    }, [anchor, onClose]);

    const commit = (day: Date) => onCommit(toValue(day, hour, minute));

    const moveActive = (next: Date) => {
        if (startOfDay(next) < today) return;                 // в прошлое не ходим
        setActive(next);
        if (next.getFullYear() !== view.getFullYear() || next.getMonth() !== view.getMonth())
            setView(new Date(next.getFullYear(), next.getMonth(), 1));
    };

    const onGridKey = (e: React.KeyboardEvent) => {
        switch (e.key) {
            case 'ArrowLeft': e.preventDefault(); moveActive(addDays(active, -1)); break;
            case 'ArrowRight': e.preventDefault(); moveActive(addDays(active, 1)); break;
            case 'ArrowUp': e.preventDefault(); moveActive(addDays(active, -7)); break;
            case 'ArrowDown': e.preventDefault(); moveActive(addDays(active, 7)); break;
            case 'PageUp': e.preventDefault(); moveActive(addMonths(active, -1)); break;
            case 'PageDown': e.preventDefault(); moveActive(addMonths(active, 1)); break;
            case 'Home': e.preventDefault(); moveActive(startOfWeek(active)); break;
            case 'End': e.preventDefault(); moveActive(endOfWeek(active)); break;
            case 'Enter': e.preventDefault(); commit(active); break;
        }
    };

    // Esc закрывает только поповер (не диалог лимитов), Tab ходит по кругу внутри него.
    const onRootKey = (e: React.KeyboardEvent) => {
        if (e.key === 'Escape') {
            e.preventDefault();
            e.stopPropagation();
            onClose();
            return;
        }
        if (e.key !== 'Tab' || !popRef.current) return;
        const nodes = popRef.current.querySelectorAll<HTMLElement>(
            'button:not([disabled]):not([tabindex="-1"]), select, [tabindex="0"]',
        );
        if (nodes.length === 0) return;
        const first = nodes[0], last = nodes[nodes.length - 1];
        if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
        else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    };

    const prevMonth = () => setView(new Date(view.getFullYear(), view.getMonth() - 1, 1));
    const nextMonth = () => setView(new Date(view.getFullYear(), view.getMonth() + 1, 1));
    const canPrev = view.getFullYear() > today.getFullYear()
        || (view.getFullYear() === today.getFullYear() && view.getMonth() > today.getMonth());

    // Шесть недель всегда: высота поповера не прыгает между месяцами.
    const weeks: Date[][] = [];
    const firstCell = startOfWeek(new Date(view.getFullYear(), view.getMonth(), 1));
    for (let w = 0; w < 6; w++) weeks.push(Array.from({ length: 7 }, (_, i) => addDays(firstCell, w * 7 + i)));

    const hours = Array.from({ length: 24 }, (_, i) => i);
    // Если у пользователя уже стоит время не кратно пяти (например 23:58), оно должно
    // остаться выбираемым — иначе селект показал бы чужое значение.
    const minutes = Array.from({ length: 12 }, (_, i) => i * 5);
    if (!minutes.includes(minute)) minutes.push(minute);
    minutes.sort((a, b) => a - b);

    const applyPreset = (p: 'month' | 'quarter' | 'year' | 'eom' | 'now' | 'never') => {
        if (p === 'never') { onCommit(''); return; }
        const base = ceil5(new Date());
        const d = p === 'eom'
            ? new Date(base.getFullYear(), base.getMonth() + 1, 0, 23, 55)
            : p === 'month' ? addMonths(base, 1)
                : p === 'quarter' ? addMonths(base, 3)
                    : p === 'year' ? addMonths(base, 12)
                        : base;                                    // «Сейчас»
        onCommit(toValue(d, d.getHours(), d.getMinutes()));
    };

    const pop = (
        <>
            <div className="dt-sheet-scrim" onClick={onClose} aria-hidden="true" />
            <div
                className="dt-pop"
                ref={popRef}
                role="dialog"
                aria-modal="true"
                aria-labelledby={monthId}
                style={{
                    '--dt-top': `${pos.top}px`,
                    '--dt-left': `${pos.left}px`,
                    ...(pos.maxH ? { '--dt-max-h': `${pos.maxH}px` } : {}),
                } as React.CSSProperties}
                onKeyDown={onRootKey}
            >
                <div className="dt-head">
                    <span className="dt-month" id={monthId} aria-live="polite">
                        {MONTHS[view.getMonth()]} {view.getFullYear()}
                    </span>
                    <span className="dt-nav-group">
                        <button type="button" className="dt-nav" onClick={prevMonth} disabled={!canPrev} aria-label="Предыдущий месяц">
                            <IcoChevronLeft />
                        </button>
                        <button type="button" className="dt-nav" onClick={nextMonth} aria-label="Следующий месяц">
                            <IcoChevronRight />
                        </button>
                    </span>
                </div>

                <div className="dt-body">
                    <div className="dt-cal" role="grid" aria-labelledby={monthId} ref={gridRef} onKeyDown={onGridKey}>
                        <div className="dt-week dt-week--head" role="row">
                            {WEEKDAYS.map((w, i) => (
                                <span key={w} className="dt-wd" role="columnheader" aria-label={WEEKDAYS_FULL[i]}>{w}</span>
                            ))}
                        </div>
                        {weeks.map((week, wi) => (
                            <div className="dt-week" role="row" key={wi}>
                                {week.map(d => {
                                    const outside = d.getMonth() !== view.getMonth();
                                    const isToday = sameDay(d, today);
                                    const isSel = sameDay(d, sel);
                                    const isActive = sameDay(d, active);
                                    const past = d < today;
                                    return (
                                        <div key={dayKey(d)} role="gridcell" className="dt-cell"
                                            aria-selected={isSel} aria-current={isToday ? 'date' : undefined}>
                                            <button
                                                type="button"
                                                data-day={dayKey(d)}
                                                tabIndex={isActive ? 0 : -1}
                                                /* текущий (даже истёкший) срок остаётся выбранным и не блокируется,
                                                   иначе для просроченного пользователя фокус падал бы в никуда */
                                                disabled={past && !isSel}
                                                aria-label={`${d.getDate()} ${MONTHS_GEN[d.getMonth()]} ${d.getFullYear()}, ${WEEKDAYS_FULL[dowMon(d)]}`}
                                                className={
                                                    'dt-day'
                                                    + (outside ? ' dt-day--out' : '')
                                                    + (isToday ? ' dt-day--today' : '')
                                                    + (isSel ? ' dt-day--sel' : '')
                                                }
                                                onClick={() => { setSel(d); setActive(d); }}
                                            >
                                                {d.getDate()}
                                            </button>
                                        </div>
                                    );
                                })}
                            </div>
                        ))}
                    </div>

                    <div className="dt-time">
                        <span className="dt-time-label">Время</span>
                        <div className="dt-time-ctl">
                            <select className="dt-select" aria-label="Часы" value={hour}
                                onChange={e => setHour(+e.target.value)}>
                                {hours.map(h => <option key={h} value={h}>{pad2(h)}</option>)}
                            </select>
                            <span className="dt-colon" aria-hidden="true">:</span>
                            <select className="dt-select" aria-label="Минуты" value={minute}
                                onChange={e => setMinute(+e.target.value)}>
                                {minutes.map(m => <option key={m} value={m}>{pad2(m)}</option>)}
                            </select>
                        </div>
                    </div>

                    <div className="dt-presets">
                        <button type="button" className="dt-preset" onClick={() => applyPreset('month')}>Через месяц</button>
                        <button type="button" className="dt-preset" onClick={() => applyPreset('quarter')}>Через 3 месяца</button>
                        <button type="button" className="dt-preset" onClick={() => applyPreset('year')}>Через год</button>
                        <button type="button" className="dt-preset" onClick={() => applyPreset('eom')}>Конец месяца</button>
                        <button type="button" className="dt-preset" onClick={() => applyPreset('now')}>Сейчас</button>
                        <button type="button" className="dt-preset" onClick={() => applyPreset('never')}>Бессрочно</button>
                    </div>
                </div>

                <div className="dt-foot">
                    <button type="button" className="btn btn--text" onClick={onClose}>Отмена</button>
                    <button type="button" className="btn btn--primary" onClick={() => commit(sel ?? active)}>Готово</button>
                </div>
            </div>
        </>
    );

    return createPortal(pop, document.body);
}
