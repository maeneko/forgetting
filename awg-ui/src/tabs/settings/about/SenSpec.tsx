// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useEffect, useState } from 'react';
import { IcoKey, IcoClose } from '../../../components/icons';
import './sen-spec.css';

// Диалог «Спецификация sen://» из раздела «О панели». Текст — выжимка из docs/sen-link.md и
// awg-ui/sen.ts. «Анатомия» — разбор вставленной ссылки по полям и байтам: формат тот же, что
// у decodeSenLink в sen.ts, но парсинг здесь браузерный (CRC32 таблицей, base64url через atob,
// no node-crypto), поэтому sen.ts не импортируется. Разбор локальный: ссылка никуда не уходит.
// Тексты ошибок повторяют sen.ts, чтобы панель и клиент говорили об одной раскладке.

const SAMPLE = 'sen://AQECBMsAcQeeNEQPdnBuLmV4YW1wbGUubmV0UBXl0rDJofhNbvAN_u0AAAAABwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcLCwsLCwsLCwsLCwsLCwsLCwsLCwsLCwsLCwsLCwsLCwpGb3JnZXR0aW5nKfW-uw';
const SEN_PREFIX = 'sen://';
const FLAG_TLS = 0x01;

// ── CRC32 (IEEE) — та же таблица, что в awg-ui/sen.ts ──────────────────────
const CRC_TABLE = (() => {
    const t = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
        let c = n;
        for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
        t[n] = c >>> 0;
    }
    return t;
})();

function crc32(buf: Uint8Array): number {
    let c = 0xffffffff;
    for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
}

const hexOf = (b: Uint8Array) => [...b].map(x => x.toString(16).padStart(2, '0')).join('');
const hexNum = (n: number) => `0x${n.toString(16).padStart(2, '0')}`;

interface KspField { name: string; from: number; to: number; val: string; raw: string }
interface KspLink { fields: KspField[]; total: number; crcOk: boolean; tls: boolean; payload: string }

