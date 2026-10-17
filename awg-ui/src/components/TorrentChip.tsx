// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.

// Пометка «торренты»: устройство за последние 10 минут пыталось открыть торрент-соединение, и оно
// было заблокировано. Без флага (блокировка выключена или awg-ctrl старше) чип не показываем.
export default function TorrentChip({ torrent }: { torrent?: boolean }) {
    if (!torrent) return null;
    return (
        <span className="tip-wrap" data-tip="За последние 10 минут пыталось качать торренты: соединения заблокированы, а при делении канала устройство в фоновом приоритете">
            <span className="chip chip--error"><span className="chip-dot" />торренты</span>
        </span>
    );
}
