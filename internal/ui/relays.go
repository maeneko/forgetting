// Релей (роль «релей», схема awgrelay://): записи в реестре nodes с ролью relay; сама
// пересылка стоит на отдельной машине (awg-control relay, cli.env: RELAY_UDP/RELAY_TCP).
// Панель собирает маршруты из «привязок» релея к серверам — UDP на VPN-порт ноды и TCP на
// хаб нод/подписку/панель — и выдаёт их в awgrelay://-строке для install.sh/`awg-ctrl join`.
// Формат строки — docs/node-protocol.md, «Релей».
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/coder/websocket"
)

// relayBind — одна пересылка релея: вид (vpn/hub/sub/ui), порт слушателя и цель.
type relayBind struct {
	Kind     string
	ServerID sql.NullInt64 // для vpn — сервер, чей VPN-порт; прочим kind не нужен
	Listen   int
}

// Таблица привязок: маршруты релея отдельно от rows — их много меняют каскадом,
// а порядок важен (он же порядок в awgrelay://).
const relayBindsSchema = `
CREATE TABLE IF NOT EXISTS relay_binds (
    relay_id  INTEGER NOT NULL,
    idx       INTEGER NOT NULL,
    kind      TEXT    NOT NULL,
    server_id INTEGER,
    listen    INTEGER NOT NULL,
    PRIMARY KEY (relay_id, idx)
);
`

