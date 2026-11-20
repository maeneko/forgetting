package ui

// Ключи API и набор серверов: all_servers (sentinel «все серверы») вместо пустого набора.
// По решению пользователя новый ключ по умолчанию видит все серверы, а ограничение набора —
// отдельное действие в панели. Харнесс как у monitor_api_test.go: настоящая ui.db, обработчики
// /ui/apikeys вызываются напрямую (JWT проверяет роутер), requireAPIKey — как в /api/v1.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func newAPIKeysTest(t *testing.T) *App {
	t.Helper()
	p := newMovePanel(t, "on", "") // LOCAL_NODE=on: сервер 0 есть всегда
	if err := p.a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	return p.a
}

func addKeyNode(t *testing.T, a *App, id int, name string) {
	t.Helper()
	if _, err := a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (?, ?, 'x', 1)", id, name); err != nil {
		t.Fatal(err)
	}
}

// panelKey — вызов обработчика /ui/apikeys: id — для PATCH.
func panelKey(t *testing.T, h handler, body jsjson.Object, id string) (int, jsjson.Object) {
	t.Helper()
	w := httptest.NewRecorder()
	c := &Ctx{W: w, R: httptest.NewRequest("POST", "/ui/apikeys", nil), Body: body}
	if id != "" {
		c.Params = map[string]string{"id": id}
	}
	h(c)
	v, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatalf("ответ не JSON (%d): %v\n%s", w.Code, err, w.Body)
	}
	o, _ := v.(jsjson.Object)
	return w.Code, o
}

func keyBody(kv ...any) jsjson.Object {
	o := jsjson.Object{}
	for i := 0; i+1 < len(kv); i += 2 {
		o = append(o, jsjson.KV{Key: kv[i].(string), Val: kv[i+1]})
	}
	return o
}

func nums(ids ...int) []any {
	out := []any{}
	for _, id := range ids {
		out = append(out, float64(id))
	}
	return out
}

// keyScope — что получил requireAPIKey: c.servers. Пустой набор — [], не nil.
func keyScope(t *testing.T, a *App, key, header, value string) []int {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", key)
	if header != "" {
		r.Header.Set(header, value)
	}
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) {
		t.Fatalf("ключ %q не принят: %s", key, c.W.(*httptest.ResponseRecorder).Body)
	}
	return c.servers
}

func scopeJSON(s []int) string { return string(jsjson.MustMarshal(intsJSON(s))) }

