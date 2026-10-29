package ui

// Несколько серверов под одной панелью (порт awg-ui/nodes.ts): core (эта панель) и ноды —
// VPN-серверы, которые сами открывают к core исходящее WSS-соединение (awg-control agent).
// Реестр нод лежит в ui.db, RPC к ноде идёт по этому соединению, а нода исполняет запрос на
// своём 127.0.0.1-awg-ctrl. awg-ctrl про ноды ничего не знает.
//
// server_id 0 — локальный awg-ctrl этой же машины; при LOCAL_NODE=off его нет, core работает
// как чистая панель. Номер 0 тогда может занять нода — сервер, с которого панель переехала
// (`awg-control migrate`): так у его пиров, ключей API и устройств не меняется id сервера.
// Протокол и формат join-строки — docs/node-protocol.md.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	joinTTL     = 3600 // секунд жизни одноразового секрета
	authTimeout = 10 * time.Second
	healthEvery = 15 * time.Second
	pingEvery   = 20 * time.Second
	rpcTimeout  = 15 * time.Second
	rpcSlow     = 120 * time.Second // /awg/*: рестарт и upgrade идут долго
	hubMaxFrame = 4 * 1024 * 1024
	// Метрики для Prometheus на ноде: каждый ответ опрашивает все серверы — не чаще этого.
	metricsEvery = 5 * time.Second
	failWindow   = 15 * time.Minute
	failMaxHits  = 20
)

var (
	reNodeName = regexp.MustCompile(`^[^\r\n<>]+$`) // длину — lenOK: в JS {1,40} считает UTF-16
	reHost     = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
	reVersion  = regexp.MustCompile(`^[\w.+-]{1,32}$`)
)

// authMsg — сообщение, которое нода подписывает своим ключом. Дублируется в internal/agent.
func authMsg(nonce string, nodeID int64) []byte {
	return []byte("awg-node-v1\n" + nonce + "\n" + strconv.FormatInt(nodeID, 10))
}

type nodeRow struct {
	ID          int64
	Name        string
	Pub         sql.NullString
	JoinHash    []byte
	JoinExpires sql.NullInt64
	CreatedAt   int64
	LastSeen    sql.NullInt64
	Version     sql.NullString
}

const nodeCols = "id, name, pub, join_hash, join_expires, created_at, last_seen, version"

func scanNode(row interface{ Scan(...any) error }) (*nodeRow, error) {
	n := &nodeRow{}
	if err := row.Scan(&n.ID, &n.Name, &n.Pub, &n.JoinHash, &n.JoinExpires, &n.CreatedAt, &n.LastSeen, &n.Version); err != nil {
		return nil, err
	}
	return n, nil
}

// health — сводка /health сервера; ip — как пришло (`d.ip ?? ""`).
type health struct {
	Server string
	IP     any
	Gen    any
	Peers  any
	AwgUp  bool
	Module any
	Tools  any
}

func (h *health) json() any {
	if h == nil {
		return nil
	}
	return jsjson.Object{{Key: "server", Val: h.Server}, {Key: "ip", Val: h.IP}, {Key: "gen", Val: h.Gen}, {Key: "peers", Val: h.Peers},
		{Key: "awg_up", Val: h.AwgUp}, {Key: "module", Val: h.Module}, {Key: "tools", Val: h.Tools}}
}

type pending struct {
	ch    chan *CtrlResp
	timer *time.Timer
}

type nodeConn struct {
	ws      *websocket.Conn
	mu      sync.Mutex // seq, pending, alive
	seq     int64
	alive   bool
	pending map[int64]*pending
	closed  bool
	// Запрос метрик от ноды (Prometheus на ней): идёт ли сейчас и когда начат последний.
	metricsBusy bool
	metricsAt   time.Time
}

type deleteVeto struct {
	Error    string
	Forcible bool
}

type Nodes struct {
	a        *App
	port     int
	local    bool
	tls      *tls.Config
	pin      []byte
	mu       sync.Mutex
	conns    map[int64]*nodeConn
	health   map[int64]*health
	failHits map[string]*rateEntry

	hmu           sync.Mutex
	onlineHandler []func(id int)
	beforeDelete  []func(id int, force bool) *deleteVeto
	afterDelete   []func(id int)
}