// Разбор ссылки по полям: порт decodeSenLink (awg-ui/sen.ts). Ошибки — текстом из него.
function parseLink(link: string): KspLink {
    const t = link.trim();
    if (!t.toLowerCase().startsWith(SEN_PREFIX)) throw new Error('Ссылка должна начинаться с sen://');
    const payload = t.slice(SEN_PREFIX.length).replace(/\s+/g, '');
    if (!/^[A-Za-z0-9_-]+$/.test(payload)) throw new Error('Ссылка повреждена: недопустимые символы');
    let b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    while (b64.length % 4) b64 += '=';
    let raw: Uint8Array;
    try { raw = Uint8Array.from(atob(b64), c => c.charCodeAt(0)); }
    catch { throw new Error('Ссылка повреждена: не прочиталась как base64url'); }
    const total = raw.length;
    if (total < 3 + 4) throw new Error('Ссылка слишком короткая');

    const body = raw.subarray(0, total - 4);
    const stored = (raw[total - 4] << 24 | raw[total - 3] << 16 | raw[total - 2] << 8 | raw[total - 1]) >>> 0;
    const crcOk = stored === crc32(body);

    const fields: KspField[] = [];
    let off = 0;
    const need = (n: number) => { if (off + n > body.length) throw new Error('Ссылка обрезана'); };
    const take = (n: number) => { need(n); const b = body.subarray(off, off + n), from = off; off += n; return { b, from, to: off - 1 }; };
    const put = (name: string, f: ReturnType<typeof take>, val: string) =>
        fields.push({ name, from: f.from, to: f.to, raw: hexOf(f.b), val });

    const version = take(1);
    if (version.b[0] !== 0x01) throw new Error(`Неизвестная версия ссылки: ${version.b[0]}`);
    put('version', version, '0x01');

    const flags = take(1);
    if (flags.b[0] & ~FLAG_TLS) throw new Error(`Неизвестные флаги ссылки: ${hexNum(flags.b[0])}`);
    const tls = (flags.b[0] & FLAG_TLS) !== 0;
    put('flags', flags, tls ? 'bit0 = TLS' : 'без флагов');

    const cnt = take(1);
    const count = cnt.b[0];
    if (count < 1 || count > 3) throw new Error(`Неверное число адресов: ${count}`);
    put('N', cnt, `${count} ${count === 1 ? 'адрес' : count < 5 ? 'адреса' : 'адресов'}`);

    for (let i = 0; i < count; i++) {
        const start = off;
        const type = take(1).b[0];
        let kind: string, host: string;
        if (type === 0x04) {
            kind = 'IPv4';
            host = [...take(4).b].join('.');
        } else if (type === 0x06) {
            kind = 'IPv6';
            host = [...take(16).b].reduce<string[]>((g, x, j) =>
                j % 2 ? (g[g.length - 1] += x.toString(16).padStart(2, '0'), g)
                      : (g.push(x.toString(16).padStart(2, '0')), g), []).join(':');
        } else if (type === 0x44) {
            kind = 'домен';
            const len = take(1).b[0];
            if (len < 1) throw new Error('Пустой домен');
            host = new TextDecoder().decode(take(len).b);
            if (!/^[A-Za-z0-9.-]+$/.test(host)) throw new Error(`Неверный домен: ${host}`);
        } else throw new Error(`Неизвестный тип адреса: ${hexNum(type)}`);
        const pb = take(2).b;
        const port = pb[0] << 8 | pb[1];
        if (port === 0) throw new Error('Порт 0');
        fields.push({ name: `адрес ${i + 1} · ${kind}`, from: start, to: off - 1,
            raw: hexOf(body.subarray(start, off)), val: `${host}:${port}` });
    }

    put('secret', take(16), 'секрет мастер-ключа (поле sub при регистрации)');
    put('sign pub', take(32), 'Ed25519, чем подписаны ответы сервера');
    if (tls) put('tls pin', take(32), 'SHA-256 от SPKI DER сертификата');

    const nameLen = take(1);
    const len = nameLen.b[0];
    if (len > 64) throw new Error('Имя длиннее 64 байт');
    const name = new TextDecoder().decode(take(len).b);
    fields.push({ name: 'name', from: nameLen.from, to: off - 1,
        raw: hexOf(body.subarray(nameLen.from, off)), val: len ? `«${name}»` : 'пусто' });

    const crcRaw = raw.subarray(total - 4);
    fields.push({ name: 'crc32', from: total - 4, to: total - 1, raw: hexOf(crcRaw),
        val: `0x${stored.toString(16).padStart(8, '0')} · ${crcOk ? 'сошлась ✓' : 'не сошлась ✗'}` });

    if (off !== body.length) throw new Error('Лишние данные в конце ссылки');
    return { fields, total, crcOk, tls, payload };
}

// Сегменты ссылки для подсветки: 3 байта = 4 символа base64. Точная граница полей внутри
// символьной группы в base64 невыразима, поэтому «место в ссылке» точен до группы из трёх
// байтов — на стыках полей допускается 1–2 символа соседнего поля.
function linkChunks(l: KspLink): { o: number; from: number; to: number }[] {
    const owner: number[] = [];
    for (const [i, f] of l.fields.entries()) for (let b = f.from; b <= f.to; b++) owner[b] = i;
    const chunks: { o: number; from: number; to: number }[] = [];
    let cur = -1, start = 0;
    for (let g = 0; g < Math.ceil(l.total / 3); g++) {
        const o = owner[g * 3];
        if (o !== cur) {
            if (cur >= 0) chunks.push({ o: cur, from: start, to: g * 4 - 1 });
            cur = o; start = g * 4;
        }
    }
    chunks.push({ o: cur, from: start, to: l.payload.length - 1 });
    return chunks;
}

