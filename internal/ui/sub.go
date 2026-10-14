package ui

// sen://-подписка (порт awg-ui/sub.ts): реестр мастер-ключей/устройств (ui.db), админ-роуты
// панели (/ui/masterkeys, /ui/devices, за JWT) и публичный листенер /sub/v1/* на отдельном
// порту SUB_PORT. Протокол и формат ссылки — docs/sen-link.md.
//
// awg-ctrl про мастер-ключи ничего не знает: сюда он отдаёт только пиров по публичному ключу
// (/api/peers*) и общий профиль сервера (/api/profile).
//
// Несколько серверов. Мастер-ключ отдаёт набор серверов (master_servers): локальный awg-ctrl
// (id 0) и/или ноды. Устройство регистрирует один ключ WireGuard, и он становится пиром на
// каждом сервере набора — IP и PSK на каждом свои (device_peers). Нода может быть не в сети,
// поэтому изменения не «толкаются», а сводятся: device_peers хранит, какой ключ сейчас стоит на
// сервере, peer_tombstones — что там надо удалить, а reconcile() приводит сервер к нужному
// состоянию, как только до него можно достучаться (сразу при действии, при подключении ноды
// и раз в минуту).
//
// Каждый проход заканчивается сведением пиров панели (PUT /api/peers/sync): awg-ctrl получает
// полный список ключей, которые этой панели нужны на сервере, и снимает из её области всё
// остальное. Область — id панели в owner пира («<panel>/m1/d2»): у ноды может быть своя панель
// на том же awg-ctrl, чужих не трогаем. Удаляем только по такому списку, никогда по времени.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	tsWindow     = 300 // секунд, ±
	defaultLimit = 3
	maxServers   = 32 // серверов в одном мастер-ключе
	syncEvery    = 60 * time.Second
	auditHour    = time.Hour
	auditFirst   = 2 * time.Minute // первый аудит — вскоре после старта, когда ноды подключились
)

