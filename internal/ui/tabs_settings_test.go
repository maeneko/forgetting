package ui

// Настройка необязательных вкладок: GET/PUT /ui/tabs помнят в sub_meta, какие из
// «Релеи», «Монитор», «Прокси» показывать. Харнесс — usage_reset_test.go: настоящая
// ui.db, полный a.routes() с JWT, поэтому проверяются и маршрут, и его охрана.
// Базовые вкладки (users, masterkeys, settings) в allowlist не входят и
// запросом не выключаются.

import (
	"database/sql"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// tabsCall — запрос к /ui/tabs через настоящий роутер панели; возвращает код, тело
// строкой (для проверки точной формы JSON) и разобранный объект.
func tabsCall(t *testing.T, app *Router, method, token, body string) (int, string, jsjson.Object) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "/ui/tabs", rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	raw := w.Body.String()
	v, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatalf("%s /ui/tabs: ответ не JSON (%d): %s", method, w.Code, raw)
	}
	o, _ := v.(jsjson.Object)
	return w.Code, raw, o
}

// storedDisabled — то, что лежит в sub_meta под ключом настройки (nil — ключа нет).
func storedDisabled(t *testing.T, a *App) (string, bool) {
	t.Helper()
	var raw string
	err := a.db.QueryRow("SELECT value FROM sub_meta WHERE key = ?", tabsMetaKey).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		t.Fatalf("sub_meta %s: %v", tabsMetaKey, err)
	}
	return raw, true
}

func TestTabsDefaultAllEnabled(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 1)
	app := a.routes()

	// Без JWT — 401, как у остальных /ui/*.
	if code, _, _ := tabsCall(t, app, "GET", "", ""); code != 401 {
		t.Fatalf("GET без токена: %d", code)
	}

	// Ключа в sub_meta нет — включены все четыре, в порядке allowlist.
	code, raw, out := tabsCall(t, app, "GET", a.sign(), "")
	if code != 200 || raw != `{"enabled":["connlog","relays","monitor","proxy"]}` {
		t.Fatalf("по умолчанию: %d %s", code, raw)
	}
	if got := arr(out.Get2("enabled")); len(got) != 4 {
		t.Fatalf("включено не четыре: %v", out)
	}
	if _, ok := storedDisabled(t, a); ok {
		t.Fatal("GET не должен создавать ключ в sub_meta")
	}
}

func TestTabsPutPersists(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 1)
	app := a.routes()
	token := a.sign()

	// Выключаем «Монитор»: в ответе только включённые, в базе — дополнение.
	code, raw, _ := tabsCall(t, app, "PUT", token, `{"enabled":["connlog","relays","proxy"]}`)
	if code != 200 || raw != `{"enabled":["connlog","relays","proxy"]}` {
		t.Fatalf("PUT: %d %s", code, raw)
	}
	if got, ok := storedDisabled(t, a); !ok || got != `["monitor"]` {
		t.Fatalf("в sub_meta не дополнение: %q (есть: %v)", got, ok)
	}

	// Новый запрос через новый роутер (и новый разбор sub_meta) видит сохранённое.
	app2 := a.routes()
	if code, raw, _ := tabsCall(t, app2, "GET", token, ""); code != 200 || raw != `{"enabled":["connlog","relays","proxy"]}` {
		t.Fatalf("после PUT: %d %s", code, raw)
	}

	// Дубликаты игнорируются, порядок нормализуется к allowlist.
	code, raw, _ = tabsCall(t, app, "PUT", token, `{"enabled":["proxy","proxy","relays"]}`)
	if code != 200 || raw != `{"enabled":["relays","proxy"]}` {
		t.Fatalf("дубликаты/порядок: %d %s", code, raw)
	}

	// Явная пустая запись — всё включено (дополнение пустое, ключ в базе есть).
	code, raw, _ = tabsCall(t, app, "PUT", token, `{"enabled":["connlog","relays","monitor","proxy"]}`)
	if code != 200 || raw != `{"enabled":["connlog","relays","monitor","proxy"]}` {
		t.Fatalf("вернуть всё: %d %s", code, raw)
	}
	if got, ok := storedDisabled(t, a); !ok || got != `[]` {
		t.Fatalf("дополнение не пустое: %q", got)
	}
}

