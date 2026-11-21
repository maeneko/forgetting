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
	"strconv"
	"strings"
	"sync"
	"time"

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

// unixOK — срок ключа: целое unix-время ≥ 0. null/отсутствие — без срока (см. bodyTime).
func unixOK(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok && isSafeInt(f) && f >= 0
}

// bytesOK — лимит трафика: целое ≥ 0 байт. null/отсутствие — без лимита (см. bodyBytes).
func bytesOK(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok && isSafeInt(f) && f >= 0
}

// bodyTime/bodyBytes — необязательное поле из тела: nil — ограничение снимается (NULL),
// числа проверяются валидатором рядом с limitOK.
func bodyTime(b jsjson.Object, key string) (any, error) {
	v, has := b.Get(key)
	if !has || v == nil {
		return nil, nil
	}
	n, ok := unixOK(v)
	if !ok {
		return nil, fail(400, key+": unix-время, целое ≥ 0 или null")
	}
	return n, nil
}

func bodyBytes(b jsjson.Object, key string) (any, error) {
	v, has := b.Get(key)
	if !has || v == nil {
		return nil, nil
	}
	n, ok := bytesOK(v)
	if !ok {
		return nil, fail(400, key+": байты, целое ≥ 0 или null")
	}
	return n, nil
}

func (s *Sub) visible(masterID int64, scope []int) bool {
	if scope == nil {
		return true
	}
	for _, id := range s.serverIDsOf(s.a.db, masterID) {
		// Мёртвый сервер (удалён из реестра) не может входить в область API-ключа —
		// не считаем его нарушением, иначе ключ-сирота 403-ит навсегда. Реестр не
		// загружен (тесты) — фильтр не работает, как раньше.
		if s.a.nodes != nil && !s.a.nodes.Exists(id) {
			continue
		}
		if !containsInt(scope, id) {
			return false
		}
	}
	return true
}