export default function SenSpec({ onClose }: { onClose: () => void }) {
    const [link, setLink] = useState('');
    const [hot, setHot] = useState<number | null>(null);   // поле под курсором: строка ↔ сегмент ссылки

    // Ошибка — производная состояния: разбор повторяется на каждый ввод, хранить его нигде не надо
    let parsed: KspLink | null = null;
    let error = '';
    if (link.trim()) {
        try { parsed = parseLink(link); }
        catch (e) { error = e instanceof Error ? e.message : String(e); }
    }

    // Escape закрывает, как все диалоги панели
    useEffect(() => {
        const key = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        window.addEventListener('keydown', key);
        return () => window.removeEventListener('keydown', key);
    }, [onClose]);

    return (
        <div className="node-scrim" onClick={onClose}>
            <div className="node-modal ksp-card" role="dialog" aria-modal="true" aria-labelledby="ksp-title" onClick={e => e.stopPropagation()}>
                <button type="button" className="ksp-close" onClick={onClose} aria-label="Закрыть" title="Закрыть (Esc)">
                    <IcoClose />
                </button>
                <p className="node-modal-title" id="ksp-title">Спецификация sen://</p>
                <p className="node-modal-hint ksp-lead">
                    sen:// — ссылка-подписка для SenAWG: <code className="st-code">sen://</code> + base64url от байтов,
                    все числа big-endian. Полная версия — <code className="st-code">docs/sen-link.md</code>, кодек и подписи — <code className="st-code">awg-ui/sen.ts</code>.
                </p>

            <div className="ksp-cols">
                <div className="ksp-col">
                <h4 className="ksp-sub">Раскладка ссылки</h4>
                <div className="ksp-scroll">
                    <table className="ksp-table">
                        <thead><tr><th>Поле</th><th>Байт</th><th>Значение</th></tr></thead>
                        <tbody>
                            <tr><td>version</td><td>1</td><td><code className="st-code">0x01</code>, другая версия отвергается</td></tr>
                            <tr><td>flags</td><td>1</td><td>bit0 = TLS; остальные биты — нули, иначе ссылка отвергается</td></tr>
                            <tr><td>N</td><td>1</td><td>число адресов, 1–3</td></tr>
                            <tr><td>адрес × N</td><td>7–27</td><td><code className="st-code">0x04</code> + 4 байт (IPv4) · <code className="st-code">0x06</code> + 16 байт (IPv6) · <code className="st-code">0x44</code> + len(1) + ASCII (домен), затем порт (2)</td></tr>
                            <tr><td>secret</td><td>16</td><td>секрет мастер-ключа: нужен только при регистрации (поле <code className="st-code">sub</code>)</td></tr>
                            <tr><td>sign pub</td><td>32</td><td>ключ Ed25519, которым подписаны все ответы сервера</td></tr>
                            <tr><td>tls pin</td><td>32</td><td><b>только при bit0</b>: SHA-256 от SPKI DER сертификата сервера</td></tr>
                            <tr><td>name</td><td>1 + len</td><td>UTF-8, ≤ 64 байт, может быть пустым</td></tr>
                            <tr><td>crc32</td><td>4</td><td>CRC-32 (IEEE) от всех предыдущих байт; неверная CRC — ошибка</td></tr>
                        </tbody>
                    </table>
                </div>

                <h4 className="ksp-sub">Модель доверия</h4>
                <ul className="ksp-list">
                    <li>Ответы подписаны всегда: JSON <code className="st-code">{'{"body", "sig"}'}</code>, <code className="st-code">sig</code> — Ed25519 над строкой <code className="st-code">body</code>. Разбирать <code className="st-code">body</code> можно только после проверки подписи.</li>
                    <li>Запросы устройства подписаны его собственным ключом <code className="st-code">auth</code> (Ed25519, отдельный от ключа WireGuard); bearer-токенов нет.</li>
                    <li>TLS необязателен: при bit0 клиент сверяет SHA-256 SPKI сертификата с <code className="st-code">tls pin</code>, CA и hostname не проверяются. Без TLS подписи защищают целостность, но содержимое (secret, конфиг) видно на линии.</li>
                    <li>Эхо запроса в ответе (<code className="st-code">ts</code>, <code className="st-code">device</code>) защищает от подмены чужим подписанным ответом.</li>
                </ul>

                <h4 className="ksp-sub">Подпись запроса</h4>
                <pre className="node-modal-cmd ksp-pre">{`METHOD \\n /path \\n ts \\n hex(sha256(тело))`}</pre>
                <p className="node-modal-hint">
                    В заголовках <code className="st-code">X-Sen-Device</code>, <code className="st-code">X-Sen-Ts</code> и <code className="st-code">X-Sen-Sig</code>;
                    окно <code className="st-code">ts</code> — ±300 с, уже виденная подпись отклоняется, поэтому <code className="st-code">ts</code> слать
                    строго возрастающим: <code className="st-code">ts = max(now, last_ts + 1)</code>.
                </p>

                <h4 className="ksp-sub">Эндпоинты /sub/v1</h4>
                <div className="ksp-scroll">
                    <table className="ksp-table">
                        <thead><tr><th>Метод</th><th>Путь</th><th>Описание</th></tr></thead>
                        <tbody>
                            <tr><td>POST</td><td>/register</td><td>WireGuard <code className="st-code">pub_key</code> и <code className="st-code">auth_pub</code>; 403 — лимит устройств</td></tr>
                            <tr><td>POST</td><td>/peek</td><td>сколько мест занято, до регистрации; без подписи</td></tr>
                            <tr><td>GET</td><td>/config</td><td>актуальный конфиг; обновляет last_seen</td></tr>
                            <tr><td>POST</td><td>/rekey</td><td>новый ключ WireGuard; IP и PSK сохраняются</td></tr>
                            <tr><td>GET</td><td>/devices</td><td>устройства того же мастер-ключа; <code className="st-code">current</code> — это</td></tr>
                            <tr><td>DELETE</td><td>/device</td><td>устройство отвязывает себя; <code className="st-code">410 revoked</code> окончательно</td></tr>
                        </tbody>
                    </table>
                </div>

                <h4 className="ksp-sub">config</h4>
                <pre className="node-modal-cmd ksp-pre">{`{
  "rev": "…",
  "rekey": false,
  "endpoints": ["1.2.3.4:40123"],
  "servers": [
    {
      "id": 0,
      "name": "VPN",
      "endpoint": "1.2.3.4:47619",
      "server_pub": "…",
      "psk": "…",
      "address": "10.9.0.5/32",
      "gen": "3.1",
      "awg": { "Jc": "4", … }
    }
  ]
}`}</pre>
                <p className="node-modal-hint">
                    Приватного ключа WireGuard в конфиге нет и быть не может: он живёт только на устройстве.
                    <code className="st-code"> rev</code> меняется при любом изменении конфига.
                </p>

                <h4 className="ksp-sub">Как устройство получает AWG-conf</h4>
                <ol className="ksp-list">
                    <li><b>Регистрация.</b> <code className="st-code">POST /sub/v1/register</code> с <code className="st-code">sub</code> (секрет из ссылки), публичными ключами WireGuard и auth — устройство получает свой <code className="st-code">device</code> и сразу <code className="st-code">config</code>. Сервер ставит ключ пиром на серверах из набора ключа, каждому — свой IP и PSK; все они приходят в <code className="st-code">servers[]</code>.</li>
                    <li><b>Обновление.</b> Клиент регулярно дергает <code className="st-code">GET /sub/v1/config</code> и <b>сразу при провале handshake</b>. Запрос идёт мимо туннеля: при смене параметров или PSK туннель лежит, пока не придёт свежий конфиг.</li>
                    <li><b>Сверка rev.</b> Не изменился — туннель не трогаем. Изменился — применяем: <code className="st-code">endpoints</code>, новые/ушедшие серверы <code className="st-code">servers[]</code>, параметры обфускации <code className="st-code">awg</code>, DNS, MTU, keepalive.</li>
                    <li><b>Переключение серверов.</b> Один и тот же ключ устройства работает на всех серверах ключа; выбор сервера — за клиентом (ручной или авто-переключение при провале handshake).</li>
                    <li><b>Rekey.</b> Если пришло <code className="st-code">rekey: true</code>, устройство генерирует новую пару WireGuard и шлёт <code className="st-code">POST /sub/v1/rekey</code>; IP и PSK на серверах сохраняются. Ни один сервер ключа не принял ключ — 502, конфиг остаётся прежним.</li>
                    <li><b>Сбои подписки ≠ отозванный доступ.</b> Подпись не сошлась, эхо не совпало — ни в коем случае не действовать по ответу (это может быть подмена); таймаут/5xx — повторять с нарастающей паузой, туннель не трогать; <code className="st-code">410 revoked</code> с совпавшим эхом — окончательно: конфиг и ключи устройства удаляются.</li>
                    <li><b>Пустых конфигов не бывает.</b> Если не готов ни один сервер, сервер отвечает <code className="st-code">502</code>, и клиент остаётся со старым конфигом, продолжая опрашивать.</li>
                </ol>
                </div>

                <div className="ksp-col ksp-col--anatomy">
                <h4 className="ksp-sub">Анатомия ключа</h4>
                <p className="node-modal-hint">
                    Вставь реальную ссылку или пример — она разложится по полям и байтам.
                </p>
                <textarea className="ksp-input" value={link} spellCheck={false} rows={3}
                    placeholder="sen://…" onChange={e => setLink(e.target.value)} />
                <div className="ksp-actions">
                    <button type="button" className="btn btn--text" disabled={!link} onClick={() => setLink('')}>Очистить</button>
                    <button type="button" className="btn btn--tonal" onClick={() => setLink(SAMPLE)}><IcoKey /> Вставить пример</button>
                </div>
                {error && <p className="ksp-error">{error}</p>}
                {parsed && (
                    <>
                        <p className="node-modal-hint ksp-total">
                            {parsed.total} байт · CRC {parsed.crcOk ? 'сошлась ✓' : 'не сошлась ✗'}{parsed.tls && ' · TLS'}
                        </p>
                        {/* сама ссылка с подсветкой: сегмент по 3 байта ↔ строка таблицы (цветным знаком слева) */}
                        <div className={`ksp-visual${hot !== null ? ' ksp-visual--hot' : ''}`}>
                            <span className="ksp-visual-prefix">sen://</span>
                            {linkChunks(parsed).map((c, i) => (
                                <span key={i} className={`ksp-seg ksp-c${c.o % 11}${hot === c.o ? ' ksp-seg--on' : hot !== null ? ' ksp-off' : ''}`}
                                    onMouseEnter={() => setHot(c.o)} onMouseLeave={() => setHot(null)}>
                                    {parsed.payload.slice(c.from, c.to + 1)}
                                </span>
                            ))}
                        </div>
                        <div className="ksp-scroll ksp-anatomy">
                            <table className="ksp-table">
                                <thead><tr><th>Поле</th><th>Байты</th><th>Значение</th><th>HEX</th></tr></thead>
                                <tbody>
                                    {parsed.fields.map((f, i) => (
                                        <tr key={f.name + f.from}
                                            className={`ksp-row${hot === i ? ' ksp-row--on' : hot !== null ? ' ksp-off' : ''}`}
                                            onMouseEnter={() => setHot(i)} onMouseLeave={() => setHot(null)}>
                                            <td><i className="mon-key ksp-key" style={{ background: `var(--ksp-${i % 11})` }} aria-hidden="true" />{f.name}</td>
                                            <td className="ksp-bytes">{f.from}–{f.to}</td>
                                            <td>{f.val}</td>
                                            <td className="ksp-hex">{f.raw}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    </>
                )}
                </div>
            </div>

                <div className="node-modal-actions">
                    <button type="button" className="btn btn--text" onClick={onClose}>Закрыть</button>
                </div>
            </div>
        </div>
    );
}