// bindsOf — сохранённые пересылки релея, в порядке добавления.
func (n *Nodes) bindsOf(relayID int64) []relayBind {
	rows, err := n.a.db.Query(`SELECT kind, server_id, listen FROM relay_binds WHERE relay_id = ? ORDER BY idx`, relayID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []relayBind
	for rows.Next() {
		b := relayBind{}
		if err := rows.Scan(&b.Kind, &b.ServerID, &b.Listen); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// setBinds — заменить все пересылки релея одним списком.
func (n *Nodes) setBinds(relayID int64, binds []relayBind) error {
	if _, err := n.a.db.Exec("DELETE FROM relay_binds WHERE relay_id = ?", relayID); err != nil {
		return err
	}
	for i, b := range binds {
		if _, err := n.a.db.Exec(`INSERT INTO relay_binds (relay_id, idx, kind, server_id, listen) VALUES (?, ?, ?, ?, ?)`,
			relayID, i, b.Kind, b.ServerID, b.Listen); err != nil {
			return err
		}
	}
	return nil
}

// targetOf — цель пересылки: для vpn — endpoint сервера из профиля подписки (ui.db); для
// kind самой панели — SUB_HOST и соответствующий порт ядра. Возвращает хост и порт.
func (n *Nodes) targetOf(b relayBind) (string, int, error) {
	if b.Kind != "vpn" {
		host := strings.TrimSpace(os.Getenv("SUB_HOST"))
		if host == "" {
			return "", 0, fmt.Errorf("SUB_HOST не задан — наружный адрес ядра неизвестен")
		}
		port := 0
		switch b.Kind {
		case "hub":
			if !n.hubReady() {
				return "", 0, fmt.Errorf("приём нод не настроен (NODE_PORT / сертификат)")
			}
			port = n.port
		case "sub":
			if port = envNum("SUB_PORT"); port == 0 {
				return "", 0, fmt.Errorf("подписка sen:// не настроена (SUB_PORT)")
			}
		case "ui":
			if port = n.a.port; port == 0 {
				return "", 0, fmt.Errorf("порт панели неизвестен")
			}
		default:
			return "", 0, fmt.Errorf("неизвестный вид привязки")
		}
		return host, port, nil
	}
	prof := n.a.sub.storedProfile(int(b.ServerID.Int64))
	if prof == nil {
		return "", 0, fmt.Errorf("у сервера #%d нет профиля (endpoint) — его awg-ctrl ещё ни разу не отвечал", b.ServerID.Int64)
	}
	epv, _ := prof.Get("endpoint")
	ep, _ := epv.(string)
	if ep == "" {
		return "", 0, fmt.Errorf("у сервера #%d в профиле нет endpoint", b.ServerID.Int64)
	}
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return "", 0, fmt.Errorf("endpoint сервера не разобран: %q", ep)
	}
	pn, _ := strconv.Atoi(port)
	return host, pn, nil
}

// routeListen — порт слушателя: явный из запроса, по умолчанию — порт цели; если он занят
// другой привязкой, берётся следующий свободный (детерминированно, без рандома).
func (n *Nodes) routeListen(binds []relayBind, want, def int) (int, error) {
	used := func(p int) bool {
		for _, b := range binds {
			if b.Listen == p {
				return true
			}
		}
		return false
	}
	if want > 0 && want <= 65535 && !used(want) {
		return want, nil
	}
	for p := def; p <= 65535; p++ {
		if !used(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("нет свободных портов свыше %d", def)
}

// relayRoutes — маршруты релея: строки RELAY_UDP/RELAY_TCP и JSON-массив для awgrelay://.
func (n *Nodes) relayRoutes(relayID int64) (udp, tcp string, list []any, err error) {
	binds := n.bindsOf(relayID)
	list = []any{}
	for _, b := range binds {
		host, port, terr := n.targetOf(b)
		if terr != nil {
			return "", "", nil, terr
		}
		target := net.JoinHostPort(host, strconv.Itoa(port))
		list = append(list, jsjson.Object{{Key: "net", Val: b.Kind}, {Key: "listen", Val: b.Listen}, {Key: "target", Val: target}})
		if b.Kind == "vpn" {
			udp = appendRoute(udp, b.Listen, target)
		} else {
			tcp = appendRoute(tcp, b.Listen, target)
		}
	}
	return udp, tcp, list, nil
}

func appendRoute(list string, port int, target string) string {
	part := strconv.Itoa(port) + "=" + target
	if list == "" {
		return part
	}
	return list + "," + part
}

// relayJoinString — awgrelay://: те же поля, что у awgjoin://, без маршрутов. Привязки
// релей получает по пушу от панели через агента после подключения (автозагрузка — v1,
// строка выписывается на машине один раз).
func (n *Nodes) relayJoinString(host string, id int64, secret []byte) string {
	b := jsjson.MustMarshal(jsjson.Object{{Key: "v", Val: 1}, {Key: "h", Val: host}, {Key: "p", Val: n.port},
		{Key: "pin", Val: b64url.EncodeToString(n.pin)}, {Key: "n", Val: id}, {Key: "s", Val: b64url.EncodeToString(secret)}})
	return "awgrelay://" + b64url.EncodeToString(b)
}

// bindsView — привязки релея для UI-редактора: {kind, server_id?, listen}.
func (n *Nodes) bindsView(relayID int64) []any {
	binds := n.bindsOf(relayID)
	out := make([]any, 0, len(binds))
	for _, b := range binds {
		o := jsjson.Object{{Key: "kind", Val: b.Kind}, {Key: "listen", Val: b.Listen}}
		if b.ServerID.Valid {
			o = append(o, jsjson.KV{Key: "server_id", Val: b.ServerID.Int64})
		}
		out = append(out, o)
	}
	return out
}

// pushRelay — пуш маршрутов живому релею: {t:"relay", udp, tcp} (fire-and-forget, как
// metrics_res у ноды). false — релей не подключён: привязки применятся при его
// переподключении (push при ready-хэншейке).
func (n *Nodes) pushRelay(id int64) bool {
	udp, tcp, _, err := n.relayRoutes(id)
	if err != nil {
		warnf("relays: маршруты релея #%d не собраны: %v", id, err)
		return false
	}
	n.mu.Lock()
	c := n.conns[id]
	n.mu.Unlock()
	if c == nil {
		return false
	}
	msg := jsjson.Object{{Key: "t", Val: "relay"}, {Key: "udp", Val: udp}, {Key: "tcp", Val: tcp}}
	c.ws.Write(context.Background(), websocket.MessageText, jsjson.MustMarshal(msg))
	return true
}

// relayRouter — роуты /ui/relays (за JWT): список, создание, смена привязок, удаление.
func (n *Nodes) relayRouter() *Router {
	if _, err := n.a.db.Exec(relayBindsSchema); err != nil {
		warnf("relays: таблица привязок не открыта: %v", err)
	}
	rt := newRouter()
	rt.Get("/", func(c *Ctx) bool {
		relays := n.RelayList()
		out := make([]any, 0, len(relays))
		for _, o := range relays {
			o = append(o, jsjson.KV{Key: "binds", Val: n.bindsView(o.Get2("id").(int64))})
			out = append(out, o)
		}
		return c.JSON(200, jsjson.Object{{Key: "hub", Val: n.hubReady()}, {Key: "relays", Val: out}})
	})
	rt.Post("/", func(c *Ctx) bool {
		if !n.hubReady() {
			return c.Error(503, "Приём нод не настроен (NODE_PORT / сертификат) — релею нужен агент со связью с панелью")
		}
		name, ok := c.Obj().Get2("name").(string)
		if !ok || !reNodeName.MatchString(name) || !lenOK(name, 1, 40) {
			return c.Error(400, "Имя: до 40 символов, без переводов строк и <>")
		}
		binds, err := n.parseBinds(c.Obj().Get2("binds"))
		if err != nil {
			return c.Error(400, err.Error())
		}
		host := n.hostOf(c)
		if !reHost.MatchString(host) {
			return c.Error(400, "Не удалось определить адрес core")
		}
		secret := make([]byte, 16)
		rand.Read(secret)
		h := sha256.Sum256(secret)
		res, err := n.a.db.Exec("INSERT INTO nodes (name, join_hash, join_expires, created_at, role) VALUES (?, ?, ?, ?, 'relay')", name, h[:], now()+joinTTL, now())
		if err != nil {
			return c.Error(500, "Internal server error")
		}
		id, _ := res.LastInsertId()
		if err := n.setBinds(id, binds); err != nil {
			n.a.db.Exec("DELETE FROM nodes WHERE id = ?", id)
			return c.Error(500, "Не удалось сохранить привязки")
		}
		return c.JSON(201, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: name},
			{Key: "join", Val: n.relayJoinString(host, id, secret)}, {Key: "expires_in", Val: joinTTL}})
	})
	// Смена привязок: у ещё не подключившегося релея — вместе с переписью join-строки
	// (секрет одноразовый), у подключённого — пуш свежих маршрутов агенту. Пуш может не
	// пройти (релей offline): привязки сохранены, применятся при его подключении.
	rt.Post("/:id/bind", func(c *Ctx) bool {
		id, ok := n.idOf(c)
		var row *nodeRow
		if ok {
			row = n.row(id)
		}
		if row == nil || !row.isRelay() {
			return c.Error(404, "Не найден")
		}
		binds, err := n.parseBinds(c.Obj().Get2("binds"))
		if err != nil {
			return c.Error(400, err.Error())
		}
		if err := n.setBinds(id, binds); err != nil {
			return c.Error(500, "Не удалось сохранить привязки")
		}
		if row.Pub.Valid {
			pushed := n.pushRelay(id)
			note := ""
			if !pushed {
				note = "Релей не на связи — привязки сохранены и применятся, когда он подключится"
			}
			return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: row.Name}, {Key: "pushed", Val: pushed}, {Key: "note", Val: note}})
		}
		host := n.hostOf(c)
		if !reHost.MatchString(host) {
			return c.Error(400, "Не удалось определить адрес core")
		}
		secret := make([]byte, 16)
		rand.Read(secret)
		h := sha256.Sum256(secret)
		n.a.db.Exec("UPDATE nodes SET join_hash = ?, join_expires = ? WHERE id = ? AND pub IS NULL", h[:], now()+joinTTL, id)
		return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: row.Name},
			{Key: "join", Val: n.relayJoinString(host, id, secret)}, {Key: "expires_in", Val: joinTTL}})
	})
	// Перевыписать строку подключения: релей ещё не привязал ключ (секрет одноразовый).
	rt.Post("/:id/join", func(c *Ctx) bool {
		id, ok := n.idOf(c)
		var row *nodeRow
		if ok {
			row = n.row(id)
		}
		if row == nil || !row.isRelay() {
			return c.Error(404, "Не найден")
		}
		if row.Pub.Valid {
			return c.Error(409, "Релей уже подключён — строка не нужна")
		}
		if !n.hubReady() {
			return c.Error(503, "Приём нод не настроен (NODE_PORT / сертификат)")
		}
		host := n.hostOf(c)
		if !reHost.MatchString(host) {
			return c.Error(400, "Не удалось определить адрес core")
		}
		secret := make([]byte, 16)
		rand.Read(secret)
		h := sha256.Sum256(secret)
		n.a.db.Exec("UPDATE nodes SET join_hash = ?, join_expires = ? WHERE id = ? AND pub IS NULL", h[:], now()+joinTTL, id)
		return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: row.Name},
			{Key: "join", Val: n.relayJoinString(host, id, secret)}, {Key: "expires_in", Val: joinTTL}})
	})
	rt.Delete("/:id", func(c *Ctx) bool {
		id, ok := n.idOf(c)
		var row *nodeRow
		if ok {
			row = n.row(id)
		}
		if row == nil || !row.isRelay() {
			return c.Error(404, "Не найден")
		}
		n.mu.Lock()
		conn := n.conns[id]
		n.mu.Unlock()
		if conn != nil {
			n.dropConn(id, conn)
			closeWS(conn.ws, 4004, "removed")
		}
		n.a.db.Exec("DELETE FROM nodes WHERE id = ?", id)
		n.a.db.Exec("DELETE FROM relay_binds WHERE relay_id = ?", id)
		return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: id}})
	})
	return rt
}