func keyRowCount(t *testing.T, a *App, id int64) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM api_key_servers WHERE key_id = ?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func keyAllServers(t *testing.T, a *App, id int64) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow("SELECT all_servers FROM api_keys WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Новый ключ без servers видит весь реестр — и серверы, появившиеся после создания.
func TestAPIKeyDefaultAllServers(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")

	code, out := panelKey(t, a.apiKeysCreate, keyBody("label", "ci"), "")
	if code != 201 || field(out, "allServers") != true {
		t.Fatalf("создание: %d %v", code, out)
	}
	if v, ok := out.Get("servers"); !ok || v != nil {
		t.Errorf("servers у «всех» = %v, ждём null", v)
	}
	if field(out, "defaultServerId") != float64(0) {
		t.Errorf("сервер по умолчанию: %v, ждём локальный 0", field(out, "defaultServerId"))
	}
	key, _ := out.Get2("key").(string)
	if key == "" {
		t.Fatal("нет открытого значения ключа")
	}
	id := int64(field(out, "id").(float64))
	if n := keyRowCount(t, a, id); n != 0 {
		t.Errorf("у «всех» %d строк в api_key_servers, ждём 0", n)
	}
	if got := scopeJSON(keyScope(t, a, key, "", "")); got != "[0,1]" {
		t.Errorf("область ключа: %s, ждём [0,1]", got)
	}

	// Сервер, добавленный после создания ключа, попадает в область сам.
	addKeyNode(t, a, 2, "nl-2")
	if got := scopeJSON(keyScope(t, a, key, "", "")); got != "[0,1,2]" {
		t.Errorf("область после новой ноды: %s, ждём [0,1,2]", got)
	}

	// Список панели: allServers, servers:null, defaultServerId, createdAt, lastUsed.
	code, list := panelKey(t, a.apiKeysList, jsjson.Object{}, "")
	if code != 200 {
		t.Fatalf("список: %d", code)
	}
	keys, _ := field(list, "keys").([]any)
	if len(keys) != 1 {
		t.Fatalf("ключей %d, ждём 1", len(keys))
	}
	k, _ := keys[0].(jsjson.Object)
	if field(k, "allServers") != true {
		t.Errorf("allServers в списке: %v", field(k, "allServers"))
	}
	if v, _ := k.Get("servers"); v != nil {
		t.Errorf("servers в списке: %v, ждём null", v)
	}
	if field(k, "defaultServerId") != float64(0) || field(k, "createdAt") == nil {
		t.Errorf("поля списка: %v", k)
	}
}

// Явный набор по-прежнему ограничивает: X-Server-Id вне набора — 404, новый сервер не виден.
func TestAPIKeyExplicitServersRestrict(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")
	addKeyNode(t, a, 2, "nl-2")

	code, out := panelKey(t, a.apiKeysCreate, keyBody("label", "bot", "servers", nums(1, 2)), "")
	if code != 201 || field(out, "allServers") != false {
		t.Fatalf("создание: %d %v", code, out)
	}
	if got := string(jsjson.MustMarshal(field(out, "servers"))); got != "[1,2]" {
		t.Errorf("servers: %s", got)
	}
	key, _ := out.Get2("key").(string)
	id := int64(field(out, "id").(float64))
	if n := keyRowCount(t, a, id); n != 2 {
		t.Errorf("строк в api_key_servers: %d, ждём 2", n)
	}

	addKeyNode(t, a, 3, "nl-3")
	if got := scopeJSON(keyScope(t, a, key, "", "")); got != "[1,2]" {
		t.Errorf("область явного ключа: %s, ждём [1,2]", got)
	}

	// pickKeyServer: 2 из набора, 3 — нет.
	r := httptest.NewRequest("GET", "/api/v1/users", nil)
	r.Header.Set("X-Api-Key", key)
	r.Header.Set("X-Server-Id", "2")
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) || !a.pickKeyServer(c) || c.serverID != 2 {
		t.Fatalf("X-Server-Id=2: %d", c.serverID)
	}
	r = httptest.NewRequest("GET", "/api/v1/users", nil)
	r.Header.Set("X-Api-Id", "")
	r.Header.Set("X-Api-Key", key)
	r.Header.Set("X-Server-Id", "3")
	c = &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) || a.pickKeyServer(c) || c.W.(*httptest.ResponseRecorder).Code != 404 {
		t.Errorf("X-Server-Id=3 вне набора не дал 404")
	}
}

// Пустой набор — 400 с текстом, ключ не создаётся.
func TestAPIKeyEmptyServersRejected(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")

	code, out := panelKey(t, a.apiKeysCreate, keyBody("label", "ci", "servers", []any{}), "")
	if code != 400 || field(out, "error") != "servers: пустой набор недопустим" {
		t.Fatalf("пустой набор: %d %v", code, out)
	}
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&n)
	if n != 0 {
		t.Errorf("ключ создан вопреки ошибке (%d)", n)
	}

	// PATCH с пустым набором тоже 400 и ничего не меняет.
	code, out = panelKey(t, a.apiKeysCreate, keyBody("label", "ok", "servers", nums(1)), "")
	if code != 201 {
		t.Fatalf("создание: %d %v", code, out)
	}
	id := field(out, "id").(float64)
	code, out = panelKey(t, a.apiKeysPatch, keyBody("servers", []any{}), jsjson.Number(id))
	if code != 400 || field(out, "error") != "servers: пустой набор недопустим" {
		t.Fatalf("PATCH пустого набора: %d %v", code, out)
	}
	if got := string(jsjson.MustMarshal(intsJSON(a.keyServers(a.db, int64(id))))); got != "[1]" {
		t.Errorf("набор после отказа: %s", got)
	}
}

