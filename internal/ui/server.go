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
	prefixes  []string // UI_PREFIX, UI_PREFIX_MOVED: секретные пути панели (prefix.go)
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
	httpSlow    *http.Client // /awg/*: рестарт и upgrade идут долго, общий таймаут им не годится

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
		port:     envNum("PORT"),
		prefixes: uiPrefixes(),
		uiUser:   envOr("UI_USER", "admin"),
		uiPass:   os.Getenv("UI_PASS"),
		brand:    pick("BRAND", "Forgetting"),
		channel:  pick("CHANNEL", "Beta"),
		version:  pick("VERSION", ""),
		ctrlURL:  "http://127.0.0.1:" + envOr("AWGCTRL_PORT", "3005"),
		baseDir:  cwd,
		// awg-ctrl на loopback: системный прокси сюда не нужен. У быстрых запросов таймаут
		// обязателен: мёртвый локальный awg-ctrl не должен держать ни /ui/nodes, ни прокси.
		httpLocal: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
		// /awg/* и mtg идут долго (рестарт, upgrade) — им отдельный клиент, иначе 5 с в обрез.
		httpSlow:      &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: rpcSlow},
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
	client := a.httpLocal
	// Всё, что меняет состояние, ходит в awg/awg-quick и может идти долго (reissue, sync,
	// рестарт): короткий таймаут оставлен только чтениям — именно они не должны держать панель.
	slow := method != "GET" || strings.HasPrefix(path, "/awg/") || strings.HasPrefix(path, "/api/mtproto/mtg")
	if a.httpSlow != nil && slow {
		client = a.httpSlow
	}
	resp, err := client.Do(req)
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
	ID         int64
	Label      string
	KeyHash    string
	Prefix     string
	ServerID   int
	CreatedAt  int64
	LastUsed   sql.NullInt64
	AllServers bool // ключ видит весь реестр серверов, включая добавленные после создания
}