// masterFor — живой (не удаляемый) ключ в пределах области. Ключ, которого нет или который
// удалён, — 404; ключ, у которого есть сервер вне области API-ключа, — 403: клиент не должен
// принимать его за удалённый и выдавать вместо него второй (так у пользователя появлялся дубль).
func (s *Sub) masterFor(id int64, scope []int) (*masterRow, error) {
	m := s.masterByID(s.a.db, id)
	if m == nil {
		return nil, fail(404, "Не найден")
	}
	if !s.visible(m.ID, scope) {
		return nil, fail(403, "Ключ вне области API-ключа")
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
// Ограничения отдаются именами панели (camelCase): срок, лимит, учтённый трафик и блокировка.
// partial/unreachable — по последнему замеру (enforceMasters): usedBytes неполон, если какой-то
// сервер не ответил; панель показывает по unreachable «нет связи». Замеров ещё не было —
// partial=false, unreachable=[]: счётчик 0 тогда не «неполный», а просто не измеренный.
func (s *Sub) masterOut(m *masterRow) jsjson.Object {
	srv := intsJSON(s.serverIDsOf(s.a.db, m.ID))
	snap := s.usageSnapshotOf(m.ID)
	b := m.blockNow(now())
	o := jsjson.Object{{Key: "id", Val: m.ID}, {Key: "uuid", Val: m.UUID}, {Key: "label", Val: m.Label}, {Key: "device_limit", Val: m.DeviceLimit},
		{Key: "devices", Val: m.Devices}, {Key: "servers", Val: srv}, {Key: "created_at", Val: m.CreatedAt},
		{Key: "expiresAt", Val: nullInt(m.ExpiresAt)}, {Key: "trafficLimitBytes", Val: nullInt(m.TrafficLimit)},
		{Key: "usedBytes", Val: m.usedTotal()}, {Key: "remainingBytes", Val: m.remaining()},
		{Key: "partial", Val: snap.known && !snap.complete}, {Key: "unreachable", Val: intsJSON(snap.unreachable)},
		{Key: "blocked", Val: b != ""}, {Key: "blockReason", Val: orNil(b)}, {Key: "expired", Val: m.expired(now())}}
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
	expires, err := bodyTime(b, "expires_at")
	if err != nil {
		return nil, err
	}
	traffic, err := bodyBytes(b, "traffic_limit_bytes")
	if err != nil {
		return nil, err
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
		} else if def := s.defaultServers(); def != nil {
			list := []any{}
			for _, id := range def {
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
		r, err := tx.Exec("INSERT INTO master_keys (uuid, label, secret, device_limit, server_id, created_at, expires_at, traffic_limit_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			uuid.NewString(), label, secret, limit, ids[0], now(), expires, traffic)
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
	// Срок и лимит трафика: null очищает ограничение, отсутствие поля — не трогает.
	expires, err := bodyTime(b, "expires_at")
	if err != nil {
		return nil, err
	}
	traffic, err := bodyBytes(b, "traffic_limit_bytes")
	if err != nil {
		return nil, err
	}
	_, hasExpires := b.Get("expires_at")
	_, hasTraffic := b.Get("traffic_limit_bytes")
	// unblock — снять ручную блокировку может только панель (у внешнего API для этого нет
	// причины: он не должен возвращать доступ в обход ограничений владельца).
	unblock := false
	if scope == nil {
		if v, has := b.Get("unblock"); has {
			uv, isBool := v.(bool)
			if !isBool {
				return nil, fail(400, "unblock: true или false")
			}
			unblock = uv
		}
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
		if err := s.a.tx(func(tx *sql.Tx) error { return s.moveServersTx(tx, m.ID, added, removed) }); err != nil {
			return nil, err
		}
		affected = append(append(affected, added...), removed...)
	}
	s.a.db.Exec("UPDATE master_keys SET label = ?, device_limit = ? WHERE id = ?", nl, nd, m.ID)
	if hasExpires {
		s.a.db.Exec("UPDATE master_keys SET expires_at = ? WHERE id = ?", expires, m.ID)
	}
	if hasTraffic {
		s.a.db.Exec("UPDATE master_keys SET traffic_limit_bytes = ? WHERE id = ?", traffic, m.ID)
	}
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
	// Ручное снятие блокировки — здесь же; иначе срок/лимит могли поднять, и расчётная
	// блокировка должна сняться сразу, не дожидаясь тика ограничений.
	switch {
	case unblock:
		s.clearBlock(m.ID)
	case hasExpires || hasTraffic:
		s.relaxBlock(m.ID)
	}
	return merge(s.masterOutByID(m.ID), jsjson.Object{{Key: "unbound", Val: unbound},
		{Key: "servers", Val: intsJSON(s.serverIDsOf(s.a.db, m.ID))}}), nil
}

// clearBlock — снять блокировку и вернуть пиров: они были сняты с серверов, а device_peers
// помнит последний известный ключ, поэтому links обнуляются — reconcile поставит пиров заново.
func (s *Sub) clearBlock(masterID int64) {
	var blocked int
	s.a.db.QueryRow("SELECT COUNT(*) FROM master_keys WHERE id = ? AND blocked_reason IS NOT NULL", masterID).Scan(&blocked)
	s.a.db.Exec("UPDATE master_keys SET blocked_at = NULL, blocked_reason = NULL WHERE id = ?", masterID)
	if blocked == 0 {
		return // блокировки не было — пиров не трогаем
	}
	s.a.db.Exec(`UPDATE device_peers SET peer_pub = NULL, ip = NULL, psk = NULL, psk_pending = 0
		WHERE device_id IN (SELECT id FROM devices WHERE master_id = ?)`, masterID)
	s.reconcileAll(s.serverIDsOf(s.a.db, masterID))
}

// relaxBlock — расчётную блокировку снимаем, если причина исчезла (срок продлили, лимит подняли
// или сняли). manual и api не трогаем: их снимает только панель/внешняя система.
func (s *Sub) relaxBlock(masterID int64) {
	m := s.masterByID(s.a.db, masterID)
	if m == nil || !m.BlockedReason.Valid || m.BlockedReason.String == blockManual || m.BlockedReason.String == blockAPI {
		return
	}
	if !m.expired(now()) && (!m.TrafficLimit.Valid || m.usedTotal() < m.TrafficLimit.Int64) {
		s.clearBlock(masterID)
	}
}

// blockMaster — блокировка из панели (по умолчанию manual) или внешней системой (api): пиры
// снимутся ближайшим сведением, ключ и устройства остаются в реестре (GET покажет blocked: true).
func (s *Sub) blockMaster(m *masterRow, reason string) (jsjson.Object, error) {
	switch reason {
	case "":
		reason = blockManual
	case blockManual, blockExpired, blockTraffic, blockAPI:
	default:
		return nil, fail(400, "Причина: expired, traffic, manual или api")
	}
	if _, err := s.a.db.Exec("UPDATE master_keys SET blocked_at = ?, blocked_reason = ? WHERE id = ?", now(), reason, m.ID); err != nil {
		return nil, err
	}
	s.reconcileAll(s.serverIDsOf(s.a.db, m.ID))
	return s.masterOutByID(m.ID), nil
}

// apiBlockReason — причина блокировки из тела внешнего API: только manual или api, по умолчанию
// api (блокировка «по API»). expired/traffic ставит расчётный тик, а не клиент.
func apiBlockReason(body any) (string, error) {
	o, _ := body.(jsjson.Object)
	v, has := o.Get("reason")
	if !has || v == nil {
		return blockAPI, nil
	}
	r, isStr := v.(string)
	if !isStr || (r != blockManual && r != blockAPI) {
		return "", fail(400, "reason: manual или api")
	}
	return r, nil
}

// unblockMaster — ручное снятие: чистится только блокировка, срок и лимит остаются как были.
func (s *Sub) unblockMaster(m *masterRow) jsjson.Object {
	s.clearBlock(m.ID)
	return s.masterOutByID(m.ID)
}

// ── Сброс счётчика трафика ─────────────────────────────────────────────────

// usageBaseKey — ключ поправки в sub_meta: показания awg-ctrl на момент сброса расхода.
func usageBaseKey(masterID int64) string {
	return "master_usage_base:" + strconv.FormatInt(masterID, 10)
}

// usageBase — сколько байт тик вычитает из показаний awg-ctrl (0/0 — сбросов не было).
func (s *Sub) usageBase(masterID int64) (rx, tx int64) {
	o, _ := s.metaGet(usageBaseKey(masterID)).(jsjson.Object)
	return int64(numOr(o.Get2("rx"), 0)), int64(numOr(o.Get2("tx"), 0))
}

func (s *Sub) setUsageBase(masterID, rx, tx int64) {
	if rx == 0 && tx == 0 {
		s.a.db.Exec("DELETE FROM sub_meta WHERE key = ?", usageBaseKey(masterID))
		return
	}
	s.metaSet(usageBaseKey(masterID), jsjson.Object{{Key: "rx", Val: rx}, {Key: "tx", Val: tx}})
}

// resetMasterUsage — учтённый трафик ключа снова с нуля: продление подписки во внешней
// системе учёта или ручной сброс в панели. Прежние used_rx/used_tx не теряются, а становятся
// поправкой (usageBase): тик сравнивает показания awg-ctrl с накопленным итогом и без неё
// вернул бы старый трафик на первом же замере. Блокировку по трафику снимаем — счётчик снова
// под лимитом, ключ получает пиров обратно; expired и manual не трогаем (их снимает срок или
// оператор), как и в ручном unblock.
func (s *Sub) resetMasterUsage(m *masterRow) (jsjson.Object, error) {
	if _, err := s.a.db.Exec("UPDATE master_keys SET used_rx = 0, used_tx = 0 WHERE id = ?", m.ID); err != nil {
		return nil, err
	}
	s.setUsageBase(m.ID, m.UsedRx, m.UsedTx)
	if m.BlockedReason.Valid && m.BlockedReason.String == blockTraffic {
		s.clearBlock(m.ID)
	}
	return jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: m.ID}, {Key: "used_bytes", Val: int64(0)}}, nil
}

// moveServersTx — ключ получает серверы added и теряет removed: пиры его устройств на снятых
// серверах — в надгробия (их снимет reconcile), на добавленных — заявки без пира (заведёт он же).
func (s *Sub) moveServersTx(tx *sql.Tx, masterID int64, added, removed []int) error {
	devices := s.devicesOf(tx, masterID)
	for _, sid := range removed {
		if _, err := tx.Exec("DELETE FROM master_servers WHERE master_id = ? AND server_id = ?", masterID, sid); err != nil {
			return err
		}
		// Последнее показание снятого сервера больше не часть ключа: если его вернут, старый
		// расход с него не должен всплыть.
		if _, err := tx.Exec("DELETE FROM master_server_usage WHERE master_id = ? AND server_id = ?", masterID, sid); err != nil {
			return err
		}
		for _, d := range devices {
			for _, l := range s.linksOf(tx, d.ID) {
				if l.ServerID == sid && l.PeerPub.Valid {
					if _, err := tx.Exec("INSERT OR IGNORE INTO peer_tombstones (server_id, pub_key) VALUES (?, ?)", sid, l.PeerPub.String); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec("DELETE FROM device_peers WHERE device_id = ? AND server_id = ?", d.ID, sid); err != nil {
				return err
			}
		}
	}
	for _, sid := range added {
		if _, err := tx.Exec("INSERT OR IGNORE INTO master_servers (master_id, server_id) VALUES (?, ?)", masterID, sid); err != nil {
			return err
		}
		for _, d := range devices {
			if _, err := tx.Exec("INSERT OR IGNORE INTO device_peers (device_id, server_id) VALUES (?, ?)", d.ID, sid); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── Общая настройка серверов ────────────────────────────────────────────────

// defaultServers — серверы новых ключей панели (без явного набора), из настройки «Серверы для
// всех»; nil — не задано или ни одного из них уже нет.
func (s *Sub) defaultServers() []int {
	var out []int
	for _, v := range arr(s.metaGet("default_servers")) {
		if isInt(v) && s.a.nodes.Exists(int(v.(float64))) {
			out = append(out, int(v.(float64)))
		}
	}
	return out
}

// bulkServers — один набор правок для всех живых ключей: add — у всех, remove — ни у кого,
// остальные серверы каждого ключа не трогаются. dry — только посчитать. hidden — ключи API,
// которые потеряют доступ к части ключей: ключ виден API-ключу, только пока все его серверы в
// области (visible), и добавление сервера вне области его прячет.
func (s *Sub) bulkServers(body any) (jsjson.Object, error) {
	b, _ := body.(jsjson.Object)
	ids := func(k string) ([]int, error) {
		v, has := b.Get(k)
		if !has || v == nil {
			return nil, nil
		}
		if l, ok := v.([]any); ok && len(l) == 0 {
			return nil, nil
		}
		out, ok := s.a.serverList(v, s.a.nodes.Exists)
		if !ok {
			return nil, fail(400, "Такого сервера нет")
		}
		return out, nil
	}
	add, err := ids("add")
	if err != nil {
		return nil, err
	}
	remove, err := ids("remove")
	if err != nil {
		return nil, err
	}
	for _, x := range add {
		if containsInt(remove, x) {
			return nil, fail(400, "Сервер не может быть и у всех, и ни у кого")
		}
	}
	defaults, hasDefaults := b.Get("defaults")
	var defIDs []int
	if hasDefaults && defaults != nil {
		if defIDs, err = s.parseServers(defaults, nil); err != nil {
			return nil, err
		}
	}

	type change struct {
		id             int64
		added, removed []int
		before, after  []int
	}
	var changes []change
	var masters []int64
	rows, err := s.a.db.Query("SELECT id FROM master_keys WHERE revoked_at IS NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			masters = append(masters, id)
		}
	}
	rows.Close()
	empty := 0
	for _, id := range masters {
		cur := s.serverIDsOf(s.a.db, id)
		c := change{id: id, before: cur}
		for _, x := range cur {
			if containsInt(remove, x) {
				c.removed = append(c.removed, x)
			} else {
				c.after = append(c.after, x)
			}
		}
		for _, x := range add {
			if !containsInt(cur, x) {
				c.added = append(c.added, x)
				c.after = append(c.after, x)
			}
		}
		if len(c.after) == 0 {
			empty++
		}
		if len(c.added)+len(c.removed) > 0 {
			changes = append(changes, c)
		}
	}
	if empty > 0 {
		return nil, fail(409, fmt.Sprintf("У %d ключей не останется ни одного сервера — оставьте им хотя бы один", empty))
	}

	// Области ключей API: id → серверы.
	type apiKey struct {
		label string
		scope []int
	}
	keys := map[int64]*apiKey{}
	var order []int64
	if r, err := s.a.db.Query("SELECT k.id, k.label, ks.server_id FROM api_keys k LEFT JOIN api_key_servers ks ON ks.key_id = k.id ORDER BY k.id"); err == nil {
		for r.Next() {
			var id int64
			var label string
			var sid sql.NullInt64
			if r.Scan(&id, &label, &sid) != nil {
				continue
			}
			if keys[id] == nil {
				keys[id] = &apiKey{label: label}
				order = append(order, id)
			}
			if sid.Valid {
				keys[id].scope = append(keys[id].scope, int(sid.Int64))
			}
		}
		r.Close()
	}
	within := func(list, scope []int) bool {
		for _, x := range list {
			if !containsInt(scope, x) {
				return false
			}
		}
		return true
	}
	hidden := []any{}
	for _, id := range order {
		k, n := keys[id], 0
		for _, c := range changes {
			if within(c.before, k.scope) && !within(c.after, k.scope) {
				n++
			}
		}
		if n > 0 {
			hidden = append(hidden, jsjson.Object{{Key: "id", Val: id}, {Key: "label", Val: k.label}, {Key: "keys", Val: n}})
		}
	}
	out := jsjson.Object{{Key: "changed", Val: len(changes)}, {Key: "total", Val: len(masters)}, {Key: "hidden", Val: hidden}}
	if b.Get2("dry_run") == true {
		return out, nil
	}

	var affected []int
	if err := s.a.tx(func(tx *sql.Tx) error {
		for _, c := range changes {
			if err := s.moveServersTx(tx, c.id, c.added, c.removed); err != nil {
				return err
			}
			affected = append(append(affected, c.added...), c.removed...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if hasDefaults {
		if defIDs == nil {
			s.a.db.Exec("DELETE FROM sub_meta WHERE key = 'default_servers'")
		} else {
			s.metaSet("default_servers", intsJSON(defIDs))
		}
	}
	logf("masterkeys: серверы для всех — добавлены %v, сняты %v, ключей изменено: %d", add, remove, len(changes))
	s.reconcileAll(affected)
	return out, nil
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

// peersTTL — сколько держится ответ GET /api/peers сервера. Вкладка за один проход просит
// устройства каждого ключа, и без этого каждый запрос заново тянул бы пиров со всех серверов.
const peersTTL = 3 * time.Second

type peersCall struct {
	done  chan struct{} // закрывается, когда ответ получен
	at    time.Time     // нулевое — ответ не годится для повторного использования (ошибка)
	peers []any
	err   error
}

// peersOf — пиры сервера с их статистикой. Параллельные запросы к одному серверу делят один
// вызов; свежий ответ переиспользуется peersTTL, ошибка — нет.
func (s *Sub) peersOf(id int) ([]any, error) {
	s.peersMu.Lock()
	if c := s.peerCalls[id]; c != nil {
		select {
		case <-c.done:
			if !c.at.IsZero() && time.Since(c.at) < peersTTL {
				s.peersMu.Unlock()
				return c.peers, c.err
			}
		default: // уже идёт — дождаться его же
			s.peersMu.Unlock()
			<-c.done
			return c.peers, c.err
		}
	}
	c := &peersCall{done: make(chan struct{})}
	s.peerCalls[id] = c
	s.peersMu.Unlock()

	r, err := s.ctrl(id)("GET", "/api/peers", nil)
	switch {
	case err != nil:
		c.err = err
	case !ok2xx(r.Status):
		c.err = fmt.Errorf("awg-ctrl ответил %d", r.Status)
	default:
		c.peers = arr(field(r.Data, "peers"))
		c.at = time.Now()
	}
	close(c.done)
	return c.peers, c.err
}

// masterDevices — статистика устройства по всем серверам ключа: онлайн, если есть свежий
// handshake хоть на одном; трафик суммируется. servers_ok — где уже стоит его текущий ключ.
// Сервер, который не в сети или не ответил, не рушит список: его статистики просто нет, а id
// возвращается вторым значением — панель покажет «нет связи».
func (s *Sub) masterDevices(m *masterRow) ([]any, []int) {
	stats := map[string]*devStat{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	unreachable := []int{}
	for _, id := range s.serverIDsOf(s.a.db, m.ID) {
		if !s.a.nodes.IsOnline(id) {
			mu.Lock()
			unreachable = append(unreachable, id)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			peers, err := s.peersOf(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unreachable = append(unreachable, id)
				return
			}
			for _, p := range peers {
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
	sort.Ints(unreachable)
	out := []any{}
	for _, d := range s.devicesOf(s.a.db, m.ID) {
		links := s.linksOf(s.a.db, d.ID)
		st := stats[d.PubKey]
		if st == nil {
			st = &devStat{}
		}
		okN := 0
		ips := []any{} // IP устройства на каждом сервере ключа, где он уже выдан
		for _, l := range links {
			if l.PeerPub.Valid && l.PeerPub.String == d.PubKey && l.IP.Valid && l.IP.String != "" {
				okN++
			}
			if l.IP.Valid && l.IP.String != "" {
				ips = append(ips, jsjson.Object{{Key: "server_id", Val: l.ServerID}, {Key: "ip", Val: l.IP.String}})
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
			{Key: "servers_total", Val: len(links)}, {Key: "servers_ok", Val: okN}, {Key: "ips", Val: ips},
		})
	}
	return out, unreachable
}

// usageSnapshot — итог последнего замера трафика ключа (masterUsage). known — замер вообще был;
// complete — ответили все серверы; unreachable — кто не ответил (id). Хранится в памяти: список
// ключей показывает неполноту, не опрашивая серверы на каждый запрос.
type usageSnapshot struct {
	known       bool
	complete    bool
	unreachable []int
}

func (s *Sub) setUsageSnapshot(masterID int64, unreachable []int, complete bool) {
	s.usageMu.Lock()
	if s.usageSnap == nil {
		s.usageSnap = map[int64]usageSnapshot{}
	}
	s.usageSnap[masterID] = usageSnapshot{known: true, complete: complete, unreachable: append([]int{}, unreachable...)}
	s.usageMu.Unlock()
}

func (s *Sub) usageSnapshotOf(masterID int64) usageSnapshot {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usageSnap[masterID]
}

// serverUsage — последние известные показания awg-ctrl по ключу на каждом его сервере (сырые
// счётчики пиров, без поправки usageBase). Нет записи — сервер ещё ни разу не ответил.
func (s *Sub) serverUsage(masterID int64) map[int][2]int64 {
	out := map[int][2]int64{}
	rows, err := s.a.db.Query("SELECT server_id, rx, tx FROM master_server_usage WHERE master_id = ?", masterID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var rx, tx int64
		if rows.Scan(&id, &rx, &tx) == nil {
			out[id] = [2]int64{rx, tx}
		}
	}
	return out
}

func (s *Sub) setServerUsage(masterID int64, serverID int, rx, tx int64) {
	s.a.db.Exec(`INSERT INTO master_server_usage (master_id, server_id, rx, tx) VALUES (?, ?, ?, ?)
        ON CONFLICT(master_id, server_id) DO UPDATE SET rx = excluded.rx, tx = excluded.tx`, masterID, serverID, rx, tx)
}

// masterUsage — известный трафик ключа по всем его серверам. rx/tx берутся у пира, который
// сейчас стоит на сервере (device_peers.peer_pub), и складываются по устройству этого ключа, а
// не по ключу пира: peer_pub меняется при перевыпуске и смене PSK, привязка к devices.id это
// переживает. Сервер не в сети или не ответил — в сумму идёт его последнее известное значение
// (master_server_usage): обрыв не обнуляет уже израсходованное, и трафик доступных серверов
// продолжает учитываться. complete=false — ответили не все; квоту это не откладывает
// (см. enforceMasters), но used_bytes тогда неполный, и панель это показывает.
func (s *Sub) masterUsage(m *masterRow) (rx, tx int64, complete bool) {
	servers := s.serverIDsOf(s.a.db, m.ID)
	if len(servers) == 0 {
		s.setUsageSnapshot(m.ID, nil, false)
		return 0, 0, false
	}
	links := map[int][]string{} // сервер → ключи, которые там стоят (по device_peers)
	rows, err := s.a.db.Query(`SELECT l.server_id, l.peer_pub FROM device_peers l JOIN devices d ON d.id = l.device_id
        WHERE d.master_id = ? AND l.peer_pub IS NOT NULL`, m.ID)
	if err != nil {
		s.setUsageSnapshot(m.ID, nil, false)
		return 0, 0, false
	}
	for rows.Next() {
		var sid int
		var pub string
		if rows.Scan(&sid, &pub) == nil {
			links[sid] = append(links[sid], pub)
		}
	}
	rows.Close()

	last := s.serverUsage(m.ID)
	unreachable := []int{}
	for _, id := range servers {
		if !s.a.nodes.IsOnline(id) {
			unreachable = append(unreachable, id)
			rx += last[id][0]
			tx += last[id][1]
			continue
		}
		peers, err := s.peersOf(id)
		if err != nil {
			unreachable = append(unreachable, id)
			rx += last[id][0]
			tx += last[id][1]
			continue
		}
		stat := map[string][2]int64{}
		for _, p := range peers {
			pub, ok := field(p, "pub_key").(string)
			if !ok {
				continue
			}
			stat[pub] = [2]int64{int64(numOr(field(p, "rx"), 0)), int64(numOr(field(p, "tx"), 0))}
		}
		var srx, stx int64
		for _, pub := range links[id] {
			if st, ok := stat[pub]; ok {
				srx += st[0]
				stx += st[1]
			}
		}
		rx += srx
		tx += stx
		s.setServerUsage(m.ID, id, srx, stx)
	}
	sort.Ints(unreachable)
	complete = len(unreachable) == 0
	s.setUsageSnapshot(m.ID, unreachable, complete)
	return rx, tx, complete
}

// enforceMasters — тик ограничений мастер-ключей: просрочка и квота трафика. Просрочка — по
// часам и применяется всегда. Квота — по известному: сумма доступных серверов плюс последнее
// известное недоступных. Дошла до лимита — блокируем, даже если часть серверов не ответила;
// ниже — не блокируем и не выдаём неполный расход за точный (флаг partial в masterOut).
// Учтённый трафик только растёт: счётчики awg-ctrl обнуляются при пересоздании пира, а
// заблокированный ключ пиров не имеет — иначе блокировка снималась бы сама собой на первом же
// тике. Сброс расхода (resetMasterUsage) сдвигает начало отсчёта: показания awg-ctrl
// уменьшаются на usageBase, поэтому прежний трафик не возвращается. Ручную блокировку тик не
// трогает.
func (s *Sub) enforceMasters() {
	rows, err := s.a.db.Query("SELECT " + masterCols + " FROM master_keys m WHERE m.revoked_at IS NULL ORDER BY id")
	if err != nil {
		warnf("sub: ограничения ключей: %v", err)
		return
	}
	var list []*masterRow
	for rows.Next() {
		if m := scanMaster(rows, false); m != nil {
			list = append(list, m)
		}
	}
	rows.Close()
	t := now()
	for _, m := range list {
		rx, tx, complete := s.masterUsage(m)
		brx, btx := s.usageBase(m.ID)
		if complete {
			// Счётчики awg-ctrl уменьшились (перезапуск интерфейса, пересоздание пира) —
			// поправка устарела: считаем заново с текущих показаний, как usage.accrue.
			if rx < brx || tx < btx {
				brx, btx = rx, tx
				s.setUsageBase(m.ID, brx, btx)
			}
		} else {
			// Неполные данные: поправку не двигаем — вернувшийся сервер дал бы скачок, — но
			// и отрицательного расхода не бывает: ниже поправки не опускаемся.
			if rx < brx {
				rx = brx
			}
			if tx < btx {
				tx = btx
			}
		}
		rx, tx = rx-brx, tx-btx
		if rx+tx > m.usedTotal() {
			m.UsedRx, m.UsedTx = rx, tx
			s.a.db.Exec("UPDATE master_keys SET used_rx = ?, used_tx = ? WHERE id = ?", rx, tx, m.ID)
		}
		if m.BlockedReason.Valid && (m.BlockedReason.String == blockManual || m.BlockedReason.String == blockAPI) {
			continue
		}
		want := ""
		if m.expired(t) {
			want = blockExpired
		} else if m.TrafficLimit.Valid && m.usedTotal() >= m.TrafficLimit.Int64 {
			want = blockTraffic
		}
		cur := ""
		if m.BlockedReason.Valid {
			cur = m.BlockedReason.String
		}
		if want == cur {
			continue
		}
		if want == "" {
			s.a.db.Exec("UPDATE master_keys SET blocked_at = NULL, blocked_reason = NULL WHERE id = ?", m.ID)
			logf("sub: мастер-ключ «%s» разблокирован: ограничение снято", m.Label)
			continue
		}
		s.a.db.Exec("UPDATE master_keys SET blocked_at = ?, blocked_reason = ? WHERE id = ?", t, want, m.ID)
		logf("sub: мастер-ключ «%s» заблокирован: %s", m.Label, want)
	}
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
		var defaults any
		if d := s.defaultServers(); d != nil {
			defaults = intsJSON(d)
		}
		return c.JSON(200, jsjson.Object{{Key: "enabled", Val: s.key() != nil}, {Key: "tls", Val: s.tlsPin != nil}, {Key: "keys", Val: s.listMasters(nil)},
			{Key: "defaults", Val: defaults}})
	})
	// «Серверы для всех»: добавить/снять серверы у всех ключей разом и задать набор для новых.
	rt.Post("/servers", aw(func(c *Ctx) (int, any, error) {
		out, err := s.bulkServers(c.Body)
		return 200, out, err
	}))
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
	// Сброс счётчика трафика — до /:id: подпуть не должен зависеть от порядка объявления
	// (роутер сверяет пути целиком, но держим это правило, как в остальных местах репозитория).
	rt.Post("/:id/usage/reset", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		if _, err := s.resetMasterUsage(m); err != nil {
			return 0, nil, err
		}
		return 200, s.masterOutByID(m.ID), nil // как block/unblock: панель получает ключ целиком
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
	// Ручная блокировка/разблокировка: пиры снимаются и возвращаются сведением, ключ остаётся.
	rt.Post("/:id/block", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		reason, _ := c.Obj().Get2("reason").(string)
		out, err := s.blockMaster(m, reason)
		return 200, out, err
	}))
	rt.Post("/:id/unblock", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.unblockMaster(m), nil
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
		list, down := s.masterDevices(m)
		return 200, jsjson.Object{{Key: "devices", Val: list}, {Key: "unreachable", Val: intsJSON(down)}}, nil
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
	// Сброс счётчика трафика: биллинг обнуляет расход при продлении подписки. Ответ — без
	// остальных полей ключа: внешней системе нужен только факт и новый счётчик. Объявлен до
	// /:id — подпуть не должен зависеть от порядка маршрутов.
	rt.Post("/:id/usage/reset", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.resetMasterUsage(m)
		return 200, out, err
	}))
	// Блокировка/разблокировка — до /:id, как сброс счётчика. Ответ — тот же masterOut, что у
	// PATCH: клиент сразу видит blocked/blockReason. Причина по умолчанию — api.
	rt.Post("/:id/block", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		reason, err := apiBlockReason(c.Body)
		if err != nil {
			return 0, nil, err
		}
		out, err := s.blockMaster(m, reason)
		return 200, out, err
	}))
	rt.Post("/:id/unblock", aw(func(c *Ctx) (int, any, error) {
		m, err := master(c)
		if err != nil {
			return 0, nil, err
		}
		return 200, s.unblockMaster(m), nil
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
		list, _ := s.masterDevices(m)
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
		list, _ := s.masterDevices(m)
		return 200, jsjson.Object{{Key: "devices", Val: list}}, nil
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
	s.a.db.Exec("DELETE FROM master_server_usage WHERE server_id = ?", id)
	s.a.db.Exec("DELETE FROM master_keys WHERE revoked_at IS NOT NULL AND id NOT IN (SELECT master_id FROM master_servers)")
}

// healDeadServers — самозалечивание при старте: ссылки на серверы, которых в реестре больше
// нет. Остаются после жёстких удалений и от версий панели до вето beforeServerDelete: живой
// мастер-ключ с мёртвым сервером для всех API-ключей отвечает 403 «вне области» (мёртвый id не
// входит ни в одну область) и не лечится никак, пока его не тронут в панели вручную.
// Мёртвые ссылки чистятся (это же делает afterServerDelete при штатном удалении), ключам без
// единого сервера даётся набор по умолчанию — тот же выбор, что у createMaster; заявки пиров
// его устройств на новом сервере сведёт reconcile, когда нода выйдет в онлайн (device_peers
// со ставкой peer_pub = NULL).
func (s *Sub) healDeadServers() {
	valid := map[int]bool{}
	for _, id := range s.a.nodes.IDs() {
		valid[id] = true
	}
	if len(valid) == 0 {
		return // реестр пуст — панель ещё не настроена, сравнивать не с чем
	}
	ids := make([]string, 0, len(valid))
	for id := range valid {
		ids = append(ids, fmt.Sprintf("%d", id))
	}
	inList := strings.Join(ids, ",")
	for _, q := range []string{
		"DELETE FROM master_servers WHERE server_id NOT IN (" + inList + ")",
		"DELETE FROM master_server_usage WHERE server_id NOT IN (" + inList + ")",
		"DELETE FROM device_peers WHERE server_id NOT IN (" + inList + ")",
		"DELETE FROM peer_tombstones WHERE server_id NOT IN (" + inList + ")",
		"DELETE FROM server_profiles WHERE server_id NOT IN (" + inList + ")",
	} {
		s.a.db.Exec(q)
	}
	targets := s.defaultServers()
	if len(targets) == 0 {
		if d := s.a.nodes.DefaultID(); valid[d] {
			targets = []int{d}
		}
	}
	if len(targets) == 0 {
		return // живого сервера нет — ключам без серверов дать нечего
	}
	rows, err := s.a.db.Query("SELECT " + masterCols + " FROM master_keys m WHERE m.revoked_at IS NULL AND m.id NOT IN (SELECT master_id FROM master_servers)")
	if err != nil {
		warnf("sub: самозалечивание: %v", err)
		return
	}
	var empty []*masterRow
	for rows.Next() {
		if m := scanMaster(rows, false); m != nil {
			empty = append(empty, m)
		}
	}
	rows.Close()
	if len(empty) == 0 {
		return
	}
	if err := s.a.tx(func(tx *sql.Tx) error {
		for _, m := range empty {
			devices := s.devicesOf(tx, m.ID)
			for _, sid := range targets {
				if _, err := tx.Exec("INSERT OR IGNORE INTO master_servers (master_id, server_id) VALUES (?, ?)", m.ID, sid); err != nil {
					return err
				}
				for _, d := range devices {
					if _, err := tx.Exec("INSERT OR IGNORE INTO device_peers (device_id, server_id) VALUES (?, ?)", d.ID, sid); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		warnf("sub: самозалечивание (назначение серверов): %v", err)
		return
	}
	logf("sub: серверы-сироты вычищены, ключам без серверов назначено %v: %d шт.", targets, len(empty))
}
