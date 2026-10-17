package ui

// Мастер-ключи: общая логика панели и внешнего API (порт части awg-ui/sub.ts). Панель
// (/ui/masterkeys, /ui/devices, за JWT) видит всё. Внешний API (/api/v1/masterkeys, ключ
// awgk_) — только ключи, чьи серверы целиком входят в набор серверов API-ключа: иначе он мог бы
// удалить или перенастроить раздачу на сервере, которого не видит. scope: nil — панель.

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail(status int, msg string) error { return &apiError{status, msg} }

// aw — apiError даёт свой статус; остальное (awg-ctrl не ответил посреди действия) — 502.
func aw(fn func(c *Ctx) (int, any, error)) handler {
	return func(c *Ctx) bool {
		var (
			st  int
			v   any
			err error
		)
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%v", p)
				}
			}()
			st, v, err = fn(c)
		}()
		var ae *apiError
		if errors.As(err, &ae) {
			return c.Error(ae.status, ae.msg)
		}
		if err != nil {
			return c.Error(502, "awg-ctrl недоступен")
		}
		return c.JSON(st, v)
	}
}

var (
	reUUID  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reLabel = regexp.MustCompile(`^[^\r\n<>]+$`) // длина — lenOK(1, 40): в JS {1,40} считает UTF-16
)

func labelOK(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && reLabel.MatchString(s) && lenOK(s, 1, 40)
}

// idOf — Number(v), если это безопасное целое, иначе -1 (не найдётся).
func idOf(v string) int64 {
	f := jsToNumber(v)
	if !isSafeInt(f) {
		return -1
	}
	return int64(f)
}

// masterIdOf — мастер-ключ в пути: числовой id или uuid.
func (s *Sub) masterIDOf(v string) int64 {
	if reUUID.MatchString(v) {
		var id int64
		if s.a.db.QueryRow("SELECT id FROM master_keys WHERE uuid = ?", strings.ToLower(v)).Scan(&id) != nil {
			return -1
		}
		return id
	}
	return idOf(v)
}

func limitOK(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok && f == float64(int64(f)) && f >= 1 && f <= 100
}

func (s *Sub) visible(masterID int64, scope []int) bool {
	if scope == nil {
		return true
	}
	for _, id := range s.serverIDsOf(s.a.db, masterID) {
		if !containsInt(scope, id) {
			return false
		}
	}
	return true
}

// masterFor — живой (не удаляемый) ключ в пределах области; чужой — как несуществующий.
func (s *Sub) masterFor(id int64, scope []int) (*masterRow, error) {
	m := s.masterByID(s.a.db, id)
	if m == nil || !s.visible(m.ID, scope) {
		return nil, fail(404, "Не найден")
	}
	return m, nil
}

// deviceFor — устройство; у внешнего API — только вместе со своим мастер-ключом.
func (s *Sub) deviceFor(id int64, master *masterRow) (*deviceRow, error) {
	d := s.deviceByID(s.a.db, id)
	if d == nil || master != nil && d.MasterID != master.ID {
		return nil, fail(404, "Не найдено")
	}
	return d, nil
}

// parseServers — набор серверов из тела: непустой, без повторов, только существующие и — для
// внешнего API — только из области его ключа.
func (s *Sub) parseServers(v any, scope []int) ([]int, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fail(400, "Нужен хотя бы один сервер")
	}
	if len(list) > maxServers {
		return nil, fail(400, fmt.Sprintf("Не больше %d серверов", maxServers))
	}
	ids, ok := s.a.serverList(v, s.a.nodes.Exists)
	if !ok {
		return nil, fail(400, "Такого сервера нет")
	}
	if scope != nil {
		for _, id := range ids {
			if !containsInt(scope, id) {
				return nil, fail(403, "Сервер вне области API-ключа")
			}
		}
	}
	return ids, nil
}

// masterOut — deleting: ключ удалён, но эти серверы ещё не подтвердили, что сняли его пиров.
func (s *Sub) masterOut(m *masterRow) jsjson.Object {
	srv := intsJSON(s.serverIDsOf(s.a.db, m.ID))
	o := jsjson.Object{{Key: "id", Val: m.ID}, {Key: "uuid", Val: m.UUID}, {Key: "label", Val: m.Label}, {Key: "device_limit", Val: m.DeviceLimit},
		{Key: "devices", Val: m.Devices}, {Key: "servers", Val: srv}, {Key: "created_at", Val: m.CreatedAt}}
	if m.RevokedAt.Valid {
		o = append(o, jsjson.KV{Key: "deleting", Val: srv})
	}
	return o
}