func (a *App) initAPIKeys() error {
	if _, err := a.db.Exec(`
    CREATE TABLE IF NOT EXISTS api_keys (
        id          INTEGER PRIMARY KEY AUTOINCREMENT,
        label       TEXT    NOT NULL,
        key_hash    TEXT    NOT NULL UNIQUE,
        prefix      TEXT    NOT NULL,
        server_id   INTEGER NOT NULL DEFAULT 0,
        created_at  INTEGER NOT NULL,
        last_used   INTEGER,
        all_servers INTEGER NOT NULL DEFAULT 1
    );
    -- Какие серверы видит ключ: на них он заводит vpn://-пользователей и в их пределах
    -- управляет мастер-ключами. api_keys.server_id — сервер по умолчанию для /api/v1/users,
    -- всегда входит в набор. При all_servers = 1 строк здесь нет: набор — весь реестр на
    -- момент запроса (серверы, добавленные позже, ключ видит сам).
    CREATE TABLE IF NOT EXISTS api_key_servers (
        key_id    INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
        server_id INTEGER NOT NULL,
        PRIMARY KEY (key_id, server_id)
    );
`); err != nil {
		return err
	}
	// all_servers — «ключ видит все серверы»: в api_key_servers при этом пусто. Новые ключи
	// создаются так (DEFAULT 1): ограничение набора — осознанное действие в панели.
	if !hasColumn(a.db, "api_keys", "all_servers") {
		if _, err := a.db.Exec("ALTER TABLE api_keys ADD COLUMN all_servers INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
		// Миграция: все существующие ключи остаются явными (0). Доказать намерение «видеть
		// всё» у старого ключа нельзя — до этой колонки набор всегда задавался строками
		// (initAPIKeys при каждом старте достраивал ключу строку с его server_id), так что
		// строка ниже возвращает такой ключ ровно в прежнее состояние. «Все серверы» —
		// политика только для ключей, созданных после обновления.
		if _, err := a.db.Exec("UPDATE api_keys SET all_servers = 0"); err != nil {
			return err
		}
	}
	// Ключи со времён одного сервера: набор — их server_id. Только явные (all_servers = 0):
	// «всем» строки не нужны, иначе нарушился бы инвариант «sentinel или строки, но не оба».
	_, err := a.db.Exec(`
    INSERT OR IGNORE INTO api_key_servers (key_id, server_id)
        SELECT id, server_id FROM api_keys
        WHERE all_servers = 0 AND id NOT IN (SELECT key_id FROM api_key_servers);
`)
	return err
}

func scanKey(row interface{ Scan(...any) error }) (*apiKeyRow, error) {
	k := &apiKeyRow{}
	if err := row.Scan(&k.ID, &k.Label, &k.KeyHash, &k.Prefix, &k.ServerID, &k.CreatedAt, &k.LastUsed, &k.AllServers); err != nil {
		return nil, err
	}
	return k, nil
}

const keyCols = "id, label, key_hash, prefix, server_id, created_at, last_used, all_servers"

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

// allServerIDs — весь реестр серверов на сейчас (релеи не серверы). Не nil: пустая область
// ключа — это пустой набор, а не nil («панель видит всё»).
func (a *App) allServerIDs() []int {
	ids := []int{}
	if a.nodes != nil {
		if v := a.nodes.IDs(); v != nil {
			ids = v
		}
	}
	return ids
}

// keyScope — серверы ключа на момент запроса. all_servers — весь реестр (и серверы,
// добавленные после создания ключа), иначе явный набор из api_key_servers.
func (a *App) keyScope(k *apiKeyRow) []int {
	if k == nil {
		return []int{}
	}
	if k.AllServers {
		return a.allServerIDs()
	}
	return a.keyServers(a.db, k.ID)
}

// setKeyScope — набор серверов ключа целиком: all — «все серверы» (строк нет), иначе явный
// список. Sentinel и строки вместе не живут: переключение в одном tx чистит/заменяет оба.
// Сервер по умолчанию остаётся, если он в наборе (у «всех» — существует), иначе — наименьший.
func (a *App) setKeyScope(id int64, all bool, ids []int) error {
	row := a.keyByID(a.db, id)
	if row == nil {
		return nil
	}
	sid := row.ServerID
	if all {
		if a.nodes == nil || !a.nodes.Exists(sid) {
			if a.nodes != nil {
				sid = a.nodes.DefaultID()
			}
		}
	} else if len(ids) > 0 && !containsInt(ids, sid) {
		sid = ids[0]
	}
	return a.tx(func(tx *sql.Tx) error {
		flag := 0
		if all {
			flag = 1
		}
		if _, err := tx.Exec("UPDATE api_keys SET all_servers = ?, server_id = ? WHERE id = ?", flag, sid, id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM api_key_servers WHERE key_id = ?", id); err != nil {
			return err
		}
		if all {
			return nil
		}
		for _, s := range ids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO api_key_servers (key_id, server_id) VALUES (?, ?)", id, s); err != nil {
				return err
			}
		}
		return nil
	})
}

