// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
// Тема панели: светлая, тёмная или как в системе. Одно хранилище на всё приложение —
// переключатель в шапке и вкладка «Настройки» читают и меняют его через useTheme().
import { useSyncExternalStore } from 'react';
import { THEME_KEY } from './shared';

export type ThemeMode = 'light' | 'dark' | 'system';
export type Theme = 'light' | 'dark';

const dark = window.matchMedia('(prefers-color-scheme: dark)');
const listeners = new Set<() => void>();

function readMode(): ThemeMode {
    try {
        const v = localStorage.getItem(THEME_KEY);
        if (v === 'dark' || v === 'system') return v;
    } catch { /* не критично */ }
    return 'light';   // без сохранённого выбора — светлая, как было до режима «как в системе»
}

let mode = readMode();
const resolved = (): Theme => (mode === 'system' ? (dark.matches ? 'dark' : 'light') : mode);

function apply() {
    document.documentElement.setAttribute('data-theme', resolved());
    listeners.forEach(l => l());
}
// «Как в системе» следует за сменой темы ОС на лету
dark.addEventListener('change', () => { if (mode === 'system') apply(); });
apply();

export function setThemeMode(next: ThemeMode) {
    mode = next;
    try { localStorage.setItem(THEME_KEY, next); } catch { /* не критично */ }
    apply();
}

const subscribe = (l: () => void) => { listeners.add(l); return () => { listeners.delete(l); }; };

// mode — выбор пользователя, theme — что реально применено.
export function useTheme(): { mode: ThemeMode; theme: Theme } {
    const snap = useSyncExternalStore(subscribe, () => `${mode}:${resolved()}`);
    const [m, t] = snap.split(':') as [ThemeMode, Theme];
    return { mode: m, theme: t };
}