// PATCH {all:true} снимает ограничение: строки удалены, область — весь реестр.
func TestAPIKeyPatchToAllClearsRows(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")
	addKeyNode(t, a, 2, "nl-2")

	_, out := panelKey(t, a.apiKeysCreate, keyBody("label", "bot", "servers", nums(1, 2)), "")
	key, _ := out.Get2("key").(string)
	id := int64(field(out, "id").(float64))

	code, out := panelKey(t, a.apiKeysPatch, keyBody("all", true), jsjson.Number(float64(id)))
	if code != 200 || field(out, "allServers") != true {
		t.Fatalf("PATCH all: %d %v", code, out)
	}
	if v, _ := out.Get("servers"); v != nil {
		t.Errorf("servers после PATCH all: %v, ждём null", v)
	}
	if n := keyRowCount(t, a, id); n != 0 {
		t.Errorf("осталось %d строк api_key_servers, ждём 0", n)
	}
	if n := keyAllServers(t, a, id); n != 1 {
		t.Errorf("all_servers = %d, ждём 1", n)
	}
	addKeyNode(t, a, 3, "nl-3")
	if got := scopeJSON(keyScope(t, a, key, "", "")); got != "[0,1,2,3]" {
		t.Errorf("область после PATCH all: %s", got)
	}

	// Пустое тело PATCH не расширяет ключ (иначе ключ «стал бы всех» случайно).
	code, _ = panelKey(t, a.apiKeysPatch, jsjson.Object{}, jsjson.Number(float64(id)))
	if code != 400 {
		t.Errorf("PATCH {} : %d, ждём 400", code)
	}
}

// Сужение набора не блокируется, но панель получает живые мастер-ключи, которые пропадут.
func TestAPIKeyPatchLostMasters(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")
	addKeyNode(t, a, 2, "nl-2")
	addKeyNode(t, a, 3, "nl-3")
	for _, q := range []string{
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES " +
			"(1, 'u1', 'Только 1', x'01', 1), (2, 'u2', '1 и 2', x'02', 1), " +
			"(3, 'u3', '1 и 3', x'03', 1), (4, 'u4', 'Отозван', x'04', 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1), (2, 1), (2, 2), (3, 1), (3, 3), (4, 2)",
		"UPDATE master_keys SET revoked_at = 1 WHERE id = 4",
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}

	_, out := panelKey(t, a.apiKeysCreate, keyBody("label", "bot", "servers", nums(1, 2, 3)), "")
	id := int64(field(out, "id").(float64))
	code, out := panelKey(t, a.apiKeysPatch, keyBody("servers", nums(1)), jsjson.Number(float64(id)))
	if code != 200 {
		t.Fatalf("PATCH: %d %v", code, out)
	}
	lost, _ := field(out, "lostMasters").([]any)
	var ids []int
	var labels []string
	for _, v := range lost {
		o, _ := v.(jsjson.Object)
		ids = append(ids, int(field(o, "id").(float64)))
		labels = append(labels, field(o, "label").(string))
	}
	if string(jsjson.MustMarshal(intsJSON(ids))) != "[2,3]" || strings.Join(labels, ",") != "1 и 2,1 и 3" {
		t.Errorf("lostMasters: %v %v, ждём ключи «1 и 2» и «1 и 3» (у них серверы вне нового набора)", ids, labels)
	}
	// Отозванный (4) в lostMasters не попадает; сужение применилось, ключи не тронуты.
	for _, id := range []int64{1, 2, 3, 4} {
		var n int
		a.db.QueryRow("SELECT COUNT(*) FROM master_keys WHERE id = ?", id).Scan(&n)
		if n != 1 {
			t.Errorf("мастер-ключ %d исчез", id)
		}
	}
	if got := string(jsjson.MustMarshal(intsJSON(a.keyServers(a.db, id)))); got != "[1]" {
		t.Errorf("набор после сужения: %s, ждём [1]", got)
	}
}

