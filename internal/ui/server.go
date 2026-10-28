// Package ui — awg-ui на Go (порт awg-ui/server.ts и модулей): веб-панель с JWT, прокси к
// awg-ctrl, публичный API /api/v1 по ключам, хаб нод (WSS), подписка sen:// и /metrics.
// Фронтенд тот же (React, собранный в dist/) — сервер его только раздаёт.
package ui

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
)

// ── Журнал: как console.log / console.warn у Node-версии (CLI пишет оба потока в один файл) ──

func logf(f string, a ...any)  { fmt.Fprintf(os.Stdout, f+"\n", a...) }
func warnf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

var nowFn = time.Now

func now() int64 { return nowFn().Unix() }

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envNum — Number(process.env.X) || 0.
func envNum(key string) int {
	f := jsToNumber(os.Getenv(key))
	if f != f || f == 0 || f != float64(int(f)) {
		return 0
	}
	return int(f)
}

// envFromFile — значение KEY=… из .env (первая строка, обрезанная).
func envFromFile(file, key string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(.*)$`).FindSubmatch(raw)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// ── Клиент awg-ctrl ─────────────────────────────────────────────────────────

type CtrlResp struct {
	Status int
	Data   any // разобранный JSON, иначе строка (как axios)
}

// Ctrl — запрос к awg-ctrl сервера: локальному по HTTP или к ноде по WSS. Ошибка — только
// когда ответа нет вовсе (сеть, нет ключа); статусы ≥ 400 приходят как есть.
type Ctrl func(method, path string, body any) (*CtrlResp, error)

// decodeLikeAxios — тело ответа так, как его отдал бы axios: JSON — значением, иначе строкой.
func decodeLikeAxios(raw []byte) any {
	if len(raw) == 0 {
		return ""
	}
	if v, err := jsjson.Decode(raw); err == nil {
		return v
	}
	return string(raw)
}

type App struct {
	port      int
	uiUser    string
	uiPass    string
	jwtSecret []byte
	brand     string
	channel   string
	version   string
	ctrlURL   string
	baseDir   string

	internalKey ed25519.PrivateKey
	httpLocal   *http.Client

	db *sql.DB

	mu            sync.Mutex
	revoked       map[string]bool
	loginAttempts map[string]*rateEntry

	nodes     *Nodes
	sub       *Sub
	metrics   *Metrics
	monitor   *Monitor
	migration *Migration
}

// New готовит панель: база, ключи, модули. Каталог панели — рабочий каталог процесса
// (CLI запускает сервис с cwd = <root>/awg-ui): там dist/, panel.id и ключи по умолчанию.
func New() (*App, error) {
	cwd, _ := os.Getwd()
	rootEnv := filepath.Join(cwd, "..", ".env")
	pick := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v := envFromFile(rootEnv, key); v != "" {
			return v
		}
		return def
	}
	a := &App{
		port:    envNum("PORT"),
		uiUser:  envOr("UI_USER", "admin"),
		uiPass:  os.Getenv("UI_PASS"),
		brand:   pick("BRAND", "Forgetting"),
		channel: pick("CHANNEL", "Beta"),
		version: pick("VERSION", ""),
		ctrlURL: "http://127.0.0.1:" + envOr("AWGCTRL_PORT", "3005"),
		baseDir: cwd,
		// awg-ctrl на loopback: системный прокси сюда не нужен.
		httpLocal:     &http.Client{Transport: &http.Transport{Proxy: nil}},
		revoked:       map[string]bool{},
		loginAttempts: map[string]*rateEntry{},
	}
	if a.port == 0 {
		a.port = 8080
	}
	// Как `process.env.JWT_SECRET ?? random` в Node: заданный (даже пустой) секрет берётся как есть.
	if s, ok := os.LookupEnv("JWT_SECRET"); ok {
		a.jwtSecret = []byte(s)
	} else {
		b := make([]byte, 32)
		rand.Read(b)
		a.jwtSecret = []byte(hex.EncodeToString(b))
	}

	keyFile := envOr("INTERNAL_AUTH_KEY_FILE", filepath.Join(cwd, "internal_auth_private.key"))
	if k, err := iauth.LoadPrivate(keyFile); err == nil {
		a.internalKey = k
	} else {
		warnf("ui: приватный ключ внутренней авторизации не найден: %s — запросы к awg-ctrl будут отклоняться", keyFile)
	}

	dbFile := os.Getenv("UI_DB_FILE")
	if dbFile == "" {
		dbFile = filepath.Join(cwd, "ui.db")
	}
	db, err := openUIDB(dbFile)
	if err != nil {
		return nil, err
	}
	a.db = db
	if err := a.initAPIKeys(); err != nil {
		return nil, err
	}

	if a.nodes, err = newNodes(a); err != nil {
		return nil, err
	}
	if a.sub, err = newSub(a); err != nil {
		return nil, err
	}
	if a.metrics, err = newMetrics(a); err != nil {
		return nil, err
	}
	a.monitor = newMonitor(a)
	a.migration = newMigration(a)
	a.nodes.BeforeDelete(a.apiKeysBeforeDelete)
	a.nodes.AfterDelete(a.apiKeysAfterDelete)
	return a, nil
}

func openUIDB(path string) (*sql.DB, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Одно соединение: запросы и транзакции идут по очереди, как в однопоточном Node.
	// Внутри tx() нельзя обращаться к a.db — только к переданному tx, иначе взаимоблокировка.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// tx — uidb.transaction(fn)(): всё или ничего.
func (a *App) tx(fn func(tx *sql.Tx) error) error {
	t, err := a.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

// ctrlLocal — запрос к awg-ctrl этой машины с внутренним токеном.
func (a *App) ctrlLocal(method, path string, body any) (*CtrlResp, error) {
	if a.internalKey == nil {
		return nil, fmt.Errorf("приватный ключ внутренней авторизации не загружен")
	}
	var rd *strings.Reader
	if body != nil {
		b, err := jsjson.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = strings.NewReader(string(b))
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequest(method, a.ctrlURL+path, rd)
	} else {
		req, err = http.NewRequest(method, a.ctrlURL+path, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+iauth.Mint(a.internalKey, "awg-ui", nowFn()))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpLocal.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		return nil, err
	}
	return &CtrlResp{resp.StatusCode, decodeLikeAxios(raw)}, nil
}

// ── JWT панели (HS256) ──────────────────────────────────────────────────────

func tokenSig(token string) string {
	if p := strings.Split(token, "."); len(p) > 2 {
		return p[2]
	}
	return token
}

func (a *App) sign() string {
	h := b64url.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	p := b64url.EncodeToString(jsjson.MustMarshal(jsjson.Object{{Key: "exp", Val: now() + 86400}}))
	m := hmac.New(sha256.New, a.jwtSecret)
	m.Write([]byte(h + "." + p))
	return h + "." + p + "." + b64url.EncodeToString(m.Sum(nil))
}

func (a *App) verify(token string) bool {
	a.mu.Lock()
	rev := a.revoked[tokenSig(token)]
	a.mu.Unlock()
	if rev {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	m := hmac.New(sha256.New, a.jwtSecret)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(fromB64url(parts[2]), m.Sum(nil)) {
		return false
	}
	v, err := jsjson.Decode(fromB64url(parts[1]))
	if err != nil {
		return false
	}
	exp, ok := field(v, "exp").(float64)
	return ok && exp > float64(now())
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	return ""
}

func (a *App) requireAuth(c *Ctx) bool {
	tok := bearer(c.R)
	if tok == "" || !a.verify(tok) {
		return c.Error(401, "Unauthorized")
	}
	return true
}

// ── Лимит входа ─────────────────────────────────────────────────────────────

const (
	rateLimit  = 5
	rateWindow = 15 * time.Minute
)

type rateEntry struct {
	count   int
	resetAt time.Time
}

func (a *App) checkRate(ip string) (bool, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := nowFn()
	e := a.loginAttempts[ip]
	if e == nil || e.resetAt.Before(t) {
		a.loginAttempts[ip] = &rateEntry{1, t.Add(rateWindow)}
		return true, 0
	}
	if e.count >= rateLimit {
		ms := e.resetAt.Sub(t).Milliseconds()
		return false, int((ms + 999) / 1000)
	}
	e.count++
	return true, 0
}

// ── Ключи внешнего API (/api/v1) ────────────────────────────────────────────
// Живут в собственной БД awg-ui: awg-ctrl про них ничего не знает. Открытое значение
// ключа не хранится — только SHA-256 и префикс для показа в панели.

type apiKeyRow struct {
	ID        int64
	Label     string
	KeyHash   string
	Prefix    string
	ServerID  int
	CreatedAt int64
	LastUsed  sql.NullInt64
}

func (a *App) initAPIKeys() error {
	_, err := a.db.Exec(`
    CREATE TABLE IF NOT EXISTS api_keys (
        id         INTEGER PRIMARY KEY AUTOINCREMENT,
        label      TEXT    NOT NULL,
        key_hash   TEXT    NOT NULL UNIQUE,
        prefix     TEXT    NOT NULL,
        server_id  INTEGER NOT NULL DEFAULT 0,
        created_at INTEGER NOT NULL,
        last_used  INTEGER
    );
    -- Какие серверы видит ключ: на них он заводит vpn://-пользователей и в их пределах
    -- управляет мастер-ключами. api_keys.server_id — сервер по умолчанию для /api/v1/users,
    -- всегда входит в набор.
    CREATE TABLE IF NOT EXISTS api_key_servers (
        key_id    INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
        server_id INTEGER NOT NULL,
        PRIMARY KEY (key_id, server_id)
    );
    -- Ключи со времён одного сервера: набор — их server_id.
    INSERT OR IGNORE INTO api_key_servers (key_id, server_id)
        SELECT id, server_id FROM api_keys WHERE id NOT IN (SELECT key_id FROM api_key_servers);
`)
	return err
}

func scanKey(row interface{ Scan(...any) error }) (*apiKeyRow, error) {
	k := &apiKeyRow{}
	if err := row.Scan(&k.ID, &k.Label, &k.KeyHash, &k.Prefix, &k.ServerID, &k.CreatedAt, &k.LastUsed); err != nil {
		return nil, err
	}
	return k, nil
}

const keyCols = "id, label, key_hash, prefix, server_id, created_at, last_used"

func (a *App) keyByID(q queryer, id int64) *apiKeyRow {
	k, _ := scanKey(q.QueryRow("SELECT "+keyCols+" FROM api_keys WHERE id = ?", id))
	return k
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// ints — первая колонка всех строк.
func ints(q queryer, query string, args ...any) []int {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) keyServers(q queryer, id int64) []int {
	return ints(q, "SELECT server_id FROM api_key_servers WHERE key_id = ? ORDER BY server_id", id)
}

// setKeyServers — набор серверов ключа целиком. Сервер по умолчанию остаётся, если он в
// наборе, иначе — наименьший из набора.
func (a *App) setKeyServers(id int64, ids []int) error {
	row := a.keyByID(a.db, id)
	if row == nil {
		return nil
	}
	return a.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM api_key_servers WHERE key_id = ?", id); err != nil {
			return err
		}
		for _, sid := range ids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO api_key_servers (key_id, server_id) VALUES (?, ?)", id, sid); err != nil {
				return err
			}
		}
		if !containsInt(ids, row.ServerID) {
			_, err := tx.Exec("UPDATE api_keys SET server_id = ? WHERE id = ?", ids[0], id)
			return err
		}
		return nil
	})
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// uniqueInts — [...new Set(v)] для массива из JSON: все элементы — целые серверы из реестра.
// Иначе — ok=false.
func (a *App) serverList(v any, exists func(int) bool) ([]int, bool) {
	list := arr(v)
	seen := map[float64]bool{}
	var ids []int
	for _, x := range list {
		f, isNum := x.(float64)
		if !isNum || !isInt(x) || !exists(int(f)) {
			return nil, false
		}
		if !seen[f] {
			seen[f] = true
			ids = append(ids, int(f))
		}
	}
	sort.Ints(ids)
	return ids, true
}

// parseKeyServers — набор серверов из тела: непустой, без повторов, только существующие.
func (a *App) parseKeyServers(v any) ([]int, string) {
	if l, ok := v.([]any); !ok || len(l) == 0 {
		return nil, "Нужен хотя бы один сервер"
	}
	ids, ok := a.serverList(v, a.nodes.Exists)
	if !ok {
		return nil, "Такого сервера нет"
	}
	return ids, ""
}

func hashKey(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

func genAPIKey() (key, hash, prefix string) {
	b := make([]byte, 24)
	rand.Read(b)
	key = "awgk_" + b64url.EncodeToString(b)
	return key, hashKey(key), key[:13] + "…"
}

var reKeyLabel = regexp.MustCompile(`^[\w \-]{1,40}$`)

func intsJSON(s []int) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func nullInt(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func (a *App) apiKeysList(c *Ctx) bool {
	rows, err := a.db.Query("SELECT " + keyCols + " FROM api_keys ORDER BY id")
	if err != nil {
		return c.Error(500, "Internal server error")
	}
	var keys []*apiKeyRow
	for rows.Next() {
		if k, err := scanKey(rows); err == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()
	out := []any{}
	for _, k := range keys {
		out = append(out, jsjson.Object{
			{Key: "id", Val: k.ID}, {Key: "label", Val: k.Label}, {Key: "prefix", Val: k.Prefix},
			{Key: "server_id", Val: k.ServerID}, {Key: "created_at", Val: k.CreatedAt}, {Key: "last_used", Val: nullInt(k.LastUsed)},
			{Key: "servers", Val: intsJSON(a.keyServers(a.db, k.ID))},
		})
	}
	return c.JSON(200, jsjson.Object{{Key: "keys", Val: out}})
}

// jsString — String(v) для меток: строка, число или логическое значение. Остальное — не метка.
func jsString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return jsjson.Number(x), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// servers — набор серверов ключа; без него — server_id (по-старому) или сервер по умолчанию.
func (a *App) apiKeysCreate(c *Ctx) bool {
	b := c.Obj()
	label, labelOK := jsString(b.Get2("label"))
	if !truthy(b.Get2("label")) || !labelOK || !reKeyLabel.MatchString(label) {
		return c.Error(400, "Метка: буквы, цифры, пробел, _ и -, до 40 символов")
	}
	// server_id === undefined в JSON — только отсутствие ключа; null — это Number(null) = 0.
	sidRaw, sidPresent := b.Get("server_id")
	var serversV any
	if s := b.Get2("servers"); s != nil {
		serversV = s
	} else if !sidPresent {
		serversV = []any{float64(a.nodes.DefaultID())}
	} else {
		serversV = []any{jsToNumberAny(sidRaw)}
	}
	ids, msg := a.parseKeyServers(serversV)
	if msg != "" {
		return c.Error(400, msg)
	}
	sid := ids[0]
	if sidPresent {
		if f := jsToNumberAny(sidRaw); f == float64(int(f)) && containsInt(ids, int(f)) {
			sid = int(f)
		}
	}
	key, hash, prefix := genAPIKey()
	var id int64
	err := a.tx(func(tx *sql.Tx) error {
		r, err := tx.Exec("INSERT INTO api_keys (label, key_hash, prefix, server_id, created_at) VALUES (?, ?, ?, ?, ?)",
			label, hash, prefix, sid, now())
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		for _, s := range ids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO api_key_servers (key_id, server_id) VALUES (?, ?)", id, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return c.Error(500, "Internal server error")
	}
	// Открытое значение отдаётся один раз — дальше в БД только хэш.
	return c.JSON(201, jsjson.Object{{Key: "id", Val: id}, {Key: "label", Val: b.Get2("label")}, {Key: "prefix", Val: prefix},
		{Key: "key", Val: key}, {Key: "server_id", Val: sid}, {Key: "servers", Val: intsJSON(ids)}})
}

// paramID — Number(req.params.id): ok, если целое.
func paramID(s string) (int64, bool) {
	f := jsToNumber(s)
	if f != f || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

func (a *App) apiKeysPatch(c *Ctx) bool {
	id, ok := paramID(c.Params["id"])
	if !ok || a.keyByID(a.db, id) == nil {
		return c.Error(404, "Не найден")
	}
	ids, msg := a.parseKeyServers(c.Obj().Get2("servers"))
	if msg != "" {
		return c.Error(400, msg)
	}
	if err := a.setKeyServers(id, ids); err != nil {
		return c.Error(500, "Internal server error")
	}
	row := a.keyByID(a.db, id)
	return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "server_id", Val: row.ServerID}, {Key: "servers", Val: intsJSON(ids)}})
}

func (a *App) apiKeysDelete(c *Ctx) bool {
	id, ok := paramID(c.Params["id"])
	if !ok {
		return c.Error(400, "Неверный id")
	}
	a.tx(func(tx *sql.Tx) error {
		tx.Exec("DELETE FROM api_key_servers WHERE key_id = ?", id)
		_, err := tx.Exec("DELETE FROM api_keys WHERE id = ?", id)
		return err
	})
	return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "id", Val: id}})
}

// Сервер удаляют из реестра: ключ API, для которого он единственный, — повод отказать
// (ключ остался бы без серверов); у остальных он убирается из набора, когда сервер удалён.
type keyOnServer struct {
	id       int64
	label    string
	serverID int
	n        int
}

func (a *App) keysOnServer(id int) []keyOnServer {
	rows, err := a.db.Query("SELECT k.id, k.label, k.server_id, (SELECT COUNT(*) FROM api_key_servers x WHERE x.key_id = k.id) AS n FROM api_keys k JOIN api_key_servers s ON s.key_id = k.id WHERE s.server_id = ?", id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []keyOnServer
	for rows.Next() {
		var k keyOnServer
		if rows.Scan(&k.id, &k.label, &k.serverID, &k.n) == nil {
			out = append(out, k)
		}
	}
	return out
}

func (a *App) apiKeysBeforeDelete(id int, _ bool) *deleteVeto {
	var only []string
	for _, k := range a.keysOnServer(id) {
		if k.n == 1 {
			only = append(only, k.label)
		}
	}
	if len(only) > 0 {
		return &deleteVeto{Error: "Это единственный сервер ключей API: " + strings.Join(only, ", ") + " — сначала удалите их или добавьте им другой сервер"}
	}
	return nil
}

func (a *App) apiKeysAfterDelete(id int) {
	for _, k := range a.keysOnServer(id) {
		var rest []int
		for _, s := range a.keyServers(a.db, k.id) {
			if s != id {
				rest = append(rest, s)
			}
		}
		a.setKeyServers(k.id, rest)
	}
}

// ── Внешний API: ключ и сервер ──────────────────────────────────────────────

func (a *App) requireAPIKey(c *Ctx) bool {
	key := c.R.Header.Get("X-Api-Key")
	if !strings.HasPrefix(key, "awgk_") {
		return c.Error(401, "API key required")
	}
	row, err := scanKey(a.db.QueryRow("SELECT "+keyCols+" FROM api_keys WHERE key_hash = ?", hashKey(key)))
	if err != nil {
		return c.Error(401, "Invalid API key")
	}
	a.db.Exec("UPDATE api_keys SET last_used = ? WHERE id = ?", now(), row.ID)
	c.apiKey = row
	c.servers = a.keyServers(a.db, row.ID)
	return true
}

// pickKeyServer — сервер для /api/v1/users: X-Server-Id из набора ключа, без заголовка —
// сервер ключа по умолчанию. Сервер вне набора — как несуществующий.
func (a *App) pickKeyServer(c *Ctx) bool {
	id := c.apiKey.ServerID
	if raw, ok := c.R.Header["X-Server-Id"]; ok {
		f := jsToNumber(strings.Join(raw, ", "))
		if f != float64(int(f)) {
			return c.Error(404, "Сервер не найден")
		}
		id = int(f)
	}
	if !containsInt(c.servers, id) {
		return c.Error(404, "Сервер не найден")
	}
	c.serverID = id
	return true
}

// serverIDOf — сервер запроса панели: X-Server-Id (0 — локальный awg-ctrl), без него — по умолчанию.
func (a *App) serverIDOf(c *Ctx) int {
	raw, ok := c.R.Header["X-Server-Id"]
	if !ok {
		return a.nodes.DefaultID()
	}
	f := jsToNumber(strings.Join(raw, ", "))
	if f == float64(int(f)) && f >= 0 {
		return int(f)
	}
	return a.nodes.DefaultID()
}

func (a *App) proxy(c *Ctx) bool {
	r, err := a.nodes.CtrlFor(a.serverIDOf(c))(c.R.Method, c.R.RequestURI, c.Body)
	if err != nil {
		return c.Error(502, "awg-ctrl недоступен")
	}
	return c.JSON(r.Status, r.Data)
}

// extUser — внешний контракт: наружу только name/ip/gen/vpn_key, psk_key и pub_key остаются внутри.
func extUser(d any) jsjson.Object {
	o := jsjson.Object{}
	for _, k := range [][2]string{{"name", "name"}, {"ip", "ip"}, {"gen", "key_gen"}, {"vpn_key", "vpn_key"}} {
		if hasField(d, k[1]) { // undefined JSON.stringify опускает
			o = append(o, jsjson.KV{Key: k[0], Val: field(d, k[1])})
		}
	}
	return o
}

func hasField(v any, k string) bool {
	o, ok := v.(jsjson.Object)
	return ok && has(o, k)
}

func (a *App) extCtrl(c *Ctx) Ctrl { return a.nodes.CtrlFor(c.serverID) }

// unavailable — ответ, когда awg-ctrl не ответил (как wrap() в Node-версии).
func unavailable(c *Ctx) bool { return c.Error(502, "awg-ctrl недоступен") }

func (a *App) extUsersCreate(c *Ctx) bool {
	body := jsjson.Object{}
	if v, ok := c.Obj().Get("name"); ok {
		body = append(body, jsjson.KV{Key: "name", Val: v})
	}
	r, err := a.extCtrl(c)("POST", "/api/users", body)
	if err != nil {
		return unavailable(c)
	}
	if r.Status >= 400 {
		return c.JSON(r.Status, r.Data)
	}
	return c.JSON(201, extUser(r.Data))
}

func (a *App) extUsersList(c *Ctx) bool {
	r, err := a.extCtrl(c)("GET", "/api/users/stats", nil)
	if err != nil {
		return unavailable(c)
	}
	return c.JSON(r.Status, r.Data)
}

func (a *App) extUsersGet(c *Ctx) bool {
	r, err := a.extCtrl(c)("POST", "/api/users/"+encodeURIComponent(c.Params["name"]), nil)
	if err != nil {
		return unavailable(c)
	}
	if r.Status >= 400 {
		return c.JSON(r.Status, r.Data)
	}
	return c.JSON(200, extUser(r.Data))
}

func (a *App) extUsersDelete(c *Ctx) bool {
	r, err := a.extCtrl(c)("DELETE", "/api/users/"+encodeURIComponent(c.Params["name"]), nil)
	if err != nil {
		return unavailable(c)
	}
	return c.JSON(r.Status, r.Data)
}

// ── Маршруты ────────────────────────────────────────────────────────────────

func (a *App) login(c *Ctx) bool {
	ip := c.remoteIP()
	if ok, retry := a.checkRate(ip); !ok {
		return c.Error(429, fmt.Sprintf("Слишком много попыток. Повтори через %d сек.", retry))
	}
	b := c.Obj()
	user, uok := b.Get2("user").(string)
	pass, pok := b.Get2("pass").(string)
	if a.uiPass == "" || !uok || !pok || user != a.uiUser || pass != a.uiPass {
		return c.Error(401, "Неверный логин или пароль")
	}
	a.mu.Lock()
	delete(a.loginAttempts, ip)
	a.mu.Unlock()
	return c.JSON(200, jsjson.Object{{Key: "token", Val: a.sign()}})
}

func (a *App) logout(c *Ctx) bool {
	a.mu.Lock()
	a.revoked[tokenSig(bearer(c.R))] = true
	a.mu.Unlock()
	return c.JSON(200, jsjson.Object{{Key: "ok", Val: true}})
}

func (a *App) routes() *Router {
	app := newRouter()
	auth := a.requireAuth

	app.Post("/login", a.login)
	app.Post("/logout", auth, a.logout)
	// За авторизацией: без неё сканер одним GET узнавал бы, что на порту панель Forgetting.
	// Экран логина обходится без вордмарка.
	app.Get("/ui/brand", auth, func(c *Ctx) bool {
		return c.JSON(200, jsjson.Object{{Key: "brand", Val: a.brand}, {Key: "channel", Val: a.channel}, {Key: "version", Val: a.version}})
	})
	app.Get("/ui/apikeys", auth, a.apiKeysList)
	app.Post("/ui/apikeys", auth, a.apiKeysCreate)
	app.Patch("/ui/apikeys/:id", auth, a.apiKeysPatch)
	app.Delete("/ui/apikeys/:id", auth, a.apiKeysDelete)

	// ВАЖНО: /api/v1 — до JWT-прокси на /api, иначе прокси потребует JWT вместо ключа.
	app.Use("/api/v1", a.requireAPIKey)
	app.Use("/api/v1/users", a.pickKeyServer)
	app.Post("/api/v1/users", a.extUsersCreate)
	app.Get("/api/v1/users", a.extUsersList)
	app.Get("/api/v1/users/:name", a.extUsersGet)
	app.Delete("/api/v1/users/:name", a.extUsersDelete)
	app.Mount("/api/v1/masterkeys", a.sub.apiRouter(func(c *Ctx) []int { return c.servers }))

	app.Use("/ui/nodes", auth)
	app.Mount("/ui/nodes", a.nodes.router())
	app.Use("/ui/masterkeys", auth)
	app.Mount("/ui/masterkeys", a.sub.masterKeysRouter())
	app.Use("/ui/devices", auth)
	app.Mount("/ui/devices", a.sub.devicesRouter())

	// Prometheus: /metrics за отдельным токеном «только метрики» (не JWT и не ключ API).
	app.Use("/ui/metrics-token", auth)
	app.Mount("/ui/metrics-token", a.metrics.router())
	app.Get("/metrics", a.metrics.requireToken, a.metrics.handler)
	app.Use("/ui/monitor", auth)
	app.Mount("/ui/monitor", a.monitor.router())
	// Перенос панели: приём архива — без JWT, его пускает только ключ из кода переноса.
	app.Post("/migrate/v1/import", a.migration.importHandler)
	app.Use("/ui/migrate", auth)
	app.Mount("/ui/migrate", a.migration.router())

	for _, p := range []string{"/api", "/health", "/awg"} {
		app.Use(p, auth, a.proxy)
	}
	app.Use("/", a.static)
	return app
}

// static — express.static(dist) и SPA-фолбэк app.get("*") → dist/index.html.
func (a *App) static(c *Ctx) bool {
	if c.R.Method != "GET" && c.R.Method != "HEAD" {
		return true
	}
	dist := filepath.Join(a.baseDir, "dist")
	p, err := url.PathUnescape(c.R.URL.EscapedPath())
	if err == nil && !strings.Contains(p, "\x00") {
		clean := filepath.Clean("/" + p)
		hidden := false
		for _, s := range strings.Split(clean, "/") {
			if strings.HasPrefix(s, ".") {
				hidden = true
			}
		}
		f := filepath.Join(dist, clean)
		if st, err := os.Stat(f); err == nil && !hidden {
			if st.IsDir() {
				if !strings.HasSuffix(c.R.URL.Path, "/") {
					// Относительный адрес: за прокси в подпапке абсолютный увёл бы из неё.
					c.W.Header().Set("Location", path.Base(c.R.URL.Path)+"/")
					c.W.WriteHeader(http.StatusMovedPermanently)
					return false
				}
				f = filepath.Join(f, "index.html")
			}
			if st, err := os.Stat(f); err == nil && !st.IsDir() {
				serveFile(c.W, c.R, f)
				return false
			}
		}
	}
	index := filepath.Join(dist, "index.html")
	if _, err := os.Stat(index); err != nil {
		writeHTMLError(c.W, 404, "Error: ENOENT: no such file or directory, stat '"+index+"'")
		return false
	}
	serveFile(c.W, c.R, index)
	return false
}

func serveFile(w http.ResponseWriter, r *http.Request, f string) {
	fh, err := os.Open(f)
	if err != nil {
		writeHTMLError(w, 404, "Not Found")
		return
	}
	defer fh.Close()
	st, _ := fh.Stat()
	w.Header().Set("Cache-Control", "public, max-age=0")
	http.ServeContent(w, r, st.Name(), st.ModTime(), fh)
}

// Run слушает панель на :PORT и запускает фоновые части (хаб нод, подписку).
func (a *App) Run() error {
	ln, err := listen(a.port)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 30 * time.Second}
	logf("ui: listening on :%d", a.port)
	a.nodes.Start()
	a.sub.Start()
	return srv.Serve(ln)
}