var offline = &CtrlResp{502, errObj("Сервер недоступен")}

func newNodes(a *App) (*Nodes, error) {
	n := &Nodes{
		a: a, port: envNum("NODE_PORT"),
		local: strings.ToLower(envOr("LOCAL_NODE", "on")) != "off",
		conns: map[int64]*nodeConn{}, health: map[int64]*health{}, failHits: map[string]*rateEntry{},
	}
	file := func(env, name string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return filepath.Join(a.baseDir, name)
	}
	// TLS: самоподписанный сертификат, pin которого зашит в join-строки. Имя в нём случайное:
	// одинаковое у всех серверов было бы сигнатурой для сканера (pin — по ключу, не по имени). Живёт столько же,
	// сколько core: пересоздать = переподключить все ноды.
	if n.port != 0 {
		keyF, crtF := file("NODE_TLS_KEY_FILE", "node_tls.key"), file("NODE_TLS_CERT_FILE", "node_tls.crt")
		if err := func() error {
			if !exists(keyF) && !exists(crtF) {
				cmd := exec.Command("openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
					"-keyout", keyF, "-out", crtF, "-days", "3650", "-subj", "/CN="+randomCN())
				if err := cmd.Run(); err != nil {
					return err
				}
				os.Chmod(keyF, 0o600)
				os.Chmod(crtF, 0o600)
				logf("nodes: создан самоподписанный сертификат %s", crtF)
			}
			crt, err := os.ReadFile(crtF)
			if err != nil {
				return err
			}
			cert, err := tls.LoadX509KeyPair(crtF, keyF)
			if err != nil {
				return err
			}
			pin, err := spkiPin(crt)
			if err != nil {
				return err
			}
			n.tls = &tls.Config{Certificates: []tls.Certificate{cert}}
			n.pin = pin
			return nil
		}(); err != nil {
			warnf("nodes: сертификат хаба недоступен (%s) — приём нод отключён", crtF)
			n.tls, n.pin = nil, nil
		}
	}

	if _, err := a.db.Exec(`
        CREATE TABLE IF NOT EXISTS nodes (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            name         TEXT    NOT NULL,
            pub          TEXT,
            join_hash    BLOB,
            join_expires INTEGER,
            created_at   INTEGER NOT NULL,
            last_seen    INTEGER
        );
    `); err != nil {
		return nil, err
	}
	// Версия Forgetting на ноде — из кадра auth (агенты до 0.3.4 её не присылают).
	if !hasColumn(a.db, "nodes", "version") {
		if _, err := a.db.Exec("ALTER TABLE nodes ADD COLUMN version TEXT"); err != nil {
			return nil, err
		}
	}
	// core_ack — адрес панели, который агент записал к себе (кадр core_ok): нода переживёт
	// выключение сервера, через который подключена сейчас.
	if !hasColumn(a.db, "nodes", "core_ack") {
		if _, err := a.db.Exec("ALTER TABLE nodes ADD COLUMN core_ack TEXT"); err != nil {
			return nil, err
		}
	}
	// preauth — нода хоть раз подключилась с подписью в заголовках (hub_preauth.go): пока такие
	// есть не все, хаб пускает и старых агентов без подписи.
	if !hasColumn(a.db, "nodes", "preauth") {
		if _, err := a.db.Exec("ALTER TABLE nodes ADD COLUMN preauth INTEGER NOT NULL DEFAULT 0"); err != nil {
			return nil, err
		}
	}
	// Нода №0 и локальный сервер — один и тот же id: вместе их не бывает.
	if n.local && n.row(0) != nil {
		return nil, fmt.Errorf("nodes: в ui.db есть нода #0 (сервер, с которого переехала панель), а LOCAL_NODE=on — " +
			"у этой панели свой VPN не включают")
	}
	return n, nil
}

func exists(f string) bool {
	_, err := os.Stat(f)
	return err == nil
}

