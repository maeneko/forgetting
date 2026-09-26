// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
// Общие типы, константы и утилиты для всех вкладок панели.
import axios from 'axios';
// Поколение протокола AmneziaWG, на параметрах которого выдан vpn:// ключ.
export type AwgGen = '2' | '3.1';
export interface User {
    name:          string;
    ip:            string;
    pub_key:       string;
    vpn_key:       string;
    key_gen:       AwgGen;
    online:        boolean;
    lastHandshake: number;
    rx?:           number;
    tx?:           number;
}
export interface ServerInfo {
    name:  string;
    ip:    string;
    peers: number;
    gen:   AwgGen;
    mod:   string;   // версия загруженного модуля amneziawg, из /health awg.module
    tools: string;   // версия awg-tools, из /health awg.tools
}
// На проводе (users.key_gen, /health gen) поколение — '2', человеку показываем '2.0'.
export const genLabel = (g: AwgGen): string => (g === '3.1' ? 'AWG 3.1' : 'AWG 2.0');
// Ключ внешнего API в списке вкладки: открытого значения тут нет — только
// префикс, сервер хранит лишь хэш.
export interface ApiKey {
    id:         number;
    label:      string;
    prefix:     string;
    server_id:  number;
    created_at: number;
    last_used:  number | null;
}

export interface PageProps {
    token:   string;
    showMsg: (text: string) => void;
}
// Имя панели, канал и версия приходят с сервера (GET /ui/brand; значения — из
// .env в корне проекта) — переименование и бамп версии живут в одном файле.
// Эта константа — фолбэк на время загрузки и при недоступности роута.
export const BRAND_FALLBACK = 'Forgetting';
export interface Brand { brand: string; channel: string; version: string }

export const TOKEN_KEY  = 'awg_token';
export const THEME_KEY  = 'awg_theme';
export const SERVER_KEY = 'awg_server';

// Сервер (core или нода) в списке ServerBar: то, что отдаёт GET /ui/nodes.
export interface NodeInfo {
    id:        number;
    name:      string;
    local:     boolean;        // id 0 — локальный awg-ctrl этой же машины
    online:    boolean;
    pending:   boolean;        // нода создана, но ещё ни разу не подключалась
    last_seen: number | null;
    health:    { server: string; ip: string; gen: AwgGen; peers: number; awg_up: boolean; module: string; tools: string } | null;
}

// Выбранный сервер. Вкладки зовут apiFetch без явного id — заголовок X-Server-Id
// подставляется здесь; App ставит значение до смены key у вкладки, поэтому та
// перечитывает данные уже с новым сервером.
let currentServerId: number | null = null;
export const setServerId = (id: number | null) => { currentServerId = id; };
export const getServerId = () => currentServerId;

export async function apiFetch(method: string, path: string, token: string, body?: object): Promise<any> {
    const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
    if (currentServerId !== null) headers['X-Server-Id'] = String(currentServerId);
    const r = await axios({ method, url: path, headers, data: body });
    return r.data;
}

export async function vpnKeyToConf(vpnKey: string): Promise<string> {
    const b64    = vpnKey.replace('vpn://', '').replace(/-/g, '+').replace(/_/g, '/');
    const raw    = Uint8Array.from(atob(b64), c => c.charCodeAt(0));
    const stream = new Blob([raw.slice(4)]).stream().pipeThrough(new DecompressionStream('deflate'));
    const json   = await new Response(stream).text();
    const data   = JSON.parse(json);
    const awg    = data.containers[0].awg;
    try {
        const lc = JSON.parse(awg.last_config || '{}');
        return (lc.config || '')
            .replace('$PRIMARY_DNS',   data.dns1 || '1.1.1.1')
            .replace('$SECONDARY_DNS', data.dns2 || '1.0.0.1');
    } catch {
        return (awg.last_config || '')
            .replace('$PRIMARY_DNS',   data.dns1 || '1.1.1.1')
            .replace('$SECONDARY_DNS', data.dns2 || '1.0.0.1');
    }
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
export function timeAgo(ts: number | undefined): string {
    if (!ts) return '—';
    const sec = Math.floor(Date.now() / 1000) - ts;
    if (sec < 60)   return sec + ' сек';
    if (sec < 3600) return Math.floor(sec / 60) + ' мин';
    return Math.floor(sec / 3600) + ' ч';
}