func (s *Sub) masterOutByID(id int64) jsjson.Object { return s.masterOut(s.masterRowByID(s.a.db, id)) }

func (s *Sub) listMasters(scope []int) []any {
	rows, err := s.a.db.Query("SELECT " + masterCols + ", (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m ORDER BY id")
	if err != nil {
		return []any{}
	}
	var ms []*masterRow
	for rows.Next() {
		if m := scanMaster(rows, true); m != nil {
			ms = append(ms, m)
		}
	}
	rows.Close()
	out := []any{}
	for _, m := range ms {
		if s.visible(m.ID, scope) {
			out = append(out, s.masterOut(m))
		}
	}
	return out
}

// createMaster — серверы по умолчанию: у панели — сервер по умолчанию, у API-ключа — вся его область.
func (s *Sub) createMaster(body any, scope []int) (jsjson.Object, error) {
	b, _ := body.(jsjson.Object)
	label, ok := labelOK(b.Get2("label"))
	if !ok {
		return nil, fail(400, "Метка: до 40 символов, без переводов строк и <>")
	}
	limitV, hasLimit := b.Get("device_limit")
	limit := int64(defaultLimit)
	if hasLimit {
		var ok bool
		if limit, ok = limitOK(limitV); !ok {
			return nil, fail(400, "Лимит устройств: от 1 до 100")
		}
	}
	wanted, hasWanted := b.Get("servers")
	src := wanted
	if src == nil {
		if scope != nil {
			list := []any{} // как пришло бы из JSON: числа — float64
			for _, id := range scope {
				list = append(list, float64(id))
			}
			src = list
		} else {
			src = []any{float64(s.a.nodes.DefaultID())}
		}
	}
	ids, err := s.parseServers(src, scope)
	if err != nil {
		if !hasWanted && scope == nil {
			return nil, fail(400, "Сначала добавьте сервер")
		}
		return nil, err
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	var id int64
	err = s.a.tx(func(tx *sql.Tx) error {
		r, err := tx.Exec("INSERT INTO master_keys (uuid, label, secret, device_limit, server_id, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			uuid.NewString(), label, secret, limit, ids[0], now())
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		for _, sid := range ids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO master_servers (master_id, server_id) VALUES (?, ?)", id, sid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.masterOutByID(id), nil
}

func (s *Sub) updateMaster(m *masterRow, body any, scope []int) (jsjson.Object, error) {
	b, _ := body.(jsjson.Object)
	nlV := b.Get2("label")
	if nlV == nil {
		nlV = m.Label
	}
	ndV := b.Get2("device_limit")
	if ndV == nil {
		ndV = float64(m.DeviceLimit)
	}
	nl, ok := labelOK(nlV)
	if !ok {
		return nil, fail(400, "Неверная метка")
	}
	nd, ok := limitOK(ndV)
	if !ok {
		return nil, fail(400, "Лимит устройств: от 1 до 100")
	}

	ids := s.serverIDsOf(s.a.db, m.ID)
	var affected []int
	if wanted, has := b.Get("servers"); has {
		next, err := s.parseServers(wanted, scope)
		if err != nil {
			return nil, err
		}
		var added, removed []int
		for _, x := range next {
			if !containsInt(ids, x) {
				added = append(added, x)
			}
		}
		for _, x := range ids {
			if !containsInt(next, x) {
				removed = append(removed, x)
			}
		}
		devices := s.devicesOf(s.a.db, m.ID)
		if err := s.a.tx(func(tx *sql.Tx) error {
			for _, sid := range removed {
				tx.Exec("DELETE FROM master_servers WHERE master_id = ? AND server_id = ?", m.ID, sid)
				for _, d := range devices {
					for _, l := range s.linksOf(tx, d.ID) {
						if l.ServerID == sid && l.PeerPub.Valid {
							tx.Exec("INSERT OR IGNORE INTO peer_tombstones (server_id, pub_key) VALUES (?, ?)", sid, l.PeerPub.String)
						}
					}
					tx.Exec("DELETE FROM device_peers WHERE device_id = ? AND server_id = ?", d.ID, sid)
				}
			}
			for _, sid := range added {
				tx.Exec("INSERT OR IGNORE INTO master_servers (master_id, server_id) VALUES (?, ?)", m.ID, sid)
				for _, d := range devices {
					tx.Exec("INSERT OR IGNORE INTO device_peers (device_id, server_id) VALUES (?, ?)", d.ID, sid)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		affected = append(append(affected, added...), removed...)
		ids = next
	}
	s.a.db.Exec("UPDATE master_keys SET label = ?, device_limit = ? WHERE id = ?", nl, nd, m.ID)
	// Лимит стал меньше, чем устройств: лишние отвязываются, начиная с подключившихся последними.
	unbound := []any{}
	devices := s.devicesOf(s.a.db, m.ID)
	if int64(len(devices)) > nd {
		newest := append([]*deviceRow{}, devices...)
		sort.SliceStable(newest, func(i, j int) bool {
			if newest[i].CreatedAt != newest[j].CreatedAt {
				return newest[i].CreatedAt > newest[j].CreatedAt
			}
			return newest[i].ID > newest[j].ID
		})
		for _, d := range newest[:int64(len(devices))-nd] {
			affected = append(affected, s.forgetDeviceTx(d)...)
			unbound = append(unbound, d.ID)
		}
	}
	s.reconcileAll(affected)
	return jsjson.Object{{Key: "id", Val: m.ID}, {Key: "label", Val: nl}, {Key: "device_limit", Val: nd},
		{Key: "servers", Val: intsJSON(ids)}, {Key: "unbound", Val: unbound}}, nil
}

// rotateMaster — новая ссылка; устройства продолжают работать: у них auth_pub, а не secret.
func (s *Sub) rotateMaster(m *masterRow) {
	secret := make([]byte, 16)
	rand.Read(secret)
	s.a.db.Exec("UPDATE master_keys SET secret = ? WHERE id = ?", secret, m.ID)
}

func (s *Sub) rekeyMaster(m *masterRow) {
	s.a.db.Exec("UPDATE devices SET rekey_requested = 1 WHERE id = ? OR master_id = ?", -1, m.ID)
}

// deleteMaster — ключ сразу перестаёт работать, но строка остаётся отозванной, пока каждый его
// сервер не подтвердит, что снял пиров.
func (s *Sub) deleteMaster(m *masterRow) jsjson.Object {
	affected := s.serverIDsOf(s.a.db, m.ID)
	s.a.tx(func(tx *sql.Tx) error {
		for _, d := range s.devicesOf(tx, m.ID) {
			affected = append(affected, s.forgetDevice(tx, d)...)
		}
		_, err := tx.Exec("UPDATE master_keys SET revoked_at = ? WHERE id = ?", now(), m.ID)
		return err
	})
	s.reconcileAll(affected)
	return jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: m.ID}, {Key: "pending", Val: intsJSON(s.serverIDsOf(s.a.db, m.ID))}}
}

// linkProblem — выдать ссылку можно, только если подписка настроена и известен адрес панели.
func (s *Sub) linkProblem(c *Ctx) string {
	if s.key() == nil || s.signPub == nil || s.port == 0 {
		return "Подписка не настроена на сервере"
	}
	if s.subHost(c) == "" {
		return "Не удалось определить адрес панели — задайте SUB_HOST"
	}
	return ""
}

func (s *Sub) linkOf(m *masterRow, c *Ctx) (jsjson.Object, error) {
	if p := s.linkProblem(c); p != "" {
		return nil, fail(503, p)
	}
	link, err := EncodeSenLink(SenLink{TLS: s.tlsPin != nil, Addrs: []SenAddr{{s.subHost(c), s.port}},
		Secret: m.Secret, SignPub: s.signPub, TLSPin: s.tlsPin, Name: m.Label})
	if err != nil {
		return nil, err // как в Node: не ApiError — 502
	}
	return jsjson.Object{{Key: "link", Val: link}, {Key: "tls", Val: s.tlsPin != nil}}, nil
}

type devStat struct {
	online     bool
	lastHS     float64
	rx, tx     float64
	background bool
	torrent    bool
}

// masterDevices — статистика устройства по всем серверам ключа: онлайн, если есть свежий
// handshake хоть на одном; трафик суммируется. servers_ok — где уже стоит его текущий ключ.
func (s *Sub) masterDevices(m *masterRow) ([]any, error) {
	stats := map[string]*devStat{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, id := range s.serverIDsOf(s.a.db, m.ID) {
		if !s.a.nodes.IsOnline(id) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.a.nodes.CtrlFor(id)("GET", "/api/peers", nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if !ok2xx(r.Status) {
				return
			}
			for _, p := range arr(field(r.Data, "peers")) {
				pub, _ := field(p, "pub_key").(string)
				st := stats[pub]
				if st == nil {
					st = &devStat{}
					stats[pub] = st
				}
				st.online = st.online || truthy(field(p, "online"))
				if h := numOr(field(p, "lastHandshake"), 0); h > st.lastHS {
					st.lastHS = h
				}
				st.rx += numOr(field(p, "rx"), 0)
				st.tx += numOr(field(p, "tx"), 0)
				// Устройство стоит на каждом сервере со своим IP; фоновым его делает любой из них.
				st.background = st.background || field(p, "priority") == "background"
				st.torrent = st.torrent || truthy(field(p, "torrent"))
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	out := []any{}
	for _, d := range s.devicesOf(s.a.db, m.ID) {
		links := s.linksOf(s.a.db, d.ID)
		st := stats[d.PubKey]
		if st == nil {
			st = &devStat{}
		}
		okN := 0
		for _, l := range links {
			if l.PeerPub.Valid && l.PeerPub.String == d.PubKey && l.IP.Valid && l.IP.String != "" {
				okN++
			}
		}
		prio := "normal"
		if st.background {
			prio = "background"
		}
		out = append(out, jsjson.Object{
			{Key: "id", Val: d.ID}, {Key: "device_id", Val: d.DeviceID}, {Key: "device_name", Val: d.DeviceName}, {Key: "platform", Val: d.Platform},
			{Key: "version", Val: d.Version}, {Key: "created_at", Val: d.CreatedAt}, {Key: "last_seen", Val: nullInt(d.LastSeen)},
			{Key: "rekey_requested", Val: d.RekeyRequested == 1}, {Key: "online", Val: st.online}, {Key: "lastHandshake", Val: st.lastHS},
			{Key: "rx", Val: st.rx}, {Key: "tx", Val: st.tx}, {Key: "priority", Val: prio},
			{Key: "torrent", Val: st.torrent},
			{Key: "servers_total", Val: len(links)}, {Key: "servers_ok", Val: okN},
		})
	}
	return out, nil
}

func (s *Sub) deleteDevice(d *deviceRow) jsjson.Object {
	s.reconcileAll(s.forgetDeviceTx(d))
	return jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: d.ID}}
}

func (s *Sub) rekeyDevice(d *deviceRow) {
	s.a.db.Exec("UPDATE devices SET rekey_requested = 1 WHERE id = ? OR master_id = ?", d.ID, -1)
}

// rotatePsk — новый PSK на каждом сервере; недоступные получат его, когда появятся.
func (s *Sub) rotatePsk(d *deviceRow) jsjson.Object {
	s.a.db.Exec("UPDATE device_peers SET psk_pending = 1 WHERE device_id = ? AND peer_pub IS NOT NULL", d.ID)
	var ids []int
	for _, l := range s.linksOf(s.a.db, d.ID) {
		ids = append(ids, l.ServerID)
	}
	s.reconcileAll(ids)
	pending := 0
	for _, l := range s.linksOf(s.a.db, d.ID) {
		if l.PSKPending == 1 {
			pending++
		}
	}
	return jsjson.Object{{Key: "id", Val: d.ID}, {Key: "pending", Val: pending}}
}

func idObj(id int64) jsjson.Object { return jsjson.Object{{Key: "id", Val: id}} }

func merge(a, b jsjson.Object) jsjson.Object {
	out := append(jsjson.Object{}, a...)
	for _, kv := range b {
		replaced := false
		for i := range out {
			if out[i].Key == kv.Key {
				out[i].Val, replaced = kv.Val, true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// ── Роуты панели (/ui/masterkeys, /ui/devices) ──────────────────────────────

func (s *Sub) masterKeysRouter() *Router {
	rt := newRouter()
	auditOff := fail(409, "Аудит выключен на этой панели")
	master := func(c *Ctx) (*masterRow, error) { return s.masterFor(s.masterIDOf(c.Params["id"]), nil) }

	rt.Get("/", func(c *Ctx) bool {
		return c.JSON(200, jsjson.Object{{Key: "enabled", Val: s.key() != nil}, {Key: "tls", Val: s.tlsPin != nil}, {Key: "keys", Val: s.listMasters(nil)}})
	})
	// Аудит серверов (только core): отчёт, внеочередной проход, ручная очистка, настройки.
	rt.Get("/audit", func(c *Ctx) bool { return c.JSON(200, s.auditState()) })
	rt.Post("/audit", aw(func(c *Ctx) (int, any, error) {
		if !s.auditOn {
			return 0, nil, auditOff
		}
		s.auditAll(false)
		return 200, s.auditState(), nil
	}))
	rt.Post("/audit/clean", aw(func(c *Ctx) (int, any, error) {
		if !s.auditOn {
			return 0, nil, auditOff
		}
		s.cleanNow()
		return 200, s.auditState(), nil
	}))
	rt.Put("/audit/settings", aw(func(c *Ctx) (int, any, error) {
		if !s.auditOn {
			return 0, nil, auditOff
		}
		b := c.Obj()
		s.auditMu.Lock()
		next := s.auditSettings
		s.auditMu.Unlock()
		if v, has := b.Get("clean"); has {
			bv, ok := v.(bool)
			if !ok {
				return 0, nil, fail(400, "clean: true или false")
			}
			next.Clean = bv
		}
		if v, has := b.Get("every_h"); has {
			if !isInt(v) || v.(float64) < 1 || v.(float64) > 168 {
				return 0, nil, fail(400, "Период: от 1 до 168 часов")
			}
			next.EveryH = int(v.(float64))
		}
		if v, has := b.Get("max"); has {
			if !isInt(v) || v.(float64) < 1 || v.(float64) > 1000 {
				return 0, nil, fail(400, "Предел: от 1 до 1000")
			}
			next.Max = int(v.(float64))
		}
		s.auditMu.Lock()
		s.auditSettings = next
		s.metaSet("audit_settings", next.json())
		s.auditMu.Unlock()
		return 200, s.auditState(), nil
	}))
	rt.Post("/", aw(func(c *Ctx) (int, any, error) {
		out, err := s.createMaster(c.Body, nil)
		return 201, out, err
	}))
	rt.Patch("/:id", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.updateMaster(m, c.Body, nil)
		return 200, out, err
	}))
	rt.Post("/:id/rotate", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		s.rotateMaster(m)
		return 200, idObj(m.ID), nil
	}))
	rt.Post("/:id/rekey", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		s.rekeyMaster(m)
		return 200, idObj(m.ID), nil
	}))
	rt.Delete("/:id", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.deleteMaster(m), nil
	}))
	rt.Get("/:id/link", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.linkOf(m, c)
		return 200, out, err
	}))
	rt.Get("/:id/devices", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		list, err := s.masterDevices(m)
		return 200, jsjson.Object{{Key: "devices", Val: list}}, err
	}))
	return rt
}