func hasColumn(db queryer, table, col string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		rows.Scan(ptrs...)
		for i, c := range cols {
			if c == "name" && fmt.Sprint(vals[i]) == col {
				return true
			}
		}
	}
	return false
}

func (n *Nodes) row(id int64) *nodeRow {
	r, _ := scanNode(n.a.db.QueryRow("SELECT "+nodeCols+" FROM nodes WHERE id = ?", id))
	return r
}

func (n *Nodes) all() []*nodeRow {
	rows, err := n.a.db.Query("SELECT " + nodeCols + " FROM nodes ORDER BY id")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*nodeRow
	for rows.Next() {
		if r, err := scanNode(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// ── RPC к нодам ─────────────────────────────────────────────────────────────

func (n *Nodes) rpc(id int64, method, path string, body any) *CtrlResp {
	n.mu.Lock()
	c := n.conns[id]
	n.mu.Unlock()
	if c == nil {
		return offline
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return offline
	}
	c.seq++
	rid := c.seq
	p := &pending{ch: make(chan *CtrlResp, 1)}
	timeout := rpcTimeout
	if strings.HasPrefix(path, "/awg/") {
		timeout = rpcSlow
	}
	p.timer = time.AfterFunc(timeout, func() {
		c.mu.Lock()
		delete(c.pending, rid)
		c.mu.Unlock()
		p.ch <- &CtrlResp{504, errObj("Сервер не ответил")}
	})
	c.pending[rid] = p
	c.mu.Unlock()

	msg := jsjson.Object{{Key: "t", Val: "req"}, {Key: "id", Val: rid}, {Key: "method", Val: method}, {Key: "path", Val: path}}
	if body != nil {
		msg = append(msg, jsjson.KV{Key: "body", Val: body})
	}
	if b, err := jsjson.Marshal(msg); err == nil {
		go c.ws.Write(context.Background(), websocket.MessageText, b)
	}
	return <-p.ch
}

// CtrlFor — контроллер нужного сервера: 0 — локальный awg-ctrl, остальное — нода по WSS.
func (n *Nodes) CtrlFor(id int) Ctrl {
	if id == 0 && n.local {
		return n.a.ctrlLocal
	}
	if n.row(int64(id)) == nil {
		if id == 0 {
			return func(string, string, any) (*CtrlResp, error) {
				return &CtrlResp{404, errObj("Локальный сервер не настроен")}, nil
			}
		}
		return func(string, string, any) (*CtrlResp, error) {
			return &CtrlResp{404, errObj("Сервер не найден")}, nil
		}
	}
	return func(m, p string, b any) (*CtrlResp, error) { return n.rpc(int64(id), m, p, b), nil }
}

func (n *Nodes) Exists(id int) bool {
	if id == 0 && n.local {
		return true
	}
	return n.row(int64(id)) != nil
}

// IsOnline — есть ли сейчас связь (локальный считаем доступным всегда).
func (n *Nodes) IsOnline(id int) bool {
	if id == 0 && n.local {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conns[int64(id)] != nil
}

// IDs — все серверы реестра: локальный (если есть) и ноды.
func (n *Nodes) IDs() []int {
	var ids []int
	if n.local {
		ids = append(ids, 0)
	}
	for _, r := range n.all() {
		ids = append(ids, int(r.ID))
	}
	return ids
}

// OnOnline — подписчики на «сервер снова на связи» (подписка догоняет пропущенное).
func (n *Nodes) OnOnline(cb func(id int)) {
	n.hmu.Lock()
	n.onlineHandler = append(n.onlineHandler, cb)
	n.hmu.Unlock()
}

// BeforeDelete — проверка перед удалением сервера: может запретить (forcible — запрет
// снимается подтверждением оператора). AfterDelete — уборка ссылок на уже удалённый сервер.
func (n *Nodes) BeforeDelete(cb func(id int, force bool) *deleteVeto) {
	n.hmu.Lock()
	n.beforeDelete = append(n.beforeDelete, cb)
	n.hmu.Unlock()
}

func (n *Nodes) AfterDelete(cb func(id int)) {
	n.hmu.Lock()
	n.afterDelete = append(n.afterDelete, cb)
	n.hmu.Unlock()
}

func (n *Nodes) fireOnline(id int) {
	n.hmu.Lock()
	hs := append([]func(int){}, n.onlineHandler...)
	n.hmu.Unlock()
	for _, cb := range hs {
		func() {
			defer func() { recover() }() // подписчик сам разберётся
			cb(id)
		}()
	}
}

// DefaultID — сервер по умолчанию: локальный, а без него — первая нода.
func (n *Nodes) DefaultID() int {
	if n.local {
		return 0
	}
	if all := n.all(); len(all) > 0 {
		return int(all[0].ID)
	}
	return 0
}

func (n *Nodes) refreshHealth(id int64) {
	var r *CtrlResp
	if id == 0 && n.local {
		r, _ = n.a.ctrlLocal("GET", "/health", nil)
	} else {
		r = n.rpc(id, "GET", "/health", nil)
	}
	var d any
	if r != nil {
		d = r.Data
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if server, ok := field(d, "server").(string); ok {
		or := func(v any, def any) any {
			if v == nil {
				return def
			}
			return v
		}
		n.health[id] = &health{
			Server: server, IP: or(field(d, "ip"), ""), Gen: or(field(d, "gen"), "2"), Peers: or(field(d, "awg", "peers"), 0.0),
			AwgUp: field(d, "awg", "status") == "ok", Module: or(field(d, "awg", "module"), ""), Tools: or(field(d, "awg", "tools"), ""),
		}
	} else {
		delete(n.health, id)
	}
}

// ── Хаб: приём соединений от нод ────────────────────────────────────────────

func (n *Nodes) failed(ip string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := nowFn()
	e := n.failHits[ip]
	if e == nil || e.resetAt.Before(t) {
		n.failHits[ip] = &rateEntry{1, t.Add(failWindow)}
	} else {
		e.count++
	}
}

func (n *Nodes) blocked(ip string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.failHits[ip]
	return e != nil && e.resetAt.After(nowFn()) && e.count >= failMaxHits
}

// dropConn — все ждущие запросы получают «недоступен»; из реестра соединений — только если
// там всё ещё это соединение.
func (n *Nodes) dropConn(id int64, c *nodeConn) {
	c.mu.Lock()
	c.closed = true
	ps := c.pending
	c.pending = map[int64]*pending{}
	c.mu.Unlock()
	for _, p := range ps {
		if p.timer.Stop() {
			p.ch <- offline
		}
	}
	n.mu.Lock()
	if n.conns[id] == c {
		delete(n.conns, id)
		delete(n.health, id)
	}
	n.mu.Unlock()
}

// MinNodeVersion — самая старая версия ноды, с которой работает панель (в UI — MIN_NODE_VERSION).
const MinNodeVersion = "0.5.0"

// closeOutdated — код закрытия для ноды старше MinNodeVersion.
const closeOutdated = 4026

// cmpVersion сравнивает версии вида 0.5.0 (хвост после «-» не учитывается): <0 — a старше b.
func cmpVersion(a, b string) int {
	// «Beta 0.5.0» → «0.5.0»: канал перед номером не сравниваем.
	if f := strings.Fields(a); len(f) > 0 {
		a = f[len(f)-1]
	}
	if f := strings.Fields(b); len(f) > 0 {
		b = f[len(f)-1]
	}
	pa, pb := strings.Split(strings.SplitN(a, "-", 2)[0], "."), strings.Split(strings.SplitN(b, "-", 2)[0], ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func closeWS(ws *websocket.Conn, code int, reason string) {
	go ws.Close(websocket.StatusCode(code), reason)
}

func (n *Nodes) serveWS(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	// До апгрейда: без верной подписи ноды (или старого агента в переходном режиме) — 404, как
	// у пустого сервера; WebSocket и hello видит только своя нода (hub_preauth.go).
	if n.blocked(ip) {
		http.NotFound(w, r)
		return
	}
	pre, present := n.preauth(r)
	if present && pre == nil {
		n.failed(ip)
		http.NotFound(w, r)
		return
	}
	if !present && !n.legacyOpen() {
		http.NotFound(w, r)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(hubMaxFrame)
	nb := make([]byte, 16)
	rand.Read(nb)
	nonce := b64url.EncodeToString(nb)

	var conn *nodeConn
	var nodeID int64
	authTimer := time.AfterFunc(authTimeout, func() { closeWS(ws, 4001, "auth_timeout") })
	defer authTimer.Stop()
	deny := func() { n.failed(ip); closeWS(ws, 4003, "unauthorized") }

	authenticate := func(m jsjson.Object) {
		sig, sigOK := m.Get2("sig").(string)
		if m.Get2("t") != "auth" || !isInt(m.Get2("node_id")) || !sigOK {
			deny()
			return
		}
		row := n.row(int64(m.Get2("node_id").(float64)))
		if row == nil || pre != nil && pre.ID != row.ID {
			deny()
			return
		}
		pubB64 := row.Pub.String
		joining := !row.Pub.Valid
		if joining {
			secret, _ := field(m, "join", "secret").(string)
			pub, pOK := field(m, "join", "pub").(string)
			if !pOK || !joinOK(row, secret) {
				deny()
				return
			}
			pubB64 = pub
		}
		key := fromB64url(pubB64)
		if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, authMsg(nonce, row.ID), fromB64url(sig)) {
			deny()
			return
		}
		authTimer.Stop()
		if joining {
			n.a.db.Exec("UPDATE nodes SET pub = ?, join_hash = NULL, join_expires = NULL WHERE id = ?", pubB64, row.ID)
		}
		// Версия — только для показа в панели: не подписана, но и доверия не даёт.
		var version any
		if v, ok := m.Get2("version").(string); ok && reVersion.MatchString(v) {
			version = v
		}
		n.a.db.Exec("UPDATE nodes SET last_seen = ?, version = ? WHERE id = ?", now(), version, row.ID)
		// Нода старше MinNodeVersion (или без версии — агент старше 0.3.4) с этой панелью не работает:
		// соединение не принимаем, для панели она «нет связи», пока её не обновят (агент переподключится сам).
		if v, _ := version.(string); v == "" || cmpVersion(v, MinNodeVersion) < 0 {
			logf("nodes: «%s» (#%d) — версия %q, нужна не ниже %s: соединение отклонено", row.Name, row.ID, v, MinNodeVersion)
			closeWS(ws, closeOutdated, "outdated "+MinNodeVersion)
			return
		}
		if pre != nil {
			n.a.db.Exec("UPDATE nodes SET preauth = 1 WHERE id = ?", row.ID)
		}

		conn = &nodeConn{ws: ws, alive: true, pending: map[int64]*pending{}}
		nodeID = row.ID
		n.mu.Lock()
		old := n.conns[row.ID]
		n.conns[row.ID] = conn
		n.mu.Unlock()
		if old != nil {
			n.dropConn(row.ID, old)
			closeWS(old.ws, 4000, "replaced")
		}
		ready := jsjson.Object{{Key: "t", Val: "ready"}}
		if home := n.home(); home != "" {
			ready = append(ready, jsjson.KV{Key: "core", Val: home})
		}
		ws.Write(context.Background(), websocket.MessageText, jsjson.MustMarshal(ready))
		first := ""
		if joining {
			first = " (первое подключение)"
		}
		if pre == nil {
			first += " — без подписи до апгрейда: старый агент, обновите ноду, чтобы хаб скрылся от сканеров"
		}
		logf("nodes: «%s» (#%d) подключена%s", row.Name, row.ID, first)
		go n.refreshHealth(row.ID)
		go n.fireOnline(int(row.ID))
	}

	hello, _ := jsjson.Marshal(jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: nonce}})
	ws.Write(r.Context(), websocket.MessageText, hello)

	ctx := context.Background()
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			break
		}
		v, err := jsjson.Decode(raw)
		if err != nil {
			closeWS(ws, 4002, "bad_frame")
			continue
		}
		m, _ := v.(jsjson.Object)
		if conn == nil {
			authenticate(m)
			continue
		}
		if m.Get2("t") == "core_ok" {
			if home, ok := m.Get2("core").(string); ok && home == n.home() {
				n.a.db.Exec("UPDATE nodes SET core_ack = ? WHERE id = ?", home, nodeID)
			}
			continue
		}
		if m.Get2("t") == "metrics" && isInt(m.Get2("id")) {
			go n.nodeMetrics(conn, m.Get2("id"))
			continue
		}
		if m.Get2("t") == "res" && isInt(m.Get2("id")) {
			rid := int64(m.Get2("id").(float64))
			conn.mu.Lock()
			p := conn.pending[rid]
			delete(conn.pending, rid)
			conn.mu.Unlock()
			if p == nil || !p.timer.Stop() {
				continue
			}
			st := jsToNumberAny(m.Get2("status"))
			status := 502
			if st == st && st != 0 {
				status = int(st)
			}
			p.ch <- &CtrlResp{status, m.Get2("data")}
		}
	}
	if conn != nil {
		n.dropConn(nodeID, conn)
		logf("nodes: #%d отключена", nodeID)
	}
}

// nodeMetrics — Prometheus на ноде скрейпит метрики панели через её агента: та же экспозиция,
// что /metrics ядра, со всеми серверами (docs/node-protocol.md, «Мониторинг на ноде»).
func (n *Nodes) nodeMetrics(c *nodeConn, id any) {
	c.mu.Lock()
	t := time.Now()
	busy := c.metricsBusy || t.Sub(c.metricsAt) < metricsEvery
	if !busy {
		c.metricsBusy, c.metricsAt = true, t
	}
	c.mu.Unlock()
	res := jsjson.Object{{Key: "t", Val: "metrics_res"}, {Key: "id", Val: id}}
	if busy {
		res = append(res, jsjson.KV{Key: "status", Val: 429}, jsjson.KV{Key: "text", Val: "слишком часто\n"})
	} else {
		text := n.a.metrics.render()
		c.mu.Lock()
		c.metricsBusy = false
		c.mu.Unlock()
		res = append(res, jsjson.KV{Key: "status", Val: 200}, jsjson.KV{Key: "text", Val: text})
	}
	c.ws.Write(context.Background(), websocket.MessageText, jsjson.MustMarshal(res))
}

// Start поднимает хаб (если настроен) и периодические ping/health.
func (n *Nodes) Start() {
	if n.local {
		go n.refreshHealth(0)
		go n.fireOnline(0)
	}
	if n.port == 0 || n.tls == nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/node/v1", n.serveWS)
	srv := &http.Server{Addr: fmt.Sprintf(":%d", n.port), Handler: mux, TLSConfig: n.tls, ReadHeaderTimeout: 15 * time.Second,
		ErrorLog: quietLog()}
	go func() {
		ln, err := listen(n.port)
		if err != nil {
			warnf("nodes: хаб не запущен: %v", err)
			return
		}
		logf("nodes: hub on :%d (wss)", n.port)
		srv.ServeTLS(ln, "", "")
	}()

	go func() {
		for range time.Tick(pingEvery) {
			n.mu.Lock()
			conns := map[int64]*nodeConn{}
			for id, c := range n.conns {
				conns[id] = c
			}
			n.mu.Unlock()
			for id, c := range conns {
				c.mu.Lock()
				alive := c.alive
				c.alive = false
				c.mu.Unlock()
				if !alive {
					c.ws.CloseNow()
					n.dropConn(id, c)
					continue
				}
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), pingEvery)
					defer cancel()
					if c.ws.Ping(ctx) == nil {
						c.mu.Lock()
						c.alive = true
						c.mu.Unlock()
					}
				}()
			}
		}
	}()
	go func() {
		for range time.Tick(healthEvery) {
			n.mu.Lock()
			var ids []int64
			for id := range n.conns {
				ids = append(ids, id)
			}
			n.mu.Unlock()
			for _, id := range ids {
				go n.refreshHealth(id)
			}
		}
	}()
}

