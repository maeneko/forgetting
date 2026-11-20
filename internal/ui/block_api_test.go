package ui

// Блокировка «по API»: публичные маршруты /api/v1/users/:name/block|unblock (ключ awgk_) и
// /api/v1/masterkeys/:id/block|unblock. Харнесс — usage_reset_test.go: настоящая ui.db,
// локальный awg-ctrl подменён httptest (a.ctrlURL), ноды — fakeSubCtrl (Sub.ctrlFor).
// Причина api должна доходить до БД как есть: /sub/v1/config отдаёт её без маппинга в manual,
// а тик ограничений такую блокировку не снимает.

import (
	"database/sql"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// blockCall — запрос через настоящий роутер панели с необязательным JSON-телом.
func blockCall(t *testing.T, app *Router, method, target, token, key, body string) (int, jsjson.Object) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	v, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatalf("%s %s: ответ не JSON (%d): %s", method, target, w.Code, w.Body)
	}
	o, _ := v.(jsjson.Object)
	return w.Code, o
}

// ── Пользователи: /api/v1/users/:name/block|unblock ────────────────────────

func TestExtUserBlock(t *testing.T) {
	a, _, _, awg := newUsageTest(t, 2) // серверы 0 (локальный) и 1
	addMonitorKey(t, a, 1, "awgk_block", 0)
	app := a.routes()

	// Без ключа API — 401, до awg-ctrl не доходит.
	before := len(awg.lastCalls())
	if code, out := blockCall(t, app, "POST", "/api/v1/users/ivan/block", "", "", ""); code != 401 || field(out, "error") != "API key required" {
		t.Fatalf("без ключа: %d %v", code, out)
	}
	if calls := awg.lastCalls(); len(calls) != before {
		t.Fatalf("запрос ушёл в awg-ctrl без ключа: %v", calls)
	}

	// Без reason — по умолчанию api; путь и причина те же, что у панели.
	code, out := blockCall(t, app, "POST", "/api/v1/users/ivan/block", "", "awgk_block", "")
	if code != 200 || field(out, "success") != true || field(out, "name") != "ivan" || field(out, "reason") != "api" {
		t.Fatalf("блокировка по умолчанию: %d %v", code, out)
	}
	if calls := awg.lastCalls(); calls[len(calls)-1] != "POST /api/users/ivan/block" {
		t.Fatalf("awg-ctrl получил %v", calls)
	}
	if body := awg.lastBody(); !strings.Contains(body, `"api"`) {
		t.Fatalf("в awg-ctrl ушло %q, ждём reason api", body)
	}

	// Явный manual проходит как есть.
	code, out = blockCall(t, app, "POST", "/api/v1/users/ivan/block", "", "awgk_block", `{"reason":"manual"}`)
	if code != 200 || field(out, "reason") != "manual" {
		t.Fatalf("manual: %d %v", code, out)
	}
	if body := awg.lastBody(); !strings.Contains(body, `"manual"`) {
		t.Fatalf("в awg-ctrl ушло %q, ждём reason manual", body)
	}

	// Мусор — 400 с текстом контракта, до awg-ctrl не доходит.
	for _, body := range []string{`{"reason":"expired"}`, `{"reason":"traffic"}`, `{"reason":1}`} {
		calls := len(awg.lastCalls())
		if code, out := blockCall(t, app, "POST", "/api/v1/users/ivan/block", "", "awgk_block", body); code != 400 || field(out, "error") != "reason: manual или api" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
		if got := len(awg.lastCalls()); got != calls {
			t.Fatalf("%s: запрос ушёл в awg-ctrl", body)
		}
	}

	// Разблокировка — без тела, как в контроллере.
	code, out = blockCall(t, app, "POST", "/api/v1/users/ivan/unblock", "", "awgk_block", "")
	if code != 200 || field(out, "success") != true || field(out, "name") != "ivan" {
		t.Fatalf("разблокировка: %d %v", code, out)
	}
	if calls := awg.lastCalls(); calls[len(calls)-1] != "POST /api/users/ivan/unblock" {
		t.Fatalf("awg-ctrl получил %v", calls)
	}

	// Пользователя нет — 404 внешнего контракта.
	awg.setNotFound(true)
	if code, out := blockCall(t, app, "POST", "/api/v1/users/nobody/block", "", "awgk_block", ""); code != 404 || field(out, "error") != "пользователь не найден" {
		t.Fatalf("неизвестный пользователь: %d %v", code, out)
	}
	if code, out := blockCall(t, app, "POST", "/api/v1/users/nobody/unblock", "", "awgk_block", ""); code != 404 || field(out, "error") != "пользователь не найден" {
		t.Fatalf("unblock неизвестного: %d %v", code, out)
	}
	awg.setNotFound(false)

	// Сервер вне набора ключа — 404, до awg-ctrl запрос не идёт.
	before = len(awg.lastCalls())
	if code, _ := usageCall(t, app, "POST", "/api/v1/users/ivan/block", "", "awgk_block", "X-Server-Id", "1"); code != 404 {
		t.Fatalf("чужой сервер: %d", code)
	}
	if calls := awg.lastCalls(); len(calls) != before {
		t.Fatalf("запрос ушёл в awg-ctrl при сервере вне набора: %v", calls)
	}

	// awg-ctrl не отвечает — 502.
	a.ctrlURL = "http://127.0.0.1:1"
	if code, out := blockCall(t, app, "POST", "/api/v1/users/ivan/block", "", "awgk_block", ""); code != 502 || field(out, "error") != "awg-ctrl недоступен" {
		t.Fatalf("awg-ctrl недоступен: %d %v", code, out)
	}
	if code, out := blockCall(t, app, "POST", "/api/v1/users/ivan/unblock", "", "awgk_block", ""); code != 502 || field(out, "error") != "awg-ctrl недоступен" {
		t.Fatalf("unblock при недоступном awg-ctrl: %d %v", code, out)
	}
}

