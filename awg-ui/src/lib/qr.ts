// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import QRCode, { type QRCodeErrorCorrectionLevel } from 'qrcode';

// Фирменный QR: скруглённые модули (соседи сливаются в «капли», одиночный — круг) и угловые
// «глаза» с фиолетовой серединкой. Рисуется в SVG по матрице библиотеки, отдаётся data-URL для <img>.
//
// Цвета — светлой темы и не зависят от текущей: сканеры читают только тёмный код на светлом фоне,
// а токены тёмной темы светлые. Логотипа нет: у vpn:// (уровень 'L') нет запаса коррекции ошибок.
const DARK   = '#3a2e22';   // on-surface светлой темы
const ACCENT = '#6a45b8';   // primary светлой темы
const LIGHT  = '#ffffff';
const MARGIN = 2;           // тихая зона в модулях — как было у toDataURL
const R      = 0.5;         // радиус открытого угла модуля (0.5 — одиночный модуль становится кругом)

// Квадрат 1×1 в (x, y) со скруглением только тех углов, у которых оба соседа по сторонам пусты.
function cell(x: number, y: number, tl: boolean, tr: boolean, br: boolean, bl: boolean): string {
    const a = tl ? R : 0, b = tr ? R : 0, c = br ? R : 0, d = bl ? R : 0;
    return `M${x + a} ${y}H${x + 1 - b}` + (b ? `a${b} ${b} 0 0 1 ${b} ${b}` : '')
         + `V${y + 1 - c}` + (c ? `a${c} ${c} 0 0 1 ${-c} ${c}` : '')
         + `H${x + d}` + (d ? `a${d} ${d} 0 0 1 ${-d} ${-d}` : '')
         + `V${y + a}` + (a ? `a${a} ${a} 0 0 1 ${a} ${-a}` : '') + 'Z';
}

// Скруглённый прямоугольник как подпуть (для «глаз»).
function rrect(x: number, y: number, w: number, h: number, r: number): string {
    return `M${x + r} ${y}H${x + w - r}a${r} ${r} 0 0 1 ${r} ${r}V${y + h - r}a${r} ${r} 0 0 1 ${-r} ${r}`
         + `H${x + r}a${r} ${r} 0 0 1 ${-r} ${-r}V${y + r}a${r} ${r} 0 0 1 ${r} ${-r}Z`;
}

// Бросает, если текст не помещается в QR на этом уровне коррекции.
export function qrDataUrl(text: string, errorCorrectionLevel: QRCodeErrorCorrectionLevel): string {
    const { modules } = QRCode.create(text, { errorCorrectionLevel });
    const n = modules.size;
    const finder = (x: number, y: number) =>
        (x < 7 && y < 7) || (x >= n - 7 && y < 7) || (x < 7 && y >= n - 7);
    const on = (x: number, y: number) =>
        x >= 0 && y >= 0 && x < n && y < n && !finder(x, y) && !!modules.get(y, x);

    // Все модули — одним путём: соседние квадраты сливаются без щелей сглаживания.
    let body = '';
    for (let y = 0; y < n; y++) {
        for (let x = 0; x < n; x++) {
            if (!on(x, y)) continue;
            const up = on(x, y - 1), down = on(x, y + 1), left = on(x - 1, y), right = on(x + 1, y);
            body += cell(x, y, !up && !left, !up && !right, !down && !right, !down && !left);
        }
    }

    // «Глаза»: кольцо 7×7 (внешний минус внутренний контур, evenodd) и серединка 3×3.
    let rings = '', pupils = '';
    for (const [x, y] of [[0, 0], [n - 7, 0], [0, n - 7]]) {
        rings  += rrect(x, y, 7, 7, 2) + rrect(x + 1, y + 1, 5, 5, 1.5);
        pupils += rrect(x + 2, y + 2, 3, 3, 1);
    }

    const s = n + MARGIN * 2;
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="${-MARGIN} ${-MARGIN} ${s} ${s}" width="560" height="560">`
        + `<rect x="${-MARGIN}" y="${-MARGIN}" width="${s}" height="${s}" fill="${LIGHT}"/>`
        + `<path d="${body}" fill="${DARK}"/>`
        + `<path d="${rings}" fill="${DARK}" fill-rule="evenodd"/>`
        + `<path d="${pupils}" fill="${ACCENT}"/>`
        + '</svg>';
    return 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
}