// ── Роуты панели (/ui/nodes, за JWT) ────────────────────────────────────────

func (n *Nodes) hubReady() bool { return n.port > 0 && n.tls != nil && n.pin != nil }

// home — публичный адрес хаба, который он сообщает нодам в ready (агент звонит туда первым).
// Из SUB_HOST: у панели один публичный адрес; не задан — нодам хватает адреса из join.
func (n *Nodes) home() string {
	host := strings.TrimSpace(os.Getenv("SUB_HOST"))
	if host == "" || !n.hubReady() {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(n.port))
}

func (n *Nodes) joinString(host string, id int64, secret []byte) string {
	b := jsjson.MustMarshal(jsjson.Object{{Key: "v", Val: 1}, {Key: "h", Val: host}, {Key: "p", Val: n.port},
		{Key: "pin", Val: b64url.EncodeToString(n.pin)}, {Key: "n", Val: id}, {Key: "s", Val: b64url.EncodeToString(secret)}})
	return "awgjoin://" + b64url.EncodeToString(b)
}

var rePortSuffix = regexp.MustCompile(`:\d+$`)

func (n *Nodes) hostOf(c *Ctx) string {
	if given, ok := c.Obj().Get2("host").(string); ok && reHost.MatchString(given) {
		return given
	}
	h := rePortSuffix.ReplaceAllString(c.R.Host, "")
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// List — серверы со всем, что известно без запросов (health — из кэша). Общий вид для GET / и метрик.
func (n *Nodes) List() []serverInfo {
	var out []serverInfo
	n.mu.Lock()
	hs := map[int64]*health{}
	for k, v := range n.health {
		hs[k] = v
	}
	online := map[int64]bool{}
	for k := range n.conns {
		online[k] = true
	}
	n.mu.Unlock()
	if n.local {
		h := hs[0]
		name := "VPN"
		if h != nil {
			name = h.Server
		}
		out = append(out, serverInfo{ID: 0, Name: name, Local: true, Online: h != nil, Version: orNil(n.a.version), Health: h})
	}
	for _, r := range n.all() {
		out = append(out, serverInfo{ID: int(r.ID), Name: r.Name, Online: online[r.ID], Pending: !r.Pub.Valid,
			LastSeen: nullInt(r.LastSeen), Version: nullStr(r.Version), Health: hs[r.ID],
			Outdated: r.Pub.Valid && (!r.Version.Valid || cmpVersion(r.Version.String, MinNodeVersion) < 0)})
	}
	return out
}

type serverInfo struct {
	ID       int
	Name     string
	Local    bool
	Online   bool
	Pending  bool
	LastSeen any
	Version  any
	Health   *health
	Outdated bool // старше MinNodeVersion: хаб её не подключает, пока не обновят
}

func (s serverInfo) json() jsjson.Object {
	return jsjson.Object{{Key: "id", Val: s.ID}, {Key: "name", Val: s.Name}, {Key: "local", Val: s.Local}, {Key: "online", Val: s.Online},
		{Key: "pending", Val: s.Pending}, {Key: "last_seen", Val: s.LastSeen}, {Key: "version", Val: s.Version}, {Key: "health", Val: s.Health.json()}, {Key: "outdated", Val: s.Outdated}}
}

func (n *Nodes) idOf(c *Ctx) (int64, bool) {
	f := jsToNumber(c.Params["id"])
	return int64(f), f == float64(int64(f)) && (f >= 1 || f == 0 && !n.local)
}

func (n *Nodes) router() *Router {
	rt := newRouter()
	rt.Get("/", func(c *Ctx) bool {
		if n.local {
			n.refreshHealth(0)
		}
		list := []any{}
		for _, s := range n.List() {
			list = append(list, s.json())
		}
		return c.JSON(200, jsjson.Object{{Key: "hub", Val: n.hubReady()}, {Key: "local", Val: n.local},
			{Key: "version", Val: orNil(n.a.version)}, {Key: "nodes", Val: list}})
	})
	rt.Post("/", func(c *Ctx) bool {
		if !n.hubReady() {
			return c.Error(503, "Приём нод не настроен (NODE_PORT / сертификат)")
		}
		name, ok := c.Obj().Get2("name").(string)
		if !ok || !reNodeName.MatchString(name) || !lenOK(name, 1, 40) {
			return c.Error(400, "Имя: до 40 символов, без переводов строк и <>")
		}
		host := n.hostOf(c)
		if !reHost.MatchString(host) {
			return c.Error(400, "Не удалось определить адрес core")
		}
		secret := make([]byte, 16)
		rand.Read(secret)
		h := sha256.Sum256(secret)
		res, err := n.a.db.Exec("INSERT INTO nodes (name, join_hash, join_expires, created_at) VALUES (?, ?, ?, ?)", name, h[:], now()+joinTTL, now())
		if err != nil {
			return c.Error(500, "Internal server error")
		}
		id, _ := res.LastInsertId()
		return c.JSON(201, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: name}, {Key: "join", Val: n.joinString(host, id, secret)}, {Key: "expires_in", Val: joinTTL}})
	})
	// Новая join-строка для ноды, которая ещё не подключалась (истёк срок / потеряли).
	rt.Post("/:id/join", func(c *Ctx) bool {
		if !n.hubReady() {
			return c.Error(503, "Приём нод не настроен (NODE_PORT / сертификат)")
		}
		id, ok := n.idOf(c)
		var row *nodeRow
		if ok {
			row = n.row(id)
		}
		if row == nil {
			return c.Error(404, "Не найден")
		}
		if row.Pub.Valid {
			return c.Error(409, "Нода уже подключена")
		}
		host := n.hostOf(c)
		if !reHost.MatchString(host) {
			return c.Error(400, "Не удалось определить адрес core")
		}
		secret := make([]byte, 16)
		rand.Read(secret)
		h := sha256.Sum256(secret)
		n.a.db.Exec("UPDATE nodes SET join_hash = ?, join_expires = ? WHERE id = ? AND pub IS NULL", h[:], now()+joinTTL, id)
		return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: row.Name}, {Key: "join", Val: n.joinString(host, id, secret)}, {Key: "expires_in", Val: joinTTL}})
	})
	rt.Patch("/:id", func(c *Ctx) bool {
		id, ok := n.idOf(c)
		if !ok || n.row(id) == nil {
			return c.Error(404, "Не найден")
		}
		name, ok := c.Obj().Get2("name").(string)
		if !ok || !reNodeName.MatchString(name) || !lenOK(name, 1, 40) {
			return c.Error(400, "Неверное имя")
		}
		n.a.db.Exec("UPDATE nodes SET name = ? WHERE id = ?", name, id)
		return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: name}})
	})
	rt.Delete("/:id", func(c *Ctx) bool {
		id, ok := n.idOf(c)
		if !ok || n.row(id) == nil {
			return c.Error(404, "Не найден")
		}
		// Ключи API и мастер-ключи проверяют их владельцы (BeforeDelete).
		force := c.R.URL.Query().Get("force") == "1"
		n.hmu.Lock()
		before := append([]func(int, bool) *deleteVeto{}, n.beforeDelete...)
		after := append([]func(int){}, n.afterDelete...)
		n.hmu.Unlock()
		for _, cb := range before {
			veto := func() (v *deleteVeto) {
				defer func() {
					if recover() != nil {
						v = &deleteVeto{"Не удалось подготовить сервер к удалению", true}
					}
				}()
				return cb(int(id), force)
			}()
			if veto != nil {
				return c.JSON(409, jsjson.Object{{Key: "error", Val: veto.Error}, {Key: "can_force", Val: veto.Forcible && !force}})
			}
		}
		if n.row(id) == nil {
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
		// Хвосты подписки для этого сервера больше некому доделать.
		for _, t := range []string{"peer_tombstones", "server_profiles"} {
			n.a.db.Exec("DELETE FROM "+t+" WHERE server_id = ?", id)
		}
		for _, cb := range after {
			func() {
				defer func() {
					if p := recover(); p != nil {
						warnf("nodes: уборка после удаления #%d: %v", id, p)
					}
				}()
				cb(int(id))
			}()
		}
		return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: id}})
	})
	return rt
}

// randomCN — имя для самоподписанного сертификата: похожее на обычный хост и своё у каждого
// сервера.
func randomCN() string {
	const abc = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b) + ".net"
}