// ── Мастер-ключи: /api/v1/masterkeys/:id/block|unblock ─────────────────────

func TestExtMasterBlock(t *testing.T) {
	a, s, _, _ := newUsageTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	execLimits(t, a, "UPDATE master_keys SET uuid = '11111111-1111-4111-8111-111111111111' WHERE id = 1")
	addMonitorKey(t, a, 1, "awgk_m", 0)
	app := a.routes()
	const target = "/api/v1/masterkeys/1/block"

	// Без reason — api; ответ — masterOut, как у PATCH.
	code, out := blockCall(t, app, "POST", target, "", "awgk_m", "")
	if code != 200 || field(out, "id") != float64(1) || field(out, "blocked") != true || field(out, "blockReason") != blockAPI {
		t.Fatalf("блокировка по умолчанию: %d %v", code, out)
	}
	if m := s.masterRowByID(a.db, 1); m.BlockedReason.String != blockAPI || !m.BlockedAt.Valid {
		t.Fatalf("в ui.db %q / %v, ждём api", m.BlockedReason.String, m.BlockedAt)
	}
	// Блокировка по API не расчётная: тик ограничений её не снимает, даже когда лимитов нет.
	s.enforceMasters()
	if m := s.masterRowByID(a.db, 1); m.BlockedReason.String != blockAPI || !m.BlockedAt.Valid {
		t.Fatalf("тик снял блокировку api: %q / %v", m.BlockedReason.String, m.BlockedAt)
	}

	// Явный manual — тоже разрешён и виден в ответе.
	code, out = blockCall(t, app, "POST", target, "", "awgk_m", `{"reason":"manual"}`)
	if code != 200 || field(out, "blockReason") != blockManual {
		t.Fatalf("manual: %d %v", code, out)
	}

	// Мусор — 400, причина не меняется.
	for _, body := range []string{`{"reason":"expired"}`, `{"reason":"traffic"}`, `{"reason":"bogus"}`, `{"reason":1}`} {
		if code, out := blockCall(t, app, "POST", target, "", "awgk_m", body); code != 400 || field(out, "error") != "reason: manual или api" {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if m := s.masterRowByID(a.db, 1); m.BlockedReason.String != blockManual {
		t.Fatalf("мусор изменил причину: %q", m.BlockedReason.String)
	}

	// :id принимает uuid — как у остальных маршрутов мастер-ключей.
	if code, out := blockCall(t, app, "POST", "/api/v1/masterkeys/11111111-1111-4111-8111-111111111111/block", "", "awgk_m", ""); code != 200 || field(out, "id") != float64(1) || field(out, "blockReason") != blockAPI {
		t.Fatalf("по uuid: %d %v", code, out)
	}

	// config отдаёт причину как есть — api, без маппинга в manual.
	cfg := s.buildConfig(s.deviceByID(a.db, 5), nil)
	if field(cfg, "blocked") != blockAPI {
		t.Fatalf("config.blocked = %v, ждём api: %s", field(cfg, "blocked"), jsjson.MustMarshal(cfg))
	}

	// Разблокировка: 200, причина и отметка очищены, ответ — тот же masterOut.
	code, out = blockCall(t, app, "POST", "/api/v1/masterkeys/1/unblock", "", "awgk_m", "")
	if code != 200 || field(out, "blocked") != false || field(out, "blockReason") != nil {
		t.Fatalf("разблокировка: %d %v", code, out)
	}
	if m := s.masterRowByID(a.db, 1); m.BlockedReason.Valid || m.BlockedAt.Valid {
		t.Fatalf("unblock не очистил: %q / %v", m.BlockedReason.String, m.BlockedAt)
	}

	// Без ключа API — 401.
	if code, out := blockCall(t, app, "POST", target, "", "", ""); code != 401 || field(out, "error") != "API key required" {
		t.Fatalf("без ключа: %d %v", code, out)
	}
}

func TestExtMasterBlockScope(t *testing.T) {
	a, _, _, _ := newUsageTest(t, 3) // серверы 0, 1, 2
	fixClock(t, 1700000000)
	execLimits(t, a,
		`INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES
            (1, '11111111-1111-4111-8111-111111111111', 'Свой', x'01', 1),
            (2, '22222222-2222-4222-8222-222222222222', 'Чужой', x'02', 1),
            (4, '44444444-4444-4444-8444-444444444444', 'Удалён', x'04', 1)`,
		"UPDATE master_keys SET revoked_at = 1 WHERE id = 4",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 0), (1, 1), (2, 2), (4, 0)")
	addMonitorKey(t, a, 1, "awgk_01", 0, 1)
	addMonitorKey(t, a, 2, "awgk_2", 2)
	app := a.routes()
	// Удаляемый ключ на месте и без причины — до запросов (сведение может его убрать).
	var blocked sql.NullString
	if err := a.db.QueryRow("SELECT blocked_reason FROM master_keys WHERE id = 4").Scan(&blocked); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if blocked.Valid {
		t.Fatalf("удаляемый ключ уже заблокирован: %q", blocked.String)
	}

	call := func(key, path, body string) (int, jsjson.Object) {
		t.Helper()
		return blockCall(t, app, "POST", path, "", key, body)
	}

	// Ключ 1 на серверах 0 и 1 — в области ключа API [0,1].
	if code, out := call("awgk_01", "/api/v1/masterkeys/1/block", ""); code != 200 || field(out, "blockReason") != blockAPI {
		t.Fatalf("свой ключ: %d %v", code, out)
	}
	// Ключ 1 вне области ключа API, который видит только сервер 2 — 403, не 404.
	if code, out := call("awgk_2", "/api/v1/masterkeys/1/block", ""); code != 403 {
		t.Fatalf("чужой ключ: %d %v", code, out)
	}
	// Ключ в области — можно и снять блокировку.
	if code, out := call("awgk_01", "/api/v1/masterkeys/1/unblock", ""); code != 200 || field(out, "blocked") != false {
		t.Fatalf("unblock своего ключа: %d %v", code, out)
	}
	if code, out := call("awgk_2", "/api/v1/masterkeys/1/unblock", ""); code != 403 {
		t.Fatalf("unblock чужого ключа: %d %v", code, out)
	}
	// Удаляемый и несуществующий — 404; блокировка при этом не ставится.
	for _, id := range []string{"4", "999", "не-id"} {
		if code, out := call("awgk_01", "/api/v1/masterkeys/"+id+"/block", ""); code != 404 {
			t.Fatalf("id %q: %d %v", id, code, out)
		}
	}
}
