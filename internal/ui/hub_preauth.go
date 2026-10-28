package ui

// Подпись агента ДО апгрейда на WebSocket (stealth хаба нод). Раньше хаб принимал WebSocket у
// любого и сразу слал hello с nonce: путь /node/v1 есть в открытом коде, и сканер, знающий
// протокол, узнавал хаб одним подключением. Теперь агент кладёт подпись своим ключом в заголовки
// запроса, хаб проверяет её до Accept и без неё отвечает 404 пустого Go-сервера.
//
// Переходный режим: агенты до этой версии заголовков не шлют. Их пускают по-старому, пока в
// панели есть нода, ни разу не подключавшаяся с подписью (nodes.preauth = 0): обновили все —
// путь без подписи закрывается сам. HUB_LEGACY=on / off — принудительно.
//
// Внутри WebSocket протокол прежний (hello → auth → ready): кадр auth обязан быть от той же ноды.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Часы ноды могут расходиться с панелью: окно широкое. Повтор подписи не страшен — заголовки
// идут внутри TLS с закреплённым сертификатом, подсмотреть их нельзя.
const preauthWindow = 24 * 3600

// preauthMsg — что агент подписывает в заголовке X-Node-Sig. Дублируется в internal/agent.
func preauthMsg(nodeID, ts int64) []byte {
	return []byte("awg-node-pre-v1\n" + strconv.FormatInt(nodeID, 10) + "\n" + strconv.FormatInt(ts, 10))
}

// joinOK — одноразовый секрет первой привязки ноды верен и не просрочен.
func joinOK(row *nodeRow, secret string) bool {
	if row.JoinHash == nil || !row.JoinExpires.Valid || row.JoinExpires.Int64 == 0 || row.JoinExpires.Int64 < now() || secret == "" {
		return false
	}
	h := sha256.Sum256(fromB64url(secret))
	return len(row.JoinHash) == len(h) && subtle.ConstantTimeCompare(h[:], row.JoinHash) == 1
}

// preauth — подпись из заголовков. present — агент её прислал (новая версия); row — нода, если
// подпись верна.
func (n *Nodes) preauth(r *http.Request) (row *nodeRow, present bool) {
	idS, tsS, sig := r.Header.Get("X-Node-Id"), r.Header.Get("X-Node-Ts"), r.Header.Get("X-Node-Sig")
	if idS == "" && tsS == "" && sig == "" {
		return nil, false
	}
	id, err1 := strconv.ParseInt(idS, 10, 64)
	ts, err2 := strconv.ParseInt(tsS, 10, 64)
	if err1 != nil || err2 != nil || sig == "" {
		return nil, true
	}
	if d := ts - now(); d > preauthWindow || d < -preauthWindow {
		warnf("nodes: подпись ноды #%d вне окна: часы расходятся на %d с", id, d)
		return nil, true
	}
	row = n.row(id)
	if row == nil {
		return nil, true
	}
	pub := row.Pub.String
	if !row.Pub.Valid {
		// Первое подключение: ключа ноды ещё нет, его приносит агент вместе с секретом привязки.
		if !joinOK(row, r.Header.Get("X-Node-Join")) {
			return nil, true
		}
		pub = r.Header.Get("X-Node-Pub")
	}
	key := fromB64url(pub)
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, preauthMsg(id, ts), fromB64url(sig)) {
		return nil, true
	}
	return row, true
}

// legacyOpen — пускать ли подключение без подписи (агент старой версии).
func (n *Nodes) legacyOpen() bool {
	switch strings.ToLower(os.Getenv("HUB_LEGACY")) {
	case "on":
		return true
	case "off":
		return false
	}
	var waiting int
	n.a.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE preauth = 0
		AND (pub IS NOT NULL OR (join_hash IS NOT NULL AND join_expires >= ?))`, now()).Scan(&waiting)
	return waiting > 0
}