var (
	reB64Pub        = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
	reClientVersion = regexp.MustCompile(`^[\w.+-]{1,32}$`) // версия приложения, чисто информационная
	reSubSecret     = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	reDeviceID      = regexp.MustCompile(`^[\w.-]{1,64}$`)
	reAuthPub       = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// pubParam — pub_key WireGuard в пути к awg-ctrl: base64url (в обычном base64 есть «/»).
func pubParam(pub string) string { return b64url.EncodeToString(fromB64(pub)) }

// ── Простой лимитер по IP ──────────────────────────────────────────────────
// onLimited зовётся один раз за окно — на первом отказе, чтобы лог не забивало.
type limiter struct {
	mu        sync.Mutex
	hits      map[string]*rateEntry
	max       int
	window    time.Duration
	onLimited func(ip string)
}

func newLimiter(max int, window time.Duration, onLimited func(string)) *limiter {
	return &limiter{hits: map[string]*rateEntry{}, max: max, window: window, onLimited: onLimited}
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	t := nowFn()
	e := l.hits[ip]
	if e == nil || e.resetAt.Before(t) {
		l.hits[ip] = &rateEntry{1, t.Add(l.window)}
		l.mu.Unlock()
		return true
	}
	if e.count >= l.max {
		fire := e.count == l.max
		if fire {
			e.count++
		}
		l.mu.Unlock()
		if fire && l.onLimited != nil {
			l.onLimited(ip)
		}
		return false
	}
	e.count++
	l.mu.Unlock()
	return true
}

// ── Строки реестра ──────────────────────────────────────────────────────────

type masterRow struct {
	ID          int64
	UUID        string
	Label       string
	Secret      []byte
	DeviceLimit int64
	ServerID    int64
	CreatedAt   int64
	RevokedAt   sql.NullInt64
	Devices     int64 // только у запросов со счётчиком
}

type deviceRow struct {
	ID             int64
	MasterID       int64
	DeviceID       string
	DeviceName     string
	Platform       string
	PubKey         string
	AuthPub        string
	CreatedAt      int64
	LastSeen       sql.NullInt64
	RekeyRequested int64
	Version        string
}

type linkRow struct {
	DeviceID   int64
	ServerID   int
	PeerPub    sql.NullString
	IP         sql.NullString
	PSK        sql.NullString
	PSKPending int64
	DevPub     string // linksToFix
	MasterID   int64  // linksToFix
}

const (
	masterCols = "m.id, m.uuid, m.label, m.secret, m.device_limit, m.server_id, m.created_at, m.revoked_at"
	deviceCols = "id, master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, last_seen, rekey_requested, version"
	linkCols   = "device_id, server_id, peer_pub, ip, psk, psk_pending"
)

func scanMaster(row interface{ Scan(...any) error }, withCount bool) *masterRow {
	m := &masterRow{}
	var u sql.NullString
	dst := []any{&m.ID, &u, &m.Label, &m.Secret, &m.DeviceLimit, &m.ServerID, &m.CreatedAt, &m.RevokedAt}
	if withCount {
		dst = append(dst, &m.Devices)
	}
	if row.Scan(dst...) != nil {
		return nil
	}
	m.UUID = u.String
	return m
}

func scanDevice(row interface{ Scan(...any) error }) *deviceRow {
	d := &deviceRow{}
	if row.Scan(&d.ID, &d.MasterID, &d.DeviceID, &d.DeviceName, &d.Platform, &d.PubKey, &d.AuthPub, &d.CreatedAt, &d.LastSeen, &d.RekeyRequested, &d.Version) != nil {
		return nil
	}
	return d
}

type Sub struct {
	a       *App
	port    int
	useTLS  bool
	host    string
	panelID string
	auditOn bool

	keyMu   sync.RWMutex
	signKey ed25519.PrivateKey // nil — подписка выключена
	signPub []byte
	tlsCert *tls.Certificate
	tlsPin  []byte

	locksMu sync.Mutex
	locks   map[int]*sync.Mutex // один проход на сервер за раз

	sigMu    sync.Mutex
	seenSigs map[string]time.Time // подпись → когда забыть

	registerLimit, failLimit *limiter

	auditMu       sync.Mutex
	lastAudit     *auditReport
	auditSettings auditSettings
	cleanedLog    []cleanedPeer
	auditing      chan struct{} // идёт проход: закроется по окончании
}

func newSub(a *App) (*Sub, error) {
	s := &Sub{
		a: a, port: envNum("SUB_PORT"),
		useTLS:   strings.ToLower(envOr("SUB_TLS", "off")) == "on",
		host:     strings.TrimSpace(os.Getenv("SUB_HOST")),
		auditOn:  strings.ToLower(envOr("AUDIT", "on")) != "off",
		locks:    map[int]*sync.Mutex{},
		seenSigs: map[string]time.Time{},
	}
	limited := func(what string) func(string) {
		return func(ip string) {
			warnf("sub: 429 %s ip=%s — лимит исчерпан на 15 минут", what, ip)
		}
	}
	s.registerLimit = newLimiter(30, 15*time.Minute, limited("register"))
	s.failLimit = newLimiter(30, 15*time.Minute, limited("неудачные запросы"))

	file := func(env, name string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return filepath.Join(a.baseDir, name)
	}
	if raw, err := os.ReadFile(file("SUB_SIGN_KEY_FILE", "sub_sign.key")); err == nil {
		if k, err := loadEd25519Private(raw); err == nil {
			s.signKey, s.signPub = k, k.Public().(ed25519.PublicKey)
		}
	}
	if s.signKey == nil {
		warnf("sub: ключ подписи не найден — sen://-подписка отключена")
	}
	if s.useTLS {
		keyF, crtF := file("SUB_TLS_KEY_FILE", "sub_tls.key"), file("SUB_TLS_CERT_FILE", "sub_tls.crt")
		crt, err1 := os.ReadFile(crtF)
		cert, err2 := tls.LoadX509KeyPair(crtF, keyF)
		var pin []byte
		var err3 error
		if err1 == nil {
			pin, err3 = spkiPin(crt)
		}
		if err1 != nil || err2 != nil || err3 != nil {
			warnf("sub: SUB_TLS=on, но сертификат не найден — подписка отключена")
			s.signKey = nil
		} else {
			s.tlsCert, s.tlsPin = &cert, pin
		}
	}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	s.loadAudit()
	// Как в Node: подписки на события реестра — при создании, до старта хаба и до
	// проверок ключей API (порядок вето при удалении сервера тот же).
	a.nodes.OnOnline(func(id int) { go s.reconcile(id) })
	a.nodes.BeforeDelete(s.beforeServerDelete)
	a.nodes.AfterDelete(s.afterServerDelete)
	return s, nil
}

func (s *Sub) initSchema() error {
	db := s.a.db
	if _, err := db.Exec(fmt.Sprintf(`
        CREATE TABLE IF NOT EXISTS master_keys (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            label        TEXT    NOT NULL,
            secret       BLOB    NOT NULL UNIQUE,
            device_limit INTEGER NOT NULL DEFAULT %d,
            server_id    INTEGER NOT NULL DEFAULT 0,
            created_at   INTEGER NOT NULL,
            revoked_at   INTEGER
        );
        CREATE TABLE IF NOT EXISTS devices (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            master_id       INTEGER NOT NULL REFERENCES master_keys(id) ON DELETE CASCADE,
            device_id       TEXT    NOT NULL,
            device_name     TEXT    NOT NULL DEFAULT '',
            platform        TEXT    NOT NULL DEFAULT '',
            pub_key         TEXT    NOT NULL UNIQUE,
            auth_pub        TEXT    NOT NULL UNIQUE,
            created_at      INTEGER NOT NULL,
            last_seen       INTEGER,
            rekey_requested INTEGER NOT NULL DEFAULT 0,
            UNIQUE (master_id, device_id)
        );
        -- Серверы мастер-ключа. master_keys.server_id остался от времени одного сервера и
        -- дальше не читается (кроме переноса ниже).
        CREATE TABLE IF NOT EXISTS master_servers (
            master_id INTEGER NOT NULL REFERENCES master_keys(id) ON DELETE CASCADE,
            server_id INTEGER NOT NULL,
            PRIMARY KEY (master_id, server_id)
        );
        -- Пир устройства на сервере. peer_pub — ключ, который сейчас стоит на сервере
        -- (NULL — ещё не добавлен); расходится с devices.pub_key после смены ключа, пока
        -- сервер не догнали. ip/psk — как их выдал awg-ctrl этого сервера.
        CREATE TABLE IF NOT EXISTS device_peers (
            device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            server_id   INTEGER NOT NULL,
            peer_pub    TEXT,
            ip          TEXT,
            psk         TEXT,
            psk_pending INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY (device_id, server_id)
        );
        -- Пиры, которые надо удалить с сервера, когда до него дотянемся.
        CREATE TABLE IF NOT EXISTS peer_tombstones (
            server_id INTEGER NOT NULL,
            pub_key   TEXT    NOT NULL,
            PRIMARY KEY (server_id, pub_key)
        );
        -- Последний известный профиль сервера: config собирается из него, даже когда
        -- нода не в сети, — иначе сервер пропадал бы из подписки на время обрыва.
        CREATE TABLE IF NOT EXISTS server_profiles (
            server_id  INTEGER PRIMARY KEY,
            profile    TEXT    NOT NULL,
            updated_at INTEGER NOT NULL
        );
        CREATE TABLE IF NOT EXISTS sub_meta (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );
    `, defaultLimit)); err != nil {
		return err
	}
	// Версия клиента — необязательное поле, добавленное позже.
	if !hasColumn(db, "devices", "version") {
		if _, err := db.Exec("ALTER TABLE devices ADD COLUMN version TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// uuid мастер-ключа — постоянный идентификатор; существующим ключам выдаётся здесь один раз.
	if !hasColumn(db, "master_keys", "uuid") {
		if _, err := db.Exec("ALTER TABLE master_keys ADD COLUMN uuid TEXT"); err != nil {
			return err
		}
	}
	for _, id := range ints(db, "SELECT id FROM master_keys WHERE uuid IS NULL") {
		db.Exec("UPDATE master_keys SET uuid = ? WHERE id = ?", uuid.NewString(), id)
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS master_keys_uuid ON master_keys (uuid)"); err != nil {
		return err
	}
	// Перенос со времени одного сервера (см. sub.ts).
	if _, err := db.Exec(`
        INSERT OR IGNORE INTO master_servers (master_id, server_id)
            SELECT id, server_id FROM master_keys
            WHERE revoked_at IS NULL AND id NOT IN (SELECT master_id FROM master_servers);
        INSERT OR IGNORE INTO device_peers (device_id, server_id, peer_pub)
            SELECT d.id, m.server_id, d.pub_key FROM devices d JOIN master_keys m ON m.id = d.master_id
            WHERE d.id NOT IN (SELECT device_id FROM device_peers);
    `); err != nil {
		return err
	}
	// Id панели — область её пиров на серверах. Копия в panel.id нужна CLI.
	pb := make([]byte, 6)
	rand.Read(pb)
	db.Exec("INSERT OR IGNORE INTO sub_meta (key, value) VALUES ('panel_id', ?)", hex.EncodeToString(pb))
	if err := db.QueryRow("SELECT value FROM sub_meta WHERE key = 'panel_id'").Scan(&s.panelID); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.a.baseDir, "panel.id"), []byte(s.panelID+"\n"), 0o644); err != nil {
		warnf("sub: не удалось записать panel.id: %v", err)
	}
	return nil
}

// ── Запросы ─────────────────────────────────────────────────────────────────

func (s *Sub) masterByID(q queryer, id int64) *masterRow {
	return scanMaster(q.QueryRow("SELECT "+masterCols+" FROM master_keys m WHERE id = ? AND revoked_at IS NULL", id), false)
}

func (s *Sub) masterRowByID(q queryer, id int64) *masterRow {
	return scanMaster(q.QueryRow("SELECT "+masterCols+", (SELECT COUNT(*) FROM devices d WHERE d.master_id = m.id) AS devices FROM master_keys m WHERE id = ?", id), true)
}

func (s *Sub) masterBySecret(secret []byte) *masterRow {
	return scanMaster(s.a.db.QueryRow("SELECT "+masterCols+" FROM master_keys m WHERE secret = ? AND revoked_at IS NULL", secret), false)
}

func (s *Sub) deviceByID(q queryer, id int64) *deviceRow {
	return scanDevice(q.QueryRow("SELECT "+deviceCols+" FROM devices WHERE id = ?", id))
}

func (s *Sub) devicesOf(q queryer, masterID int64) []*deviceRow {
	rows, err := q.Query("SELECT "+deviceCols+" FROM devices WHERE master_id = ? ORDER BY id", masterID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*deviceRow
	for rows.Next() {
		if d := scanDevice(rows); d != nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *Sub) deviceCount(q queryer, masterID int64) int64 {
	var n int64
	q.QueryRow("SELECT COUNT(*) AS n FROM devices WHERE master_id = ?", masterID).Scan(&n)
	return n
}

func (s *Sub) linksOf(q queryer, deviceID int64) []*linkRow {
	rows, err := q.Query("SELECT "+linkCols+" FROM device_peers WHERE device_id = ? ORDER BY server_id", deviceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*linkRow
	for rows.Next() {
		l := &linkRow{}
		if rows.Scan(&l.DeviceID, &l.ServerID, &l.PeerPub, &l.IP, &l.PSK, &l.PSKPending) == nil {
			out = append(out, l)
		}
	}
	return out
}

func (s *Sub) serverIDsOf(q queryer, masterID int64) []int {
	return ints(q, "SELECT server_id FROM master_servers WHERE master_id = ? ORDER BY server_id", masterID)
}

func strs(q queryer, query string, args ...any) []string {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *Sub) linkSet(q queryer, peerPub, ip, psk any, pending int64, deviceID int64, serverID int) {
	q.Exec("UPDATE device_peers SET peer_pub = ?, ip = ?, psk = ?, psk_pending = ? WHERE device_id = ? AND server_id = ?",
		peerPub, ip, psk, pending, deviceID, serverID)
}

func (s *Sub) tombCount(id int) int64 {
	var n int64
	s.a.db.QueryRow("SELECT COUNT(*) AS n FROM peer_tombstones WHERE server_id = ?", id).Scan(&n)
	return n
}

// keepOf — что панели нужно на сервере: текущий ключ каждого устройства и ключ, который там
// стоит сейчас (расходится с текущим, пока смена ключа не догнала сервер).
func (s *Sub) keepOf(id int) []string {
	return strs(s.a.db, `
            SELECT d.pub_key AS pub FROM device_peers l JOIN devices d ON d.id = l.device_id WHERE l.server_id = ?
            UNION SELECT peer_pub FROM device_peers WHERE server_id = ? AND peer_pub IS NOT NULL`, id, id)
}

func ok2xx(st int) bool { return st >= 200 && st < 300 }

// gone — связь пропала посреди прохода.
func gone(st int) bool { return st == 502 || st == 504 }

// ── Сведение серверов ───────────────────────────────────────────────────────

func (s *Sub) storedProfile(id int) jsjson.Object {
	var raw string
	if s.a.db.QueryRow("SELECT profile FROM server_profiles WHERE server_id = ?", id).Scan(&raw) != nil {
		return nil
	}
	v, err := jsjson.Decode([]byte(raw))
	if err != nil {
		return nil
	}
	o, _ := v.(jsjson.Object)
	return o
}

func (s *Sub) refreshProfile(id int, ctrl Ctrl) (bool, error) {
	r, err := ctrl("GET", "/api/profile", nil)
	if err != nil {
		return false, err
	}
	if _, isStr := field(r.Data, "endpoint").(string); !ok2xx(r.Status) || !isStr {
		return false, nil
	}
	js := string(jsjson.MustMarshal(r.Data))
	var cur string
	if s.a.db.QueryRow("SELECT profile FROM server_profiles WHERE server_id = ?", id).Scan(&cur) != nil || cur != js {
		s.a.db.Exec("INSERT INTO server_profiles (server_id, profile, updated_at) VALUES (?, ?, ?) ON CONFLICT(server_id) DO UPDATE SET profile = excluded.profile, updated_at = excluded.updated_at", id, js, now())
	}
	return true, nil
}

func (s *Sub) lock(id int) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	m := s.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// reconcile — один проход на сервер за раз: параллельные проходы добавили бы одного пира дважды.
func (s *Sub) reconcile(id int) {
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	if err := s.reconcileNow(id); err != nil {
		warnf("sub: сервер #%d не сведён: %v", id, err)
	}
}

func (s *Sub) reconcileAll(ids []int) {
	seen := map[int]bool{}
	var wg sync.WaitGroup
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		wg.Add(1)
		go func() { defer wg.Done(); s.reconcile(id) }()
	}
	wg.Wait()
}

type peerInfo struct{ ip, psk any }

func (s *Sub) reconcileNow(id int) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	if !s.a.nodes.Exists(id) || !s.a.nodes.IsOnline(id) {
		return nil
	}
	ctrl := s.a.nodes.CtrlFor(id)
	if ok, err := s.refreshProfile(id, ctrl); err != nil || !ok {
		return err // сервер не отвечает — попробуем позже
	}

	// Список пиров сервера — лениво, только если понадобится (409, потерянный ip/psk).
	var listed map[string]jsjson.Object
	onServer := func(pub string) (*peerInfo, error) {
		if listed == nil {
			r, err := ctrl("GET", "/api/peers", nil)
			if err != nil {
				return nil, err
			}
			if !ok2xx(r.Status) {
				return nil, fmt.Errorf("peers %d", r.Status)
			}
			listed = map[string]jsjson.Object{}
			for _, p := range arr(field(r.Data, "peers")) {
				if o, ok := p.(jsjson.Object); ok {
					if k, ok := o.Get2("pub_key").(string); ok {
						listed[k] = o
					}
				}
			}
		}
		if p, ok := listed[pub]; ok {
			return &peerInfo{p.Get2("ip"), p.Get2("psk_key")}, nil
		}
		return nil, nil
	}
	asPeer := func(d any) *peerInfo { return &peerInfo{field(d, "ip"), field(d, "psk_key")} }

	for _, pub := range strs(s.a.db, "SELECT pub_key FROM peer_tombstones WHERE server_id = ?", id) {
		r, err := ctrl("DELETE", "/api/peers/"+pubParam(pub), nil)
		if err != nil {
			return err
		}
		if ok2xx(r.Status) || r.Status == 404 {
			s.a.db.Exec("DELETE FROM peer_tombstones WHERE server_id = ? AND pub_key = ?", id, pub)
		} else if gone(r.Status) {
			return nil
		}
	}

	rows, err := s.a.db.Query(`
            SELECT l.device_id, l.server_id, l.peer_pub, l.ip, l.psk, l.psk_pending, d.pub_key AS dev_pub, d.master_id
            FROM device_peers l JOIN devices d ON d.id = l.device_id
            WHERE l.server_id = ? AND (l.peer_pub IS NULL OR l.peer_pub <> d.pub_key
                                       OR l.ip IS NULL OR l.psk IS NULL OR l.psk_pending = 1)`, id)
	if err != nil {
		return err
	}
	var fix []*linkRow
	for rows.Next() {
		l := &linkRow{}
		if rows.Scan(&l.DeviceID, &l.ServerID, &l.PeerPub, &l.IP, &l.PSK, &l.PSKPending, &l.DevPub, &l.MasterID) == nil {
			fix = append(fix, l)
		}
	}
	rows.Close()

	errGone := errors.New("gone")
	for _, l := range fix {
		// nil — не вышло, попробуем в следующий проход; errGone — связь пропала, проход прекращаем.
		add := func() (*peerInfo, error) {
			r, err := ctrl("POST", "/api/peers", jsjson.Object{{Key: "pub_key", Val: l.DevPub},
				{Key: "owner", Val: fmt.Sprintf("%s/m%d/d%d", s.panelID, l.MasterID, l.DeviceID)}})
			if err != nil {
				return nil, err
			}
			switch {
			case ok2xx(r.Status):
				return asPeer(r.Data), nil
			case r.Status == 409: // уже добавлен, но ответ потерялся
				return onServer(l.DevPub)
			case gone(r.Status):
				return nil, errGone
			}
			return nil, nil
		}

		var peer *peerInfo
		var perr error
		switch {
		case !l.PeerPub.Valid:
			peer, perr = add()
		case l.PeerPub.String != l.DevPub:
			// Ключ сменили, пока сервер был недоступен: тот же IP и PSK, новый ключ.
			r, err := ctrl("PUT", "/api/peers/"+pubParam(l.PeerPub.String), jsjson.Object{{Key: "pub_key", Val: l.DevPub}})
			if err != nil {
				return err
			}
			switch {
			case ok2xx(r.Status):
				peer = asPeer(r.Data)
			case r.Status == 404: // старого пира на сервере уже нет
				peer, perr = add()
			case r.Status == 409:
				peer, perr = onServer(l.DevPub)
			case gone(r.Status):
				perr = errGone
			}
		default:
			// ip/psk не знаем или пир пропал с сервера
			if peer, perr = onServer(l.DevPub); perr == nil && peer == nil {
				peer, perr = add()
			}
		}
		if perr == errGone {
			return nil
		}
		if perr != nil {
			return perr
		}
		if peer == nil {
			continue
		}
		var pend int64
		if l.PSKPending == 1 {
			r, err := ctrl("POST", "/api/peers/"+pubParam(l.DevPub)+"/psk", nil)
			if err != nil {
				return err
			}
			switch {
			case ok2xx(r.Status):
				peer = asPeer(r.Data)
			case gone(r.Status):
				return nil
			default:
				pend = 1 // пир на месте, PSK сменим позже
			}
		}
		s.linkSet(s.a.db, l.DevPub, peer.ip, peer.psk, pend, l.DeviceID, id)
	}
	return s.syncPeers(id, ctrl)
}

// syncPeers — сведение пиров панели на сервере, в конце прохода и под его блокировкой.
func (s *Sub) syncPeers(id int, ctrl Ctrl) error {
	keep := []any{}
	for _, k := range s.keepOf(id) {
		keep = append(keep, k)
	}
	r, err := ctrl("PUT", "/api/peers/sync", jsjson.Object{{Key: "owner", Val: s.panelID}, {Key: "keep", Val: keep}})
	if err != nil {
		return err
	}
	if ok2xx(r.Status) {
		// Самолечение: ключ считается стоящим, а его нет — забываем и ставим заново следующим проходом.
		present := map[string]bool{}
		for _, p := range arr(field(r.Data, "present")) {
			if ps, ok := p.(string); ok {
				present[ps] = true
			}
		}
		rows, err := s.a.db.Query(`
            SELECT l.device_id, l.peer_pub FROM device_peers l JOIN devices d ON d.id = l.device_id
            WHERE l.server_id = ? AND l.peer_pub = d.pub_key`, id)
		if err != nil {
			return err
		}
		type settled struct {
			dev int64
			pub string
		}
		var all []settled
		for rows.Next() {
			var x settled
			if rows.Scan(&x.dev, &x.pub) == nil {
				all = append(all, x)
			}
		}
		rows.Close()
		lost := 0
		for _, x := range all {
			if present[x.pub] {
				continue
			}
			s.linkSet(s.a.db, nil, nil, nil, 0, x.dev, id)
			lost++
		}
		if lost > 0 {
			go s.reconcile(id)
		}
	} else if r.Status != 400 && r.Status != 413 {
		return nil // 400/413 — awg-ctrl старше sync: чистим, как раньше, одними надгробиями
	}
	if s.tombCount(id) > 0 {
		return nil
	}
	s.a.db.Exec("DELETE FROM master_servers WHERE server_id = ? AND master_id IN (SELECT id FROM master_keys WHERE revoked_at IS NOT NULL)", id)
	s.a.db.Exec("DELETE FROM master_keys WHERE revoked_at IS NOT NULL AND id NOT IN (SELECT master_id FROM master_servers)")
	return nil
}

// forgetDevice — устройство удаляется: его пиров — в список на удаление, строку — из реестра.
// Внутри уже открытой транзакции — с её tx.
func (s *Sub) forgetDevice(q queryer, dev *deviceRow) []int {
	links := s.linksOf(q, dev.ID)
	for _, l := range links {
		if l.PeerPub.Valid {
			q.Exec("INSERT OR IGNORE INTO peer_tombstones (server_id, pub_key) VALUES (?, ?)", l.ServerID, l.PeerPub.String)
		}
	}
	q.Exec("DELETE FROM devices WHERE id = ?", dev.ID) // device_peers уйдут каскадом
	ids := []int{}
	for _, l := range links {
		ids = append(ids, l.ServerID)
	}
	return ids
}

func (s *Sub) forgetDeviceTx(dev *deviceRow) []int {
	var ids []int
	s.a.tx(func(tx *sql.Tx) error { ids = s.forgetDevice(tx, dev); return nil })
	return ids
}

// ── config ──────────────────────────────────────────────────────────────────

// hostOfEndpoint — endpoint.slice(0, endpoint.lastIndexOf(":")).
func hostOfEndpoint(ep string) string {
	i := strings.LastIndex(ep, ":")
	if i < 0 {
		u := utf16Len(ep)
		if u == 0 {
			return ""
		}
		return sliceUTF16(ep, u-1)
	}
	return ep[:i]
}

func (s *Sub) subHost(c *Ctx) string {
	if s.host != "" {
		return s.host
	}
	if s.a.nodes.Exists(0) {
		if p := s.storedProfile(0); p != nil {
			if ep, ok := p.Get2("endpoint").(string); ok {
				return hostOfEndpoint(ep)
			}
		}
	}
	if c == nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(c.hostname(), "["), "]")
}

// buildConfig — в servers[] только серверы, где уже стоит текущий ключ устройства и известны
// ip/psk. rev меняется при любом изменении; клиент по нему решает, трогать ли туннель.
func (s *Sub) buildConfig(dev *deviceRow, c *Ctx) jsjson.Object {
	list := []any{}
	for _, l := range s.linksOf(s.a.db, dev.ID) {
		prof := s.storedProfile(l.ServerID)
		if prof == nil || !l.PeerPub.Valid || l.PeerPub.String != dev.PubKey || !l.IP.Valid || l.IP.String == "" || !l.PSK.Valid || l.PSK.String == "" {
			continue
		}
		// Поле, которого в профиле нет, JSON.stringify опустил бы (undefined) — опускаем и мы:
		// от этого зависит rev.
		e := jsjson.Object{{Key: "id", Val: l.ServerID}}
		pf := func(k string) {
			if v, ok := prof.Get(k); ok {
				e = append(e, jsjson.KV{Key: k, Val: v})
			}
		}
		pf("name")
		pf("endpoint")
		pf("server_pub")
		e = append(e, jsjson.KV{Key: "psk", Val: l.PSK.String}, jsjson.KV{Key: "address", Val: l.IP.String + "/32"})
		for _, k := range []string{"dns", "keepalive", "mtu", "gen", "awg"} {
			pf(k)
		}
		list = append(list, e)
	}
	cfg := jsjson.Object{
		{Key: "rekey", Val: dev.RekeyRequested == 1},
		{Key: "endpoints", Val: []any{fmt.Sprintf("%s:%d", s.subHost(c), s.port)}},
		{Key: "servers", Val: list},
	}
	sum := sha256.Sum256(jsjson.MustMarshal(cfg))
	return append(jsjson.Object{{Key: "rev", Val: hex.EncodeToString(sum[:])[:16]}}, cfg...)
}

func configServers(cfg jsjson.Object) int { return len(arr(cfg.Get2("servers"))) }

// readyConfig — конфиг, в котором есть хотя бы один сервер. Пустой не отдаём: клиент снёс бы
// туннель из-за временной недоступности нод.
func (s *Sub) readyConfig(dev *deviceRow, c *Ctx) (jsjson.Object, error) {
	cfg := s.buildConfig(dev, c)
	if configServers(cfg) == 0 {
		var ids []int
		for _, l := range s.linksOf(s.a.db, dev.ID) {
			ids = append(ids, l.ServerID)
		}
		s.reconcileAll(ids)
		if d := s.deviceByID(s.a.db, dev.ID); d != nil {
			cfg = s.buildConfig(d, c)
		}
	}
	if configServers(cfg) == 0 {
		return nil, errors.New("no servers")
	}
	return cfg, nil
}

// ── Публичная часть: /sub/v1 ────────────────────────────────────────────────

func (s *Sub) key() ed25519.PrivateKey {
	s.keyMu.RLock()
	defer s.keyMu.RUnlock()
	return s.signKey
}

// send — ответ подписан и несёт эхо запроса: device (X-Sen-Device) и ts (X-Sen-Ts). Поля
// ответа важнее эха: device из тела register не перетрёт лишний заголовок.
func (s *Sub) send(w http.ResponseWriter, r *http.Request, status int, payload jsjson.Object) bool {
	out := jsjson.Object{}
	for _, h := range []struct{ hdr, key string }{{"X-Sen-Device", "device"}, {"X-Sen-Ts", "ts"}} {
		if v, ok := r.Header[h.hdr]; ok {
			if f := jsToNumber(strings.Join(v, ", ")); isSafeInt(f) {
				out = append(out, jsjson.KV{Key: h.key, Val: f})
			}
		}
	}
	for _, kv := range payload {
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
	writeJSON(w, status, signResponse(out, s.key()))
	return false
}

func (s *Sub) sendC(c *Ctx, status int, payload jsjson.Object) bool {
	return s.send(c.W, c.R, status, payload)
}

func subErr(code string) jsjson.Object { return jsjson.Object{{Key: "error", Val: code}} }

// reqPath — req.path: путь как пришёл, без query.
func reqPath(r *http.Request) string {
	p := r.RequestURI
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// signFailure — подпись запроса и защита от повторов. "" — всё верно, иначе причина (для лога).
func (s *Sub) signFailure(c *Ctx, authPub []byte) string {
	tsF := jsToNumber(strings.Join(c.R.Header["X-Sen-Ts"], ", "))
	if _, has := c.R.Header["X-Sen-Ts"]; !has {
		tsF = jsToNumber("x")
	}
	sig := c.R.Header.Get("X-Sen-Sig")
	if tsF != float64(int64(tsF)) || sig == "" {
		return "no_signature"
	}
	ts := int64(tsF)
	if d := ts - now(); d > tsWindow || d < -tsWindow {
		return fmt.Sprintf("ts_window (часы клиента расходятся на %d с)", d)
	}
	if !verifyRequest(c.R.Method, reqPath(c.R), ts, c.RawBody, sig, authPub) {
		return "bad_signature"
	}
	s.sigMu.Lock()
	defer s.sigMu.Unlock()
	t := nowFn()
	for k, exp := range s.seenSigs {
		if exp.Before(t) {
			delete(s.seenSigs, k)
		}
	}
	if _, ok := s.seenSigs[sig]; ok {
		return "replay"
	}
	s.seenSigs[sig] = t.Add(2 * tsWindow * time.Second)
	return ""
}

func (s *Sub) deviceAuth(c *Ctx) bool {
	raw, hasHdr := c.R.Header["X-Sen-Device"]
	f := jsToNumber(strings.Join(raw, ", "))
	valid := hasHdr && isSafeInt(f) && f > 0
	var dev *deviceRow
	if valid {
		dev = s.deviceByID(s.a.db, int64(f))
	}
	// Устройства нет — его удалили (id не переиспользуются). 410, чтобы клиент снял конфиг, и
	// не в лимит неудач: такие клиенты опрашивают по таймеру и выбивали бы лимит соседям за NAT.
	if dev == nil && valid {
		return s.sendC(c, 410, subErr("revoked"))
	}
	why := "no_device"
	if dev != nil {
		why = s.signFailure(c, fromB64url(dev.AuthPub))
	}
	if why == "" {
		c.device = dev
		return true
	}
	ip := c.remoteIP()
	if !s.failLimit.allow(ip) {
		return s.sendC(c, 429, subErr("rate_limited"))
	}
	hdr := c.R.Header.Get("X-Sen-Device")
	if !hasHdr {
		hdr = "-"
	}
	warnf("sub: 401 %s %s device=%s %s ip=%s", c.R.Method, reqPath(c.R), hdr, why, ip)
	return s.sendC(c, 401, subErr("unauthorized"))
}

// wrapSub — ошибки обработчика (awg-ctrl не ответил, нет серверов) — подписанный 502.
func (s *Sub) wrapSub(fn func(c *Ctx) error) handler {
	return func(c *Ctx) bool {
		defer func() {
			if p := recover(); p != nil {
				s.sendC(c, 502, subErr("unavailable"))
			}
		}()
		if err := fn(c); err != nil {
			return s.sendC(c, 502, subErr("unavailable"))
		}
		return false
	}
}

var errLimit = errors.New("limit")

func (s *Sub) register(c *Ctx) error {
	ip := c.remoteIP()
	if !s.registerLimit.allow(ip) {
		s.sendC(c, 429, subErr("rate_limited"))
		return nil
	}
	b := c.Obj()
	strRe := func(k string, re *regexp.Regexp) (string, bool) {
		v, ok := b.Get2(k).(string)
		return v, ok && re.MatchString(v)
	}
	secretB, ok1 := strRe("sub", reSubSecret)
	deviceID, ok2 := strRe("device_id", reDeviceID)
	pubKey, ok3 := strRe("pub_key", reB64Pub)
	authPubS, ok4 := strRe("auth_pub", reAuthPub)
	name, platform := "", ""
	if v, ok := b.Get2("device_name").(string); ok {
		name = sliceUTF16(v, 64)
	}
	if v, ok := b.Get2("platform").(string); ok {
		platform = sliceUTF16(v, 32)
	}
	version, ok5 := strRe("version", reClientVersion)
	if !ok5 {
		version = ""
	}
	if !ok1 || !ok2 || !ok3 || !ok4 {
		s.sendC(c, 400, subErr("bad_request"))
		return nil
	}

	// Регистрация подписана ключом auth_pub из самого тела — доказательство владения.
	authPub := fromB64url(authPubS)
	why := "bad_auth_pub"
	if len(authPub) == 32 {
		why = s.signFailure(c, authPub)
	}
	if why != "" {
		warnf("sub: 401 register %s ip=%s", why, ip)
		s.sendC(c, 401, subErr("unauthorized"))
		return nil
	}

	master := s.masterBySecret(fromB64url(secretB))
	if master == nil {
		s.sendC(c, 404, subErr("not_found"))
		return nil
	}
	var serverIDs []int
	for _, id := range s.serverIDsOf(s.a.db, master.ID) {
		if s.a.nodes.Exists(id) {
			serverIDs = append(serverIDs, id)
		}
	}
	if len(serverIDs) == 0 {
		s.sendC(c, 502, subErr("unavailable"))
		return nil
	}

	var rowID int64
	var touched []int
	err := s.a.tx(func(tx *sql.Tx) error {
		if existing := scanDevice(tx.QueryRow("SELECT "+deviceCols+" FROM devices WHERE master_id = ? AND device_id = ?", master.ID, deviceID)); existing != nil {
			touched = s.forgetDevice(tx, existing) // переустановка — старые пиры на удаление
		} else if s.deviceCount(tx, master.ID) >= master.DeviceLimit {
			return errLimit
		}
		res, err := tx.Exec("INSERT INTO devices (master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			master.ID, deviceID, name, platform, pubKey, authPubS, now(), version)
		if err != nil {
			return err
		}
		rowID, _ = res.LastInsertId()
		for _, sid := range serverIDs {
			if _, err := tx.Exec("INSERT OR IGNORE INTO device_peers (device_id, server_id) VALUES (?, ?)", rowID, sid); err != nil {
				return err
			}
		}
		return nil
	})
	if err == errLimit {
		s.sendC(c, 403, subErr("device_limit"))
		return nil
	}
	if err != nil {
		s.sendC(c, 409, subErr("conflict")) // UNIQUE: pub_key/auth_pub уже заняты
		return nil
	}

	s.reconcileAll(append(touched, serverIDs...))
	dev := s.deviceByID(s.a.db, rowID)
	if dev == nil {
		return errors.New("device vanished")
	}
	cfg := s.buildConfig(dev, c)
	if configServers(cfg) == 0 {
		// Ни один сервер пира не принял — регистрацию откатываем, клиент повторит позже.
		s.reconcileAll(s.forgetDeviceTx(dev))
		s.sendC(c, 502, subErr("unavailable"))
		return nil
	}
	s.sendC(c, 201, jsjson.Object{{Key: "device", Val: dev.ID}, {Key: "config", Val: cfg}, {Key: "key_uuid", Val: master.UUID},
		{Key: "devices", Val: s.deviceCount(s.a.db, master.ID)}, {Key: "device_limit", Val: master.DeviceLimit}})
	return nil
}

// peek — сколько мест у ключа занято, по секрету из ссылки (устройства ещё нет).
func (s *Sub) peek(c *Ctx) error {
	ip := c.remoteIP()
	secretB, ok := c.Obj().Get2("sub").(string)
	ok = ok && reSubSecret.MatchString(secretB)
	var master *masterRow
	if ok {
		master = s.masterBySecret(fromB64url(secretB))
	}
	if master == nil {
		if !s.failLimit.allow(ip) {
			s.sendC(c, 429, subErr("rate_limited"))
			return nil
		}
		if ok {
			s.sendC(c, 404, subErr("not_found"))
		} else {
			s.sendC(c, 400, subErr("bad_request"))
		}
		return nil
	}
	s.sendC(c, 200, jsjson.Object{{Key: "key_uuid", Val: master.UUID}, {Key: "devices", Val: s.deviceCount(s.a.db, master.ID)},
		{Key: "device_limit", Val: master.DeviceLimit}})
	return nil
}

func (s *Sub) config(c *Ctx) error {
	dev := c.device
	s.a.db.Exec("UPDATE devices SET last_seen = ? WHERE id = ?", now(), dev.ID)
	// Версия клиента меняется при обновлении приложения; заголовок не подписан — лишь подпись в панели.
	if v := strings.Join(c.R.Header["X-Sen-Version"], ", "); reClientVersion.MatchString(v) && v != dev.Version {
		s.a.db.Exec("UPDATE devices SET version = ? WHERE id = ?", v, dev.ID)
	}
	cfg, err := s.readyConfig(dev, c)
	if err != nil {
		return err
	}
	s.sendC(c, 200, jsjson.Object{{Key: "config", Val: cfg}})
	return nil
}

func (s *Sub) devicesList(c *Ctx) error {
	dev := c.device
	master := s.masterByID(s.a.db, dev.MasterID)
	if master == nil {
		s.sendC(c, 404, subErr("not_found"))
		return nil
	}
	list := []any{}
	for _, d := range s.devicesOf(s.a.db, master.ID) {
		list = append(list, jsjson.Object{{Key: "id", Val: d.ID}, {Key: "name", Val: d.DeviceName}, {Key: "platform", Val: d.Platform},
			{Key: "version", Val: d.Version}, {Key: "created_at", Val: d.CreatedAt}, {Key: "last_seen", Val: nullInt(d.LastSeen)},
			{Key: "current", Val: d.ID == dev.ID}})
	}
	s.sendC(c, 200, jsjson.Object{{Key: "name", Val: master.Label}, {Key: "key_uuid", Val: master.UUID},
		{Key: "device_limit", Val: master.DeviceLimit}, {Key: "devices", Val: list}})
	return nil
}

// rekey — новый ключ ставится на все серверы ключа; если не принял ни один — ключ не меняем.
func (s *Sub) rekey(c *Ctx) error {
	dev := c.device
	pub, ok := c.Obj().Get2("pub_key").(string)
	if !ok || !reB64Pub.MatchString(pub) {
		s.sendC(c, 400, subErr("bad_request"))
		return nil
	}
	if pub == dev.PubKey {
		cfg, err := s.readyConfig(dev, c)
		if err != nil {
			return err
		}
		s.sendC(c, 200, jsjson.Object{{Key: "config", Val: cfg}})
		return nil
	}
	if _, err := s.a.db.Exec("UPDATE devices SET pub_key = ?, rekey_requested = ? WHERE id = ?", pub, 0, dev.ID); err != nil {
		s.sendC(c, 409, subErr("conflict")) // этот ключ уже у другого устройства
		return nil
	}
	var ids []int
	for _, l := range s.linksOf(s.a.db, dev.ID) {
		ids = append(ids, l.ServerID)
	}
	s.reconcileAll(ids)
	d := s.deviceByID(s.a.db, dev.ID)
	if d == nil {
		return errors.New("device vanished")
	}
	cfg := s.buildConfig(d, c)
	if configServers(cfg) == 0 {
		s.a.db.Exec("UPDATE devices SET pub_key = ?, rekey_requested = ? WHERE id = ?", dev.PubKey, dev.RekeyRequested, dev.ID)
		s.reconcileAll(ids) // вернуть старый ключ туда, где успели сменить
		s.sendC(c, 502, subErr("unavailable"))
		return nil
	}
	s.sendC(c, 200, jsjson.Object{{Key: "config", Val: cfg}})
	return nil
}

func (s *Sub) deviceDelete(c *Ctx) error {
	s.reconcileAll(s.forgetDeviceTx(c.device))
	s.sendC(c, 200, jsjson.Object{{Key: "ok", Val: true}})
	return nil
}

func (s *Sub) publicRouter() *Router {
	rt := newRouter()
	rt.bodyLimit = 4 * 1024
	// Битый JSON и т. п.: ответ всё равно подписан, чтобы клиент мог ему доверять.
	rt.badBody = func(w http.ResponseWriter, r *http.Request, _ int, _ string) {
		s.send(w, r, 400, subErr("bad_request"))
	}
	rt.notFound = func(c *Ctx) { s.sendC(c, 404, subErr("not_found")) }
	rt.Post("/sub/v1/register", s.wrapSub(s.register))
	rt.Post("/sub/v1/peek", s.wrapSub(s.peek))
	rt.Get("/sub/v1/config", s.deviceAuth, s.wrapSub(s.config))
	rt.Get("/sub/v1/devices", s.deviceAuth, s.wrapSub(s.devicesList))
	rt.Post("/sub/v1/rekey", s.deviceAuth, s.wrapSub(s.rekey))
	rt.Delete("/sub/v1/device", s.deviceAuth, s.wrapSub(s.deviceDelete))
	return rt
}

// ── Листенер и фоновое сведение ─────────────────────────────────────────────

func (s *Sub) usedServers() []int {
	return ints(s.a.db, "SELECT server_id FROM master_servers UNION SELECT server_id FROM peer_tombstones")
}

// Start — фоновое сведение, плановый аудит и публичный листенер /sub/v1.
func (s *Sub) Start() {
	// Сводим и без подписки: у ключей могут остаться хвосты на удаление.
	go func() {
		for range time.Tick(syncEvery) {
			s.reconcileAll(s.usedServers())
		}
	}()
	if s.auditOn {
		// Период читается из настроек каждый раз: смена в панели действует без рестарта.
		go func() {
			wait := auditFirst
			for {
				time.Sleep(wait)
				s.auditAll(true)
				s.auditMu.Lock()
				wait = time.Duration(s.auditSettings.EveryH) * auditHour
				s.auditMu.Unlock()
			}
		}()
	}

	if s.key() == nil {
		return
	}
	if s.port == 0 {
		warnf("sub: SUB_PORT не задан — подписка отключена")
		s.keyMu.Lock()
		s.signKey = nil
		s.keyMu.Unlock()
		return
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", s.port), Handler: s.publicRouter(), ReadHeaderTimeout: 15 * time.Second, ErrorLog: quietLog()}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		warnf("sub: листенер не запущен: %v", err)
		return
	}
	scheme := "http"
	if s.tlsCert != nil {
		scheme = "https"
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*s.tlsCert}}
	}
	logf("sub: listening on :%d (%s)", s.port, scheme)
	go func() {
		if s.tlsCert != nil {
			srv.ServeTLS(ln, "", "")
		} else {
			srv.Serve(ln)
		}
	}()
}