func (s *Sub) devicesRouter() *Router {
	rt := newRouter()
	dev := func(c *Ctx) (*deviceRow, error) { return s.deviceFor(idOf(c.Params["id"]), nil) }
	rt.Delete("/:id", aw(func(c *Ctx) (int, any, error) {
		d, err := dev(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.deleteDevice(d), nil
	}))
	rt.Post("/:id/rekey", aw(func(c *Ctx) (int, any, error) {
		d, err := dev(c)
		if err != nil {
			return 0, nil, err
		}
		s.rekeyDevice(d)
		return 200, idObj(d.ID), nil
	}))
	rt.Post("/:id/psk", aw(func(c *Ctx) (int, any, error) {
		d, err := dev(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.rotatePsk(d), nil
	}))
	return rt
}

// ── Внешний API: /api/v1/masterkeys (ключ awgk_) ────────────────────────────
// scopeOf — набор серверов API-ключа запроса. Устройства — вложенными путями, чтобы
// область проверялась через их мастер-ключ.
func (s *Sub) apiRouter(scopeOf func(c *Ctx) []int) *Router {
	rt := newRouter()
	scope := func(c *Ctx) []int {
		if sc := scopeOf(c); sc != nil {
			return sc
		}
		return []int{}
	}
	master := func(c *Ctx) (*masterRow, error) { return s.masterFor(s.masterIDOf(c.Params["id"]), scope(c)) }
	device := func(c *Ctx) (*deviceRow, error) {
		m, err := master(c)
		if err != nil {
			return nil, err
		}
		return s.deviceFor(idOf(c.Params["device"]), m)
	}
	withLink := func(c *Ctx, m *masterRow, base jsjson.Object) (jsjson.Object, error) {
		l, err := s.linkOf(m, c)
		if err != nil {
			return nil, err
		}
		return merge(base, l), nil
	}

	rt.Get("/", aw(func(c *Ctx) (int, any, error) {
		return 200, jsjson.Object{{Key: "keys", Val: s.listMasters(scope(c))}}, nil
	}))
	rt.Post("/", aw(func(c *Ctx) (int, any, error) {
		if p := s.linkProblem(c); p != "" { // без ссылки ключ внешней программе бесполезен
			return 0, nil, fail(503, p)
		}
		out, err := s.createMaster(c.Body, scope(c))
		if err != nil {
			return 0, nil, err
		}
		m, err := s.masterFor(int64(out.Get2("id").(int64)), nil)
		if err != nil {
			return 0, nil, err
		}
		res, err := withLink(c, m, out)
		return 201, res, err
	}))
	rt.Get("/:id", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		// devices — счётчик, как в списке; сами устройства — в device_list.
		out := s.masterOutByID(m.ID)
		if s.linkProblem(c) != "" {
			out = merge(out, jsjson.Object{{Key: "link", Val: nil}, {Key: "tls", Val: s.tlsPin != nil}})
		} else if out, err = withLink(c, m, out); err != nil {
			return 0, nil, err
		}
		list, err := s.masterDevices(m)
		if err != nil {
			return 0, nil, err
		}
		return 200, merge(out, jsjson.Object{{Key: "device_list", Val: list}}), nil
	}))
	rt.Patch("/:id", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.updateMaster(m, c.Body, scope(c))
		return 200, out, err
	}))
	rt.Post("/:id/rotate", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		s.rotateMaster(m)
		m2, err := s.masterFor(m.ID, nil)
		if err != nil {
			return 0, nil, err
		}
		out, err := withLink(c, m2, idObj(m.ID))
		return 200, out, err
	}))
	rt.Post("/:id/rekey", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		s.rekeyMaster(m)
		return 200, idObj(m.ID), nil
	}))
	rt.Delete("/:id", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.deleteMaster(m), nil
	}))
	rt.Get("/:id/devices", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		list, err := s.masterDevices(m)
		return 200, jsjson.Object{{Key: "devices", Val: list}}, err
	}))
	rt.Delete("/:id/devices/:device", aw(func(c *Ctx) (int, any, error) {
		d, err := device(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.deleteDevice(d), nil
	}))
	rt.Post("/:id/devices/:device/rekey", aw(func(c *Ctx) (int, any, error) {
		d, err := device(c)
		if err != nil {
			return 0, nil, err
		}
		s.rekeyDevice(d)
		return 200, idObj(d.ID), nil
	}))
	rt.Post("/:id/devices/:device/psk", aw(func(c *Ctx) (int, any, error) {
		d, err := device(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.rotatePsk(d), nil
	}))
	return rt
}

// ── Удаление сервера из реестра ─────────────────────────────────────────────

// beforeServerDelete — пока сервером пользуется живой мастер-ключ — нельзя. Иначе сперва
// сводим его (пиры панели снимутся) — после удаления до него уже не дотянуться. Не вышло
// (нода не в сети) — удалять только с force.
func (s *Sub) beforeServerDelete(id int, force bool) *deleteVeto {
	live := strs(s.a.db, "SELECT m.label FROM master_servers s JOIN master_keys m ON m.id = s.master_id WHERE s.server_id = ? AND m.revoked_at IS NULL", id)
	if len(live) > 0 {
		return &deleteVeto{Error: "Сервер в мастер-ключах: " + strings.Join(live, ", ") + " — сначала уберите его оттуда"}
	}
	if s.a.nodes.IsOnline(id) {
		s.reconcile(id)
	}
	waiting := strs(s.a.db, "SELECT m.label FROM master_servers s JOIN master_keys m ON m.id = s.master_id WHERE s.server_id = ? AND m.revoked_at IS NOT NULL", id)
	tombs := s.tombCount(id)
	if len(waiting) == 0 && tombs == 0 || force {
		return nil
	}
	msg := "Сервер не подтвердил, что снял пиров"
	if len(waiting) > 0 {
		msg += " удалённых мастер-ключей (" + strings.Join(waiting, ", ") + ")"
	}
	msg += ". Дождитесь, пока он выйдет на связь, или удалите принудительно — тогда пиры могут остаться на нём (снять: awg-ctrl peers drop)"
	return &deleteVeto{Error: msg, Forcible: true}
}

func (s *Sub) afterServerDelete(id int) {
	s.a.db.Exec("DELETE FROM master_servers WHERE server_id = ?", id)
	s.a.db.Exec("DELETE FROM master_keys WHERE revoked_at IS NOT NULL AND id NOT IN (SELECT master_id FROM master_servers)")
}