// setKeyServers — явный набор (старый контракт: панель, уборка после удаления сервера).
func (a *App) setKeyServers(id int64, ids []int) error {
	return a.setKeyScope(id, false, ids)
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

// keySet — набор серверов ключа из тела панели:
//   - all:true, servers отсутствует или servers:null — «все серверы» (sentinel all_servers);
//   - servers:[] — ошибка: ключ без серверов не нужен;
//   - servers:[…] — явный набор: только существующие, без повторов, по возрастанию.
//
// given — в теле было хоть одно из полей (PATCH с пустым телом не должен расширять ключ).
func (a *App) keySet(b jsjson.Object) (all bool, ids []int, given bool, msg string) {
	av, hasAll := b.Get("all")
	sv, hasServers := b.Get("servers")
	given = hasAll || hasServers
	if hasAll && truthy(av) {
		return true, nil, given, ""
	}
	if !hasServers || sv == nil {
		return true, nil, given, ""
	}
	if _, ok := sv.([]any); !ok {
		return false, nil, given, "servers: нужен массив серверов"
	}
	if len(arr(sv)) == 0 {
		return false, nil, given, "servers: пустой набор недопустим"
	}
	ids, ok := a.serverList(sv, a.nodes.Exists)
	if !ok {
		return false, nil, given, "servers: такого сервера нет"
	}
	return false, ids, given, ""
}

// keyDefaultID — сервер по умолчанию из тела: defaultServerId (или старый server_id), только
// существующий в реестре; иначе def.
func (a *App) keyDefaultID(b jsjson.Object, def int) int {
	for _, key := range []string{"defaultServerId", "server_id"} {
		v, ok := b.Get(key)
		if !ok || v == nil {
			continue
		}
		if f := jsToNumberAny(v); f == float64(int(f)) && a.nodes.Exists(int(f)) {
			return int(f)
		}
	}
	return def
}

// keyServersJSON — набор в ответе панели: null у «всех» (набор не перечислен), иначе список.
func keyServersJSON(all bool, ids []int) any {
	if all {
		return nil
	}
	return intsJSON(ids)
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
		var srv any
		if !k.AllServers {
			srv = intsJSON(a.keyServers(a.db, k.ID))
		}
		out = append(out, jsjson.Object{
			{Key: "id", Val: k.ID}, {Key: "label", Val: k.Label}, {Key: "name", Val: k.Label}, {Key: "prefix", Val: k.Prefix},
			{Key: "allServers", Val: k.AllServers}, {Key: "servers", Val: srv},
			{Key: "defaultServerId", Val: k.ServerID},
			{Key: "createdAt", Val: k.CreatedAt}, {Key: "lastUsed", Val: nullInt(k.LastUsed)},
			// Старые имена: текущая панель читает их, пока не перешла на контракт выше.
			{Key: "server_id", Val: k.ServerID}, {Key: "created_at", Val: k.CreatedAt},
			{Key: "last_used", Val: nullInt(k.LastUsed)},
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

// Новый ключ: {label|name, servers?, all?, defaultServerId?}. Без servers/all — «все серверы»
// (решение: ограничение набора — отдельное действие в панели). servers:[] — 400.
func (a *App) apiKeysCreate(c *Ctx) bool {
	b := c.Obj()
	label, labelOK := jsString(b.Get2("label"))
	if !truthy(b.Get2("label")) || !labelOK || !reKeyLabel.MatchString(label) {
		// name — синоним label в контракте панели.
		label, labelOK = jsString(b.Get2("name"))
		if !truthy(b.Get2("name")) || !labelOK || !reKeyLabel.MatchString(label) {
			return c.Error(400, "Метка: буквы, цифры, пробел, _ и -, до 40 символов")
		}
	}
	all, ids, _, msg := a.keySet(b)
	if msg != "" {
		return c.Error(400, msg)
	}
	sid := a.keyDefaultID(b, a.nodes.DefaultID())
	if !all {
		if !containsInt(ids, sid) {
			sid = ids[0]
		}
	} else if !a.nodes.Exists(sid) {
		sid = a.nodes.DefaultID()
	}
	key, hash, prefix := genAPIKey()
	var id int64
	err := a.tx(func(tx *sql.Tx) error {
		r, err := tx.Exec("INSERT INTO api_keys (label, key_hash, prefix, server_id, created_at, all_servers) VALUES (?, ?, ?, ?, ?, ?)",
			label, hash, prefix, sid, now(), boolInt(all))
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		if all {
			return nil
		}
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
	return c.JSON(201, jsjson.Object{{Key: "id", Val: id}, {Key: "label", Val: label}, {Key: "name", Val: label}, {Key: "prefix", Val: prefix},
		{Key: "key", Val: key}, {Key: "allServers", Val: all}, {Key: "servers", Val: keyServersJSON(all, ids)},
		{Key: "defaultServerId", Val: sid}, {Key: "server_id", Val: sid}})
}

// boolInt — 0/1 для SQLite: логических колонок в ui.db нет.
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// paramID — Number(req.params.id): ok, если целое.
func paramID(s string) (int64, bool) {
	f := jsToNumber(s)
	if f != f || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// PATCH {servers:[…]} | {all:true} — тот же смысл, что у POST. Сужение набора не блокируется,
// но в ответе приходят живые мастер-ключи, которые ключ перестанет видеть (lostMasters):
// панель показывает их в подтверждении.
func (a *App) apiKeysPatch(c *Ctx) bool {
	id, ok := paramID(c.Params["id"])
	row := a.keyByID(a.db, id)
	if !ok || row == nil {
		return c.Error(404, "Не найден")
	}
	all, ids, given, msg := a.keySet(c.Obj())
	if msg != "" {
		return c.Error(400, msg)
	}
	if !given {
		return c.Error(400, "servers или all: нужен набор серверов")
	}
	after := ids
	if all {
		after = a.allServerIDs()
	}
	lost := a.lostMasters(a.keyScope(row), after)
	if err := a.setKeyScope(id, all, ids); err != nil {
		return c.Error(500, "Internal server error")
	}
	if row = a.keyByID(a.db, id); row == nil {
		return c.Error(404, "Не найден")
	}
	return c.JSON(200, jsjson.Object{{Key: "id", Val: id}, {Key: "allServers", Val: row.AllServers},
		{Key: "servers", Val: keyServersJSON(row.AllServers, ids)}, {Key: "defaultServerId", Val: row.ServerID},
		{Key: "server_id", Val: row.ServerID}, {Key: "lostMasters", Val: lost}})
}

// lostMasters — живые (не отозванные) мастер-ключи, которые видны API-ключу сейчас и перестанут
// быть видны в наборе after. Считается до применения, поэтому before — старый набор.
// Список ключей читается и закрывается заранее: у a.db одно соединение, и запрос внутри
// незакрытых rows (visible → master_servers) встал бы навсегда.
func (a *App) lostMasters(before, after []int) []any {
	out := []any{}
	if a.sub == nil {
		return out
	}
	type masterLabel struct {
		id    int64
		label string
	}
	var masters []masterLabel
	rows, err := a.db.Query("SELECT id, label FROM master_keys WHERE revoked_at IS NULL ORDER BY id")
	if err != nil {
		return out
	}
	for rows.Next() {
		var m masterLabel
		if rows.Scan(&m.id, &m.label) == nil {
			masters = append(masters, m)
		}
	}
	rows.Close()
	for _, m := range masters {
		if a.sub.visible(m.id, before) && !a.sub.visible(m.id, after) {
			out = append(out, jsjson.Object{{Key: "id", Val: m.id}, {Key: "label", Val: m.label}})
		}
	}
	return out
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
	// Ключ «все серверы» строк не имеет, но помнит server_id по умолчанию; если это был
	// удалённый сервер, без X-Server-Id он бы 404-ил на /api/v1/users. Берём сервер панели.
	a.db.Exec("UPDATE api_keys SET server_id = ? WHERE all_servers = 1 AND server_id = ?", a.nodes.DefaultID(), id)
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
	// all_servers разворачивается в реестр на момент запроса: сервер, добавленный после
	// создания ключа, ключ видит сразу. Ниже по коду — всегда конкретные id, без sentinel.
	c.servers = a.keyScope(row)
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

// extUsersBlock — блокировка пользователя внешней системой (биллинг). Reason необязателен:
// manual — по решению оператора, api (по умолчанию) — по решению внешней системы. Ставится та
// же отметка users.blocked_reason, что у панели, поэтому /sub/v1/config и статистика видят
// блокировку без маппинга. 404 — пользователя нет, 502 — awg-ctrl не отвечает.
func (a *App) extUsersBlock(c *Ctx) bool {
	name := c.Params["name"]
	reason := "api"
	if v, ok := c.Obj().Get("reason"); ok && v != nil {
		r, isStr := v.(string)
		if !isStr || (r != "manual" && r != "api") {
			return c.Error(400, "reason: manual или api")
		}
		reason = r
	}
	r, err := a.extCtrl(c)("POST", "/api/users/"+encodeURIComponent(name)+"/block",
		jsjson.Object{{Key: "reason", Val: reason}})
	if err != nil {
		return unavailable(c)
	}
	if r.Status == 404 {
		return c.Error(404, "пользователь не найден")
	}
	if r.Status >= 400 {
		return c.JSON(r.Status, r.Data)
	}
	// Уже заблокированный пользователь сохраняет прежнюю причину — отдаём то, что в БД.
	if got, ok := str(field(r.Data, "blocked_reason")); ok && got != "" {
		reason = got
	}
	return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "name", Val: name}, {Key: "reason", Val: reason}})
}

// extUsersUnblock — снятие блокировки: blocked_at и blocked_reason очищаются, пир возвращается
// в conf/ядро. Идемпотентно: повторный unblock тоже 200.
func (a *App) extUsersUnblock(c *Ctx) bool {
	name := c.Params["name"]
	r, err := a.extCtrl(c)("POST", "/api/users/"+encodeURIComponent(name)+"/unblock", nil)
	if err != nil {
		return unavailable(c)
	}
	if r.Status == 404 {
		return c.Error(404, "пользователь не найден")
	}
	if r.Status >= 400 {
		return c.JSON(r.Status, r.Data)
	}
	return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "name", Val: name}})
}

// extUsersUsageReset — сброс израсходованного трафика (внешний биллинг обнуляет счётчик при
// продлении подписки). Счётчик живёт в awg-ctrl выбранного сервера ключа, туда и уходит
// запрос — тем же путём, что у панели. Идемпотентно: повторный сброс тоже 200, счётчик 0.
// Отсутствие пользователя отдаём своим текстом (внешний контракт не зависит от формулировки
// awg-ctrl), недоступный awg-ctrl — 502, как у остальных маршрутов /api/v1.
func (a *App) extUsersUsageReset(c *Ctx) bool {
	name := c.Params["name"]
	r, err := a.extCtrl(c)("POST", "/api/users/"+encodeURIComponent(name)+"/usage/reset", nil)
	if err != nil {
		return unavailable(c)
	}
	if r.Status == 404 {
		return c.Error(404, "пользователь не найден")
	}
	if r.Status >= 400 {
		return c.JSON(r.Status, r.Data)
	}
	return c.JSON(200, jsjson.Object{{Key: "success", Val: true}, {Key: "name", Val: name}})
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
	// Необязательные вкладки меню («Релеи», «Монитор», «Прокси»): какие показывать.
	app.Get("/ui/tabs", auth, a.tabsGet)
	app.Put("/ui/tabs", auth, a.tabsPut)

	// ВАЖНО: /api/v1 — до JWT-прокси на /api, иначе прокси потребует JWT вместо ключа.
	app.Use("/api/v1", a.requireAPIKey)
	app.Get("/api/v1/monitor", a.monitor.apiMonitor)
	app.Get("/api/v1/health", newHealthAPI(a).handle)
	app.Use("/api/v1/users", a.pickKeyServer)
	app.Post("/api/v1/users", a.extUsersCreate)
	app.Get("/api/v1/users", a.extUsersList)
	// Блокировка/разблокировка — до /users/:name: подпуть не должен зависеть от порядка
	// объявления (как и сброс счётчика).
	app.Post("/api/v1/users/:name/block", a.extUsersBlock)
	app.Post("/api/v1/users/:name/unblock", a.extUsersUnblock)
	// Сброс счётчика — до /users/:name: подпуть не должен зависеть от порядка объявления.
	app.Post("/api/v1/users/:name/usage/reset", a.extUsersUsageReset)
	app.Get("/api/v1/users/:name", a.extUsersGet)
	app.Delete("/api/v1/users/:name", a.extUsersDelete)
	app.Mount("/api/v1/masterkeys", a.sub.apiRouter(func(c *Ctx) []int { return c.servers }))

	app.Use("/ui/nodes", auth)
	app.Mount("/ui/nodes", a.nodes.router())
	app.Use("/ui/relays", auth)
	app.Mount("/ui/relays", a.nodes.relayRouter())
	app.Use("/ui/masterkeys", auth)
	app.Mount("/ui/masterkeys", a.sub.masterKeysRouter())
	app.Use("/ui/devices", auth)
	app.Mount("/ui/devices", a.sub.devicesRouter())

	// Prometheus: /metrics за отдельным токеном «только метрики» (не JWT и не ключ API).
	app.Use("/ui/metrics-token", auth)
	app.Mount("/ui/metrics-token", a.metrics.router())
	app.Get("/metrics", a.metrics.requireToken, a.metrics.handler)
	app.Get("/ui/connlog", auth, a.connlog)
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
	srv := &http.Server{Handler: prefixGate(a.prefixes, a.routes(), a.migration.awaitsImport), ReadHeaderTimeout: 30 * time.Second}
	logf("ui: listening on :%d", a.port)
	a.nodes.Start()
	a.sub.Start()
	return srv.Serve(ln)
}
