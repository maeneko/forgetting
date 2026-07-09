
import { type ServerInfo } from '../lib/shared';
import { IcoPlus, IcoRefresh } from './icons';

// Сервер-бар: карточка активного сервера + «Добавить сервер» + (на мобильной)
// перезапуск AWG под ними. Общий для вкладок — показывает (и в будущем выбирает)
// сервер, к которому относится содержимое вкладки (пиры / API-ключи).
//   TODO multi-server: сейчас сервер один и всегда активен. Когда серверов
//   станет несколько — карточки станут кликабельными, активная = выбранная.
export default function ServerBar({
    serverInfo, serverOnline, onRestartAwg, restarting,
    showAddServer = true, showRestart = true,
}: {
    serverInfo: ServerInfo | null;
    serverOnline: boolean;
    onRestartAwg: () => void;
    restarting: boolean;
    showAddServer?: boolean;
    showRestart?: boolean;
}) {
    return (
        <div className="server-bar">
            {serverInfo && (
                <div className="server-card server-card--active">
                    {/* toggle — виден на мобильном вместо чипа */}
                    <div
                        className={`server-toggle${serverOnline ? ' server-toggle--on' : ''}`}
                        aria-hidden="true"
                    >
                        <div className="server-toggle-thumb" />
                    </div>
                    {/* чип онлайн/офлайн — виден на десктопе */}
                    <span className={`chip chip--${serverOnline ? 'online' : 'offline'} server-online-chip`}>
                        <span className="chip-dot" />
                    </span>
                    <div className="server-card-info">
                        <span className="server-card-name">{serverInfo.name}</span>
                        <span className="server-card-ip">{serverInfo.ip}</span>
                    </div>
                    <span className="server-card-peers">{serverInfo.peers} peers</span>
                </div>
            )}
            {showAddServer && (
                <span className="tip-wrap" data-tip="Недоступно в альфа-версии">
                    <button className="server-add" disabled>
                        <IcoPlus /> Добавить сервер
                    </button>
                </span>
            )}
            {/* Перезапуск AWG — только на мобильной (на десктопе кнопка в углу). */}
            {showRestart && (
                <button
                    className={`btn btn--tonal awg-restart-mobile${restarting ? ' spinning' : ''}`}
                    onClick={onRestartAwg}
                    disabled={restarting}
                >
                    <IcoRefresh /> {restarting ? 'Перезапуск…' : 'Перезапустить AWG'}
                </button>
            )}
        </div>
    );
}