func TestTabsBadRequests(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 1)
	app := a.routes()
	token := a.sign()
	wantErr := `{"error":"enabled: connlog, relays, monitor, proxy"}`

	cases := []struct{ name, body string }{
		{"неизвестный id", `{"enabled":["relays","socks"]}`},
		{"базовая вкладка users", `{"enabled":["users"]}`},
		{"базовая вкладка masterkeys", `{"enabled":["relays","masterkeys"]}`},
		{"базовая вкладка settings", `{"enabled":["settings"]}`},
		{"не массив", `{"enabled":"relays"}`},
		{"null", `{"enabled":null}`},
		{"нет ключа", `{}`},
		{"не строка в массиве", `{"enabled":[1]}`},
		{"верхнеуровневый массив", `[]`},
	}
	for _, c := range cases {
		code, raw, _ := tabsCall(t, app, "PUT", token, c.body)
		if code != 400 || raw != wantErr {
			t.Fatalf("%s (%s): %d %s", c.name, c.body, code, raw)
		}
	}
	// Ни один неверный запрос ничего не записал — всё включено.
	if code, raw, _ := tabsCall(t, app, "GET", token, ""); code != 200 || raw != `{"enabled":["connlog","relays","monitor","proxy"]}` {
		t.Fatalf("после 400: %d %s", code, raw)
	}

	// PUT без JWT — 401 и запись не создаётся.
	if code, _, _ := tabsCall(t, app, "PUT", "", `{"enabled":["relays"]}`); code != 401 {
		t.Fatalf("PUT без токена: %d", code)
	}
	if _, ok := storedDisabled(t, a); ok {
		t.Fatal("PUT без токена записал настройку")
	}
}

func TestTabsMalformedStored(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 1)
	app := a.routes()
	token := a.sign()

	execLimits(t, a, `INSERT INTO sub_meta (key, value) VALUES ('tabs_disabled', 'не json')`)
	code, raw, _ := tabsCall(t, app, "GET", token, "")
	if code != 200 || raw != `{"enabled":["connlog","relays","monitor","proxy"]}` {
		t.Fatalf("испорченный JSON: %d %s", code, raw)
	}

	// Не массив, а строка — тоже «всё включено», без 500.
	execLimits(t, a, `UPDATE sub_meta SET value = '"relays"' WHERE key = 'tabs_disabled'`)
	if code, raw, _ := tabsCall(t, app, "GET", token, ""); code != 200 || raw != `{"enabled":["connlog","relays","monitor","proxy"]}` {
		t.Fatalf("не массив: %d %s", code, raw)
	}

	// Неизвестные id в сохранённой записи отбрасываются, известные работают.
	execLimits(t, a, `UPDATE sub_meta SET value = '["monitor","socks",42]' WHERE key = 'tabs_disabled'`)
	if code, raw, _ := tabsCall(t, app, "GET", token, ""); code != 200 || raw != `{"enabled":["connlog","relays","proxy"]}` {
		t.Fatalf("неизвестные id в базе: %d %s", code, raw)
	}
}

// [] в enabled — валидный запрос по контракту: выключить все необязательные вкладки.
// Базовые при этом остаются на месте — их выключить нечем.
func TestTabsEmptyEnableList(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 1)
	app := a.routes()
	token := a.sign()

	code, raw, _ := tabsCall(t, app, "PUT", token, `{"enabled":[]}`)
	if code != 200 || raw != `{"enabled":[]}` {
		t.Fatalf("пустой список: %d %s", code, raw)
	}
	if got, ok := storedDisabled(t, a); !ok || got != `["connlog","relays","monitor","proxy"]` {
		t.Fatalf("дополнение: %q", got)
	}
	if code, raw, _ := tabsCall(t, app, "GET", token, ""); code != 200 || raw != `{"enabled":[]}` {
		t.Fatalf("после выключения всех: %d %s", code, raw)
	}
}
