// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { useState } from 'react';

// Заголовок-«барабан»: при смене текста старый уходит гранью барабана, новый выкатывается на его
// место. order — позиция вкладки в меню: дальше по списку — барабан крутится вверх, обратно — вниз.
// Первый показ — без анимации. Стили — App.css, «Заголовок-барабан».
export default function DrumTitle({ text, order }: { text: string; order: number }) {
    const [s, setS] = useState({ text, order, from: null as string | null, up: true, n: 0 });
    // Текст сменился — запоминаем прежний прямо в рендере, чтобы новый не мелькнул без анимации
    if (s.text !== text) setS({ text, order, from: s.text, up: order >= s.order, n: s.n + 1 });
    return (
        <span key={s.n} className={`drum${s.from !== null ? ` drum--roll drum--${s.up ? 'up' : 'down'}` : ''}`}>
            {s.from !== null && <span className="drum-out" aria-hidden="true">{s.from}</span>}
            <span className="drum-in">{text}</span>
        </span>
    );
}
