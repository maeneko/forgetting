// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
// Общие типы, константы и утилиты для всех вкладок панели.
import axios from 'axios';

// Пропсы, которые оболочка передаёт каждой вкладке. Всё остальное (свои данные,
// свой стейт) вкладка берёт сама.
export interface PageProps {
    token:   string;
    showMsg: (text: string) => void;
}

export const TOKEN_KEY = 'admin_token';
export const THEME_KEY = 'admin_theme';

export async function apiFetch(method: string, path: string, token: string, body?: object): Promise<any> {
    const r = await axios({ method, url: path, headers: { Authorization: `Bearer ${token}` }, data: body });
    return r.data;
}

export function downloadFile(filename: string, text: string) {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
    const a   = document.createElement('a');
    a.href    = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
}

export async function copyText(text: string) {
    if (navigator.clipboard) {
        await navigator.clipboard.writeText(text);
    } else {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.cssText = 'position:fixed;opacity:0';
        document.body.appendChild(ta);
        ta.select();
        document.execCommand('copy');
        document.body.removeChild(ta);
    }
}

export function bytes(n: number | undefined): string {
    if (!n) return '—';
    if (n < 1024)          return n + ' B';
    if (n < 1_048_576)     return (n / 1024).toFixed(1) + ' KB';
    if (n < 1_073_741_824) return (n / 1_048_576).toFixed(1) + ' MB';
    return (n / 1_073_741_824).toFixed(2) + ' GB';
}

// unix-секунды → «5 мин» / «3 ч» (без суффикса «назад» — его добавляет вызывающий)
export function timeAgo(ts: number | undefined): string {
    if (!ts) return '—';
    const sec = Math.floor(Date.now() / 1000) - ts;
    if (sec < 60)   return sec + ' сек';
    if (sec < 3600) return Math.floor(sec / 60) + ' мин';
    return Math.floor(sec / 3600) + ' ч';
}