// Миграция: существующие ключи остаются явными (all_servers = 0), даже если строк нет —
// намерение «видеть всё» у них недоказуемо, и старый backfill возвращает им server_id.
func TestAPIKeysAllServersMigration(t *testing.T) {
	p := newMovePanel(t, "on", "")
	a := p.a
	// Старая схема: колонки all_servers ещё нет.
	for _, q := range []string{
		`CREATE TABLE api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
			server_id INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, last_used INTEGER)`,
		`CREATE TABLE api_key_servers (
			key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
			server_id INTEGER NOT NULL, PRIMARY KEY (key_id, server_id))`,
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (1, 'A', 'x', 1), (2, 'B', 'x', 1)",
		"INSERT INTO api_keys (id, label, key_hash, prefix, server_id, created_at) VALUES (1, 'Явный', 'h1', 'p1', 1, 1), (2, 'Без строк', 'h2', 'p2', 2, 1)",
		"INSERT INTO api_key_servers (key_id, server_id) VALUES (1, 1), (1, 2)",
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	if n := keyAllServers(t, a, 1); n != 0 {
		t.Errorf("ключ с явным набором: all_servers = %d, ждём 0", n)
	}
	if n := keyRowCount(t, a, 1); n != 2 {
		t.Errorf("явный набор потерял строки: %d, ждём 2", n)
	}
	if n := keyAllServers(t, a, 2); n != 0 {
		t.Errorf("ключ без строк: all_servers = %d, ждём 0 (намерение «все» недоказуемо)", n)
	}
	if n := keyRowCount(t, a, 2); n != 1 {
		t.Errorf("ключу без строк не вернули server_id: %d строк, ждём 1", n)
	}
	if got := scopeJSON(a.keyScope(a.keyByID(a.db, 2))); got != "[2]" {
		t.Errorf("область мигрированного ключа: %s, ждём [2]", got)
	}
	// Второй прогон (рестарт) миграцию не повторяет и ничего не ломает.
	if err := a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	if got := scopeJSON(a.keyScope(a.keyByID(a.db, 2))); got != "[2]" {
		t.Errorf("после рестарта: %s, ждём [2]", got)
	}
}

// defaultServerId всегда внутри явного набора; выпал — берём наименьший.
func TestAPIKeyDefaultServerInvariant(t *testing.T) {
	a := newAPIKeysTest(t)
	addKeyNode(t, a, 1, "nl-1")
	addKeyNode(t, a, 2, "nl-2")
	addKeyNode(t, a, 3, "nl-3")

	// defaultServerId 1 существует, но не входит в набор [2,3] — станет 2.
	code, out := panelKey(t, a.apiKeysCreate, keyBody("label", "bot", "servers", nums(2, 3), "defaultServerId", float64(1)), "")
	if code != 201 || field(out, "defaultServerId") != float64(2) {
		t.Fatalf("создание: %d %v", code, out)
	}
	id := int64(field(out, "id").(float64))

	// Сервер по умолчанию 2 выпал из набора [1] — станет 1.
	code, out = panelKey(t, a.apiKeysPatch, keyBody("servers", nums(1)), jsjson.Number(float64(id)))
	if code != 200 || field(out, "defaultServerId") != float64(1) {
		t.Fatalf("сужение: %d %v", code, out)
	}
	var sid int
	a.db.QueryRow("SELECT server_id FROM api_keys WHERE id = ?", id).Scan(&sid)
	if sid != 1 {
		t.Errorf("server_id в БД = %d, ждём 1", sid)
	}

	// У «всех» defaultServerId остаётся существующим сервером (1 из прошлого набора).
	code, out = panelKey(t, a.apiKeysPatch, keyBody("all", true), jsjson.Number(float64(id)))
	if code != 200 || field(out, "defaultServerId") != float64(1) {
		t.Fatalf("PATCH all: %d %v", code, out)
	}
	if got := scopeJSON(a.keyScope(a.keyByID(a.db, id))); got != "[0,1,2,3]" {
		t.Errorf("область «всех»: %s, ждём весь реестр", got)
	}
}