// parseBinds — binds из запроса: [{kind, server_id?, listen?}]. Порт слушателя берётся из
// запроса, по умолчанию — порт цели; занятая другим bind'ом — следующий свободный. Все цели
// проверяются сразу: профиль сервера или наружный адрес ядра (SUB_HOST). Пустой список
// легален — релей подключается без маршрутов, привязываются серверы после (пуш агенту).
func (n *Nodes) parseBinds(v any) ([]relayBind, error) {
	in, _ := v.([]any)
	if len(in) > 8 {
		return nil, fmt.Errorf("binds: до 8 пересылок")
	}
	var binds []relayBind
	for _, raw := range in {
		m, ok := raw.(jsjson.Object)
		if !ok {
			return nil, fmt.Errorf("binds: неверный элемент")
		}
		kind, _ := m.Get2("kind").(string)
		switch kind {
		case "vpn", "hub", "sub", "ui":
		default:
			return nil, fmt.Errorf("binds: kind должен быть vpn, hub, sub или ui")
		}
		b := relayBind{Kind: kind}
		var want int
		if isInt(m.Get2("listen")) {
			want = int(m.Get2("listen").(float64))
		}
		if isInt(m.Get2("server_id")) && m.Get2("server_id").(float64) != 0 {
			b.ServerID = sql.NullInt64{Int64: int64(m.Get2("server_id").(float64)), Valid: true}
		}
		if kind == "vpn" {
			if !b.ServerID.Valid {
				return nil, fmt.Errorf("binds: для vpn нужен server_id")
			}
			// 0 — свой сервер панели: строки в nodes он не имеет (id 0 синтезируется в UI-списке),
			// его endpoint проверит targetOf через server_profiles.
			if b.ServerID.Int64 != 0 {
				row := n.row(b.ServerID.Int64)
				if row == nil || row.isRelay() {
					return nil, fmt.Errorf("binds: сервер #%d не найден", b.ServerID.Int64)
				}
			} else if !n.local {
				return nil, fmt.Errorf("binds: сервер #0 не найден")
			}
		} else if b.ServerID.Valid {
			return nil, fmt.Errorf("binds: server_id задаётся только для kind vpn")
		}
		_, port, terr := n.targetOf(relayBind{Kind: kind, ServerID: b.ServerID})
		if terr != nil {
			return nil, terr
		}
		listen, lerr := n.routeListen(binds, want, port)
		if lerr != nil {
			return nil, lerr
		}
		b.Listen = listen
		binds = append(binds, b)
	}
	return binds, nil
}
