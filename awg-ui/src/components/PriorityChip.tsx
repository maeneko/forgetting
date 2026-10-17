// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import type { Priority } from '../lib/shared';

// Текущая приоритетность ключа/устройства. «Фоновый» — только пока канал занят, а устройство давно
// качает заметно больше своей доли: оно уступает остальным, но свободный канал занимает по-прежнему.
// Когда деление канала выключено или канал свободен, у всех «обычный». Без поля (awg-ctrl старше
// деления канала) чип не показываем: «обычный» там было бы выдумкой. С меткой «торренты» устройство
// фоновое независимо от загрузки канала — подсказка говорит об этом.
export default function PriorityChip({ priority, torrent }: { priority?: Priority; torrent?: boolean }) {
    if (!priority) return null;
    const bg = priority === 'background';
    return (
        <span
            className="tip-wrap"
            data-tip={!bg
                ? 'Обычный приоритет: канал делится поровну'
                : torrent
                    ? 'Пытается качать торренты — пока висит метка, устройство уступает остальным'
                    : 'Канал занят, а это устройство давно качает больше своей доли — пока оно уступает остальным'}
        >
            <span className={`chip ${bg ? 'chip--warm' : 'chip--offline'}`}>
                <span className="chip-dot" />{bg ? 'фоновый' : 'обычный'}
            </span>
        </span>
    );
}
