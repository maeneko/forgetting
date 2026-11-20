package ui

// Сброс счётчиков трафика: публичный API (ключ awgk_) для внешнего биллинга — пользователи и
// мастер-ключи — плюс панельный дубль для оператора. Харнесс как в sub_limits_test.go:
// настоящая ui.db, awg-ctrl нод подменяется через Sub.ctrlFor; локальный awg-ctrl для
// /api/v1/users — httptest по a.ctrlURL (CtrlFor(0) идёт в ctrlLocal). Запросы идут через
// a.routes(), поэтому проверяются и сами маршруты, и их охрана (requireAPIKey / requireAuth).

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// ── Локальный awg-ctrl ─────────────────────────────────────────────────────

// fakeAWG — awg-ctrl этой машины: запоминает запросы и умеет ответить 404 (пользователя нет).
type fakeAWG struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []string // "METHOD путь"
	bodies   []string // тело каждого запроса
	notFound bool
}

func newFakeAWG(t *testing.T) *fakeAWG {
	t.Helper()
	f := &fakeAWG{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(raw))
		notFound := f.notFound
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if notFound {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"Пользователь не найден"}`)
			return
		}
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/usage/reset"):
			io.WriteString(w, `{"name":"ivan","used_bytes":0,"used_rx":0,"used_tx":0}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/block"):
			reason := "manual"
			if v, err := jsjson.Decode(raw); err == nil {
				if o, ok := v.(jsjson.Object); ok {
					if s, ok := o.Get2("reason").(string); ok {
						reason = s
					}
				}
			}
			fmt.Fprintf(w, `{"name":"ivan","blocked":true,"blocked_reason":%q}`, reason)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/unblock"):
			io.WriteString(w, `{"name":"ivan","blocked":false,"blocked_reason":null}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"нет такого пути"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAWG) lastCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeAWG) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeAWG) setNotFound(v bool) {
	f.mu.Lock()
	f.notFound = v
	f.mu.Unlock()
}

// ── Панель ─────────────────────────────────────────────────────────────────

// newUsageTest — newLimitsTest плюс всё, что нужно a.routes(): подписка, метрики, монитор,
// перенос, ключи API и httptest вместо локального awg-ctrl.
func newUsageTest(t *testing.T, servers int) (*App, *Sub, *fakeSubCtrl, *fakeAWG) {
	t.Helper()
	a, s, fake := newLimitsTest(t, servers)
	a.sub = s
	var err error
	if a.metrics, err = newMetrics(a); err != nil {
		t.Fatal(err)
	}
	a.monitor = newMonitor(a)
	a.migration = newMigration(a)
	if err := a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	awg := newFakeAWG(t)
	a.ctrlURL = awg.srv.URL
	a.httpLocal = &http.Client{Transport: &http.Transport{Proxy: nil}}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a.internalKey = priv // ctrlLocal без него отказывается идти в awg-ctrl
	return a, s, fake, awg
}

// usageCall — запрос через настоящий роутер панели: token — JWT, key — X-Api-Key,
// hdr — дополнительные заголовки парами.
func usageCall(t *testing.T, app *Router, method, target, token, key string, hdr ...string) (int, jsjson.Object) {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
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

// dropPeersCache — peersOf держит ответ peersTTL секунд, тесту нужны свежие показания.
func dropPeersCache(s *Sub, id int) {
	s.peersMu.Lock()
	delete(s.peerCalls, id)
	s.peersMu.Unlock()
}

// ── Пользователи: /api/v1/users/:name/usage/reset ──────────────────────────

func TestExtUserUsageReset(t *testing.T) {
	a, _, _, awg := newUsageTest(t, 2)      // серверы 0 (локальный) и 1
	addMonitorKey(t, a, 1, "awgk_usage", 0) // сервер по умолчанию — 0
	app := a.routes()
	const target = "/api/v1/users/ivan/usage/reset"

	// 200: путь и метод — как у внутреннего API awg-ctrl; наружу только success и name.
	code, out := usageCall(t, app, "POST", target, "", "awgk_usage")
	if code != 200 || field(out, "success") != true || field(out, "name") != "ivan" {
		t.Fatalf("сброс: %d %v", code, out)
	}
	if calls := awg.lastCalls(); len(calls) != 1 || calls[0] != "POST /api/users/ivan/usage/reset" {
		t.Fatalf("awg-ctrl получил %v", calls)
	}
	// Идемпотентно: повторный сброс так же 200.
	if code, _ := usageCall(t, app, "POST", target, "", "awgk_usage"); code != 200 {
		t.Fatalf("повторный сброс: %d", code)
	}
	if calls := awg.lastCalls(); len(calls) != 2 || calls[1] != "POST /api/users/ivan/usage/reset" {
		t.Fatalf("повтор не дошёл до awg-ctrl: %v", calls)
	}

	// Неизвестный пользователь — 404 с текстом внешнего контракта.
	awg.setNotFound(true)
	if code, out := usageCall(t, app, "POST", target, "", "awgk_usage"); code != 404 || field(out, "error") != "пользователь не найден" {
		t.Fatalf("неизвестный пользователь: %d %v", code, out)
	}
	awg.setNotFound(false)

	// Сервер вне набора ключа — 404, до awg-ctrl запрос не идёт (как GET/DELETE /users/:name).
	before := len(awg.lastCalls())
	if code, _ := usageCall(t, app, "POST", target, "", "awgk_usage", "X-Server-Id", "1"); code != 404 {
		t.Fatalf("чужой сервер: %d", code)
	}
	if calls := awg.lastCalls(); len(calls) != before {
		t.Fatalf("запрос ушёл в awg-ctrl при сервере вне набора: %v", calls)
	}

	// awg-ctrl не отвечает — 502.
	a.ctrlURL = "http://127.0.0.1:1"
	if code, out := usageCall(t, app, "POST", target, "", "awgk_usage"); code != 502 || field(out, "error") != "awg-ctrl недоступен" {
		t.Fatalf("awg-ctrl недоступен: %d %v", code, out)
	}
}

// ── Мастер-ключи: /api/v1/masterkeys/:id/usage/reset ───────────────────────

func TestMasterUsageReset(t *testing.T) {
	a, s, fake, _ := newUsageTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	addMonitorKey(t, a, 1, "awgk_m", 0)
	app := a.routes()
	const target = "/api/v1/masterkeys/1/usage/reset"

	// Как после тика: 110 байт учтено, ключ закрыт по трафику (лимит 100).
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100, used_rx = 60, used_tx = 50, blocked_at = 7, blocked_reason = 'traffic' WHERE id = 1")
	fake.setPeers(0, peer("WG", 60, 50))
	if code, out := usageCall(t, app, "POST", target, "", "awgk_m"); code != 200 ||
		field(out, "success") != true || field(out, "id") != float64(1) || field(out, "used_bytes") != float64(0) {
		t.Fatalf("сброс: %d %v", code, out)
	}
	m := s.masterRowByID(a.db, 1)
	if m.UsedRx != 0 || m.UsedTx != 0 {
		t.Fatalf("в ui.db used_rx=%d used_tx=%d", m.UsedRx, m.UsedTx)
	}
	// Блокировка по трафику снята, пиры вернулись (clearBlock обнулил links, reconcile поставил).
	if m.BlockedReason.Valid || m.BlockedAt.Valid {
		t.Fatalf("блокировка по трафику осталась: %v / %v", m.BlockedAt, m.BlockedReason)
	}
	if added := fake.addedKeys(0); !containsStr(added, "WG") {
		t.Fatalf("пир не поставлен заново: добавлено %v", added)
	}
	if brx, btx := s.usageBase(1); brx != 60 || btx != 50 {
		t.Fatalf("поправка на сброс: %d/%d, ожидали 60/50", brx, btx)
	}
	// Повтор идемпотентен и не оставляет старую поправку (сбрасывать уже нечего).
	if code, out := usageCall(t, app, "POST", target, "", "awgk_m"); code != 200 || field(out, "used_bytes") != float64(0) {
		t.Fatalf("повторный сброс: %d %v", code, out)
	}
	if brx, btx := s.usageBase(1); brx != 0 || btx != 0 {
		t.Fatalf("поправка после повторного сброса: %d/%d", brx, btx)
	}

	// expired и manual сброс не снимает — их снимает срок или оператор.
	for _, reason := range []string{blockExpired, blockManual} {
		execLimits(t, a, "UPDATE master_keys SET blocked_at = 7, blocked_reason = '"+reason+"', used_rx = 40, used_tx = 10 WHERE id = 1")
		if code, _ := usageCall(t, app, "POST", target, "", "awgk_m"); code != 200 {
			t.Fatalf("%s: сброс %d", reason, code)
		}
		m = s.masterRowByID(a.db, 1)
		if m.UsedRx != 0 || m.UsedTx != 0 {
			t.Fatalf("%s: счётчик не обнулён: %d/%d", reason, m.UsedRx, m.UsedTx)
		}
		if m.BlockedReason.String != reason || !m.BlockedAt.Valid {
			t.Fatalf("%s: сброс тронул блокировку: %v / %v", reason, m.BlockedAt, m.BlockedReason)
		}
	}
}

// Сброс сдвигает начало отсчёта: следующий тик не возвращает прежний трафик, а считает только
// прибавку после сброса.
func TestMasterResetBaseline(t *testing.T) {
	a, s, fake, _ := newUsageTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	addMonitorKey(t, a, 1, "awgk_m", 0)
	app := a.routes()
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 1000 WHERE id = 1")

	fake.setPeers(0, peer("WG", 300, 100))
	s.enforceMasters()
	if m := s.masterRowByID(a.db, 1); m.usedTotal() != 400 {
		t.Fatalf("до сброса учтено %d, ожидали 400", m.usedTotal())
	}

	if code, _ := usageCall(t, app, "POST", "/api/v1/masterkeys/1/usage/reset", "", "awgk_m"); code != 200 {
		t.Fatalf("сброс: %d", code)
	}
	// Те же показания awg-ctrl, что и до сброса, — тик обязан оставить ноль.
	dropPeersCache(s, 0)
	s.enforceMasters()
	if m := s.masterRowByID(a.db, 1); m.usedTotal() != 0 {
		t.Fatalf("тик вернул старый трафик: %d", m.usedTotal())
	}

	// Прибавка после сброса считается как обычно.
	fake.setPeers(0, peer("WG", 380, 120))
	dropPeersCache(s, 0)
	s.enforceMasters()
	if got := s.masterRowByID(a.db, 1).usedTotal(); got != 100 {
		t.Fatalf("после сброса учтено %d, ожидали 100 (80+20)", got)
	}
}

// Область ключа API: 200 — все серверы мастер-ключа в наборе, 403 — есть сервер вне набора,
// 404 — ключа нет или он удаляется. Тот же masterFor, что у остальных маршрутов.
func TestMasterUsageResetScope(t *testing.T) {
	a, s, _, _ := newUsageTest(t, 3) // серверы 0, 1, 2
	fixClock(t, 1700000000)
	execLimits(t, a,
		`INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES
            (1, '11111111-1111-4111-8111-111111111111', 'Свой', x'01', 1),
            (2, '22222222-2222-4222-8222-222222222222', 'Чужой', x'02', 1),
            (4, '44444444-4444-4444-8444-444444444444', 'Удалён', x'04', 1)`,
		"UPDATE master_keys SET revoked_at = 1, used_rx = 10 WHERE id = 4",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 0), (1, 1), (2, 2), (4, 0)",
		"UPDATE master_keys SET used_rx = 10 WHERE id = 1")
	addMonitorKey(t, a, 1, "awgk_01", 0, 1)
	addMonitorKey(t, a, 2, "awgk_2", 2)
	app := a.routes()

	call := func(key, id string) (int, jsjson.Object) {
		t.Helper()
		return usageCall(t, app, "POST", "/api/v1/masterkeys/"+id+"/usage/reset", "", key)
	}

	if code, out := call("awgk_01", "1"); code != 200 || field(out, "used_bytes") != float64(0) {
		t.Fatalf("свой ключ: %d %v", code, out)
	}
	if m := s.masterRowByID(a.db, 1); m.UsedRx != 0 {
		t.Fatalf("счётчик своего ключа: %d", m.UsedRx)
	}
	// Ключ 1 живёт на серверах 0 и 1 — для ключа API, который видит только 2, он вне области.
	if code, out := call("awgk_2", "1"); code != 403 {
		t.Fatalf("чужой ключ: %d %v", code, out)
	}
	// Удаляемый и несуществующий — 404, а не 403.
	for _, id := range []string{"4", "999", "не-id"} {
		if code, out := call("awgk_01", id); code != 404 {
			t.Fatalf("id %q: %d %v", id, code, out)
		}
	}
	// :id принимает и uuid — как у остальных маршрутов мастер-ключей.
	if code, out := call("awgk_01", "11111111-1111-4111-8111-111111111111"); code != 200 || field(out, "id") != float64(1) {
		t.Fatalf("по uuid: %d %v", code, out)
	}
}

// ── Панель: /ui/masterkeys/:id/usage/reset ─────────────────────────────────

func TestPanelMasterUsageReset(t *testing.T) {
	a, s, _, _ := newUsageTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100, used_rx = 70, used_tx = 30, blocked_at = 5, blocked_reason = 'traffic' WHERE id = 1")
	app := a.routes()
	const target = "/ui/masterkeys/1/usage/reset"

	// Без JWT — 401, счётчик не тронут.
	if code, _ := usageCall(t, app, "POST", target, "", ""); code != 401 {
		t.Fatalf("без JWT: %d", code)
	}
	if m := s.masterRowByID(a.db, 1); m.UsedRx != 70 || m.UsedTx != 30 {
		t.Fatalf("без JWT счётчик тронут: %d/%d", m.UsedRx, m.UsedTx)
	}

	// С JWT — ключ целиком, как у block/unblock: ноль, трафик-блокировка снята, лимит на месте.
	code, out := usageCall(t, app, "POST", target, a.sign(), "")
	if code != 200 || field(out, "id") != float64(1) || field(out, "usedBytes") != float64(0) ||
		field(out, "remainingBytes") != float64(100) || field(out, "blocked") != false || field(out, "blockReason") != nil {
		t.Fatalf("панельный сброс: %d %v", code, out)
	}
	// Идемпотентно и ключ остаётся на месте.
	if code, out := usageCall(t, app, "POST", target, a.sign(), ""); code != 200 || field(out, "usedBytes") != float64(0) {
		t.Fatalf("повтор: %d %v", code, out)
	}
	if s.masterByID(a.db, 1) == nil {
		t.Fatal("ключ пропал после сброса")
	}
}

// ── Охрана ─────────────────────────────────────────────────────────────────

// Оба публичных маршрута требуют ключ API: без него и с чужим — 401, до awg-ctrl не доходит.
func TestUsageResetRequiresKey(t *testing.T) {
	a, _, _, awg := newUsageTest(t, 1)
	addMonitorKey(t, a, 1, "awgk_ok", 0)
	app := a.routes()

	for _, target := range []string{"/api/v1/users/ivan/usage/reset", "/api/v1/masterkeys/1/usage/reset"} {
		if code, out := usageCall(t, app, "POST", target, "", ""); code != 401 || field(out, "error") != "API key required" {
			t.Fatalf("%s без ключа: %d %v", target, code, out)
		}
		if code, out := usageCall(t, app, "POST", target, "", "awgk_чужой"); code != 401 || field(out, "error") != "Invalid API key" {
			t.Fatalf("%s с чужим ключом: %d %v", target, code, out)
		}
	}
	if calls := awg.lastCalls(); len(calls) != 0 {
		t.Fatalf("запрос дошёл до awg-ctrl без ключа API: %v", calls)
	}
}
