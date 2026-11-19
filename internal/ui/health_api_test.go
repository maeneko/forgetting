package ui

// Публичный GET /api/v1/health. Харнесс тот же, что у монитора (monitor_api_test.go):
// настоящая ui.db и requireAPIKey; CtrlFor подменяется фейком — сеть и WSS не нужны.

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// fakeHealthCtrl — CtrlFor по id: свой ответ /api/metrics на сервер, недоступность и счётчик
// вызовов. Значения можно менять между запросами (cpu_percent).
type fakeHealthCtrl struct {
	mu    sync.Mutex
	data  map[int]jsjson.Object
	down  map[int]bool
	calls map[int]int
}

func newFakeHealthCtrl() *fakeHealthCtrl {
	return &fakeHealthCtrl{data: map[int]jsjson.Object{}, down: map[int]bool{}, calls: map[int]int{}}
}

func (f *fakeHealthCtrl) set(id int, d jsjson.Object) {
	f.mu.Lock()
	f.data[id] = d
	f.mu.Unlock()
}

func (f *fakeHealthCtrl) setDown(id int) {
	f.mu.Lock()
	f.down[id] = true
	f.mu.Unlock()
}

func (f *fakeHealthCtrl) count(id int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeHealthCtrl) ctrlFor(id int) Ctrl {
	return func(method, path string, body any) (*CtrlResp, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[id]++
		if f.down[id] {
			return nil, errors.New("нода не отвечает")
		}
		d, ok := f.data[id]
		if !ok {
			return &CtrlResp{Status: 404, Data: errObj("Сервер не найден")}, nil
		}
		return &CtrlResp{Status: 200, Data: d}, nil
	}
}

// healthMetrics — тело /api/metrics в том виде, в каком его отдаёт awg-ctrl.
func healthMetrics(up bool, peers, peersOnline, rx, tx, cores float64, load []any, memTotal, memAvail float64, cpu []any) jsjson.Object {
	host := jsjson.Object{
		{Key: "cores", Val: cores},
		{Key: "load", Val: load},
	}
	if cpu != nil {
		host = append(host, jsjson.KV{Key: "cpu", Val: cpu})
	}
	host = append(host, jsjson.KV{Key: "mem", Val: jsjson.Object{
		{Key: "total", Val: memTotal}, {Key: "available", Val: memAvail},
	}})
	return jsjson.Object{
		{Key: "up", Val: up},
		{Key: "peers", Val: jsjson.Object{{Key: "total", Val: peers}, {Key: "online", Val: peersOnline}}},
		{Key: "rx_bytes", Val: rx}, {Key: "tx_bytes", Val: tx},
		{Key: "host", Val: host},
	}
}

func healthCPU(user, idle float64) []any {
	return []any{
		jsjson.Object{{Key: "mode", Val: "user"}, {Key: "val", Val: user}},
		jsjson.Object{{Key: "mode", Val: "idle"}, {Key: "val", Val: idle}},
	}
}

// newHealthTest — панель с реестром ключей API и живым Monitor (у него лимитер).
func newHealthTest(t *testing.T) (*App, *healthAPI) {
	t.Helper()
	t.Setenv("CORE_HOST", "")
	p := newMovePanel(t, "on", "")
	if err := p.a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	p.a.monitor = newMonitor(p.a)
	return p.a, newHealthAPI(p.a)
}

func healthCall(t *testing.T, a *App, h *healthAPI, token string) (*httptest.ResponseRecorder, jsjson.Object) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", token)
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) {
		t.Fatalf("ключ %q не принят", token)
	}
	h.handle(c)
	w := c.W.(*httptest.ResponseRecorder)
	v, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatalf("ответ не JSON (%d): %v\n%s", w.Code, err, w.Body)
	}
	doc, _ := v.(jsjson.Object)
	return w, doc
}

func healthServers(t *testing.T, doc jsjson.Object) []any {
	t.Helper()
	return arr(field(doc, "servers"))
}

// ── Счастливый путь: поля, порядок, CPU по дельте ───────────────────────────

func TestHealthAPIHappyPath(t *testing.T) {
	a, h := newHealthTest(t)
	fixClock(t, 1700000060)
	addMonitorNode(t, a, 1, "Германия")
	addMonitorNode(t, a, 2, "Сингапур")
	addMonitorKey(t, a, 1, "awgk_health", 1, 2)

	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 42, 17, 1234, 5678, 4, []any{0.4, 0.5, 0.6}, 8589934592, 4294967296, healthCPU(39, 61)))
	fake.set(2, healthMetrics(true, 5, 3, 100, 200, 8, []any{1.0, 2.0, 3.0}, 800, 400, nil))
	h.ctrl = fake.ctrlFor

	w, doc := healthCall(t, a, h, "awgk_health")
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}
	if got := jsonAt(t, doc, "generated_at"); got != `"2023-11-14T22:14:20Z"` {
		t.Errorf("generated_at = %s", got)
	}
	if got := jsonAt(t, doc, "scope"); got != "[1,2]" {
		t.Errorf("scope = %s", got)
	}
	for _, c := range []struct {
		key  string
		want float64
	}{{"servers_total", 2}, {"servers_online", 2}, {"servers_up", 2}, {"peers", 47}, {"peers_online", 20}} {
		if n := numAt(t, doc, "summary", c.key); n != c.want {
			t.Errorf("summary.%s = %v, ожидали %v", c.key, n, c.want)
		}
	}

	servers := healthServers(t, doc)
	if len(servers) != 2 {
		t.Fatalf("servers: %d записей (%s)", len(servers), jsonAt(t, doc, "servers"))
	}
	s1, s2 := servers[0], servers[1]
	if n := numAt(t, s1, "id"); n != 1 {
		t.Errorf("servers[0].id = %v", n)
	}
	if name, _ := field(s1, "name").(string); name != "Германия" {
		t.Errorf("servers[0].name = %q", name)
	}
	if b, ok := field(s1, "online").(bool); !ok || !b {
		t.Errorf("servers[0].online = %v", field(s1, "online"))
	}
	if b, ok := field(s1, "up").(bool); !ok || !b {
		t.Errorf("servers[0].up = %v", field(s1, "up"))
	}
	if n := numAt(t, s1, "peers"); n != 42 {
		t.Errorf("servers[0].peers = %v", n)
	}
	if n := numAt(t, s1, "peers_online"); n != 17 {
		t.Errorf("servers[0].peers_online = %v", n)
	}
	if n := numAt(t, s1, "rx_bytes"); n != 1234 {
		t.Errorf("servers[0].rx_bytes = %v", n)
	}
	if n := numAt(t, s1, "tx_bytes"); n != 5678 {
		t.Errorf("servers[0].tx_bytes = %v", n)
	}
	if n := numAt(t, s1, "cores"); n != 4 {
		t.Errorf("servers[0].cores = %v", n)
	}
	if got := jsonAt(t, s1, "load"); got != "[0.4,0.5,0.6]" {
		t.Errorf("servers[0].load = %s", got)
	}
	if n := numAt(t, s1, "mem_total_bytes"); n != 8589934592 {
		t.Errorf("servers[0].mem_total_bytes = %v", n)
	}
	if n := numAt(t, s1, "mem_available_bytes"); n != 4294967296 {
		t.Errorf("servers[0].mem_available_bytes = %v", n)
	}
	if src, _ := field(s1, "source").(string); src != "ctrl" {
		t.Errorf("servers[0].source = %q", src)
	}
	if _, ok := field(s1, "error").(string); ok {
		t.Errorf("servers[0].error не должен появляться у доступного сервера: %v", field(s1, "error"))
	}
	// Первый запрос: базовой линии нет — cpu_percent null, а не 0.
	nullAt(t, s1, "cpu_percent")
	// У второго сервера host.cpu нет вовсе.
	nullAt(t, s2, "cpu_percent")

	// Второй запрос: user +1 с, idle +7 с → занятость 1/8 = 12.5%.
	fake.set(1, healthMetrics(true, 42, 17, 1234, 5678, 4, []any{0.4, 0.5, 0.6}, 8589934592, 4294967296, healthCPU(40, 68)))
	_, doc = healthCall(t, a, h, "awgk_health")
	servers = healthServers(t, doc)
	if n := numAt(t, servers[0], "cpu_percent"); n != 12.5 {
		t.Errorf("servers[0].cpu_percent = %v, ожидали 12.5", n)
	}
	nullAt(t, servers[1], "cpu_percent")
	t.Logf("health (второй запрос): %s", jsjson.MustMarshal(doc))

	// Сброс счётчика (значения меньше прошлых) — снова null, без отрицательного процента.
	fake.set(1, healthMetrics(true, 42, 17, 1234, 5678, 4, []any{0.4, 0.5, 0.6}, 8589934592, 4294967296, healthCPU(1, 1)))
	_, doc = healthCall(t, a, h, "awgk_health")
	nullAt(t, healthServers(t, doc)[0], "cpu_percent")
}

// ── Область ключа ───────────────────────────────────────────────────────────

func TestHealthAPIScope(t *testing.T) {
	a, h := newHealthTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorNode(t, a, 2, "sg-1")
	addMonitorNode(t, a, 3, "us-1")
	// Ключ видит 1 и 2; сервер 9 в его области не существует и просто отсутствует.
	addMonitorKey(t, a, 1, "awgk_two", 1, 2, 9)
	addMonitorKey(t, a, 2, "awgk_one", 1)

	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 10, 1, 1, 2, 1, []any{0.1}, 100, 50, nil))
	fake.set(2, healthMetrics(true, 20, 2, 3, 4, 2, []any{0.2}, 200, 100, nil))
	fake.set(3, healthMetrics(true, 30, 3, 5, 6, 3, []any{0.3}, 300, 150, nil))
	h.ctrl = fake.ctrlFor

	// Ключ {1,2}: у каждого сервера свои значения — ничего не перепутано.
	_, doc := healthCall(t, a, h, "awgk_two")
	if got := jsonAt(t, doc, "scope"); got != "[1,2,9]" {
		t.Errorf("scope = %s", got)
	}
	servers := healthServers(t, doc)
	if len(servers) != 2 {
		t.Fatalf("servers: %d (%s)", len(servers), jsonAt(t, doc, "servers"))
	}
	if n := numAt(t, servers[0], "peers"); n != 10 {
		t.Errorf("server 1: peers = %v", n)
	}
	if n := numAt(t, servers[1], "peers"); n != 20 {
		t.Errorf("server 2: peers = %v", n)
	}
	if fake.count(3) != 0 {
		t.Errorf("сервер 3 вне области, но опрошен %d раз", fake.count(3))
	}

	// Ключ {1}: только сервер 1, хотя фейк умеет и 2.
	_, doc = healthCall(t, a, h, "awgk_one")
	if got := jsonAt(t, doc, "scope"); got != "[1]" {
		t.Errorf("scope = %s", got)
	}
	servers = healthServers(t, doc)
	if len(servers) != 1 {
		t.Fatalf("servers: %d (%s)", len(servers), jsonAt(t, doc, "servers"))
	}
	if n := numAt(t, doc, "summary", "servers_total"); n != 1 {
		t.Errorf("servers_total = %v", n)
	}
	if n := numAt(t, servers[0], "peers"); n != 10 {
		t.Errorf("server 1: peers = %v", n)
	}
}

// Ключ без серверов — пустая область, а не «вся панель»: серверы не опрашиваются.
func TestHealthAPINilScope(t *testing.T) {
	a, h := newHealthTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorKey(t, a, 1, "awgk_none")
	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 1, 1, 1, 1, 1, []any{1.0}, 1, 1, nil))
	h.ctrl = fake.ctrlFor

	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", "awgk_none")
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) {
		t.Fatal("ключ не принят")
	}
	c.servers = nil // как у ключа, которому серверы не назначены
	h.handle(c)
	w := c.W.(*httptest.ResponseRecorder)
	v, _ := jsjson.Decode(w.Body.Bytes())
	doc, _ := v.(jsjson.Object)
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}
	if got := jsonAt(t, doc, "scope"); got != "[]" {
		t.Errorf("scope = %s", got)
	}
	if got := jsonAt(t, doc, "servers"); got != "[]" {
		t.Errorf("servers = %s", got)
	}
	for _, k := range []string{"servers_total", "servers_online", "servers_up", "peers", "peers_online"} {
		if n := numAt(t, doc, "summary", k); n != 0 {
			t.Errorf("summary.%s = %v, ожидали 0", k, n)
		}
	}
	if fake.count(1) != 0 {
		t.Errorf("сервер опрошен без области: %d", fake.count(1))
	}
}

// ── Недоступный сервер: 200 и точная форма с null ──────────────────────────

func TestHealthAPIUnreachable(t *testing.T) {
	a, h := newHealthTest(t)
	addMonitorNode(t, a, 1, "Германия")
	addMonitorNode(t, a, 2, "Сингапур")
	addMonitorKey(t, a, 1, "awgk_health", 1, 2)

	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 42, 17, 1234, 5678, 4, []any{0.4}, 1000, 500, nil))
	fake.setDown(2)
	h.ctrl = fake.ctrlFor

	w, doc := healthCall(t, a, h, "awgk_health")
	if w.Code != 200 {
		t.Fatalf("недоступный сервер не должен давать 5xx: код %d", w.Code)
	}
	servers := healthServers(t, doc)
	if len(servers) != 2 {
		t.Fatalf("servers: %d", len(servers))
	}
	want := `{"id":2,"name":"Сингапур","online":false,"up":null,"peers":null,"peers_online":null,` +
		`"rx_bytes":null,"tx_bytes":null,"cores":null,"load":null,"mem_total_bytes":null,` +
		`"mem_available_bytes":null,"cpu_percent":null,"source":"none","error":"ctrl недоступен"}`
	if got := string(jsjson.MustMarshal(servers[1])); got != want {
		t.Errorf("недоступный сервер:\n получено %s\n ожидали %s", got, want)
	}
	if n := numAt(t, doc, "summary", "servers_total"); n != 2 {
		t.Errorf("servers_total = %v", n)
	}
	if n := numAt(t, doc, "summary", "servers_online"); n != 1 {
		t.Errorf("servers_online = %v", n)
	}
	if n := numAt(t, doc, "summary", "servers_up"); n != 1 {
		t.Errorf("servers_up = %v", n)
	}
	if n := numAt(t, doc, "summary", "peers"); n != 42 {
		t.Errorf("peers = %v (недоступный сервер не прибавляет 0)", n)
	}
}

// ── Приватность и авторизация ───────────────────────────────────────────────

func TestHealthAPIPrivacy(t *testing.T) {
	a, h := newHealthTest(t)
	addMonitorNode(t, a, 1, "Германия")
	addMonitorKey(t, a, 1, "awgk_health", 1)
	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 42, 17, 1234, 5678, 4, []any{0.4, 0.5, 0.6}, 8589934592, 4294967296, healthCPU(39, 61)))
	h.ctrl = fake.ctrlFor

	_, doc := healthCall(t, a, h, "awgk_health")
	raw := string(jsjson.MustMarshal(doc))
	for _, bad := range []string{"uuid", "device_name", "10.", "link", "psk", "secret", "pub_key", "peer_pub"} {
		if strings.Contains(raw, bad) {
			t.Errorf("в ответе есть приватная подстрока %q: %s", bad, raw)
		}
	}
}

func TestHealthAPIRequiresKey(t *testing.T) {
	a, _ := newHealthTest(t)
	c := &Ctx{W: httptest.NewRecorder(), R: httptest.NewRequest("GET", "/api/v1/health", nil)}
	if a.requireAPIKey(c) {
		t.Fatal("/api/v1/health пустил без ключа")
	}
	if w := c.W.(*httptest.ResponseRecorder); w.Code != 401 {
		t.Errorf("код без ключа = %d, ожидали 401", w.Code)
	}
}

// Лимитер — тот же, что у /api/v1/monitor: отказ 429 c Retry-After: 60.
func TestHealthAPISharesMonitorLimiter(t *testing.T) {
	a, h := newHealthTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorKey(t, a, 1, "awgk_health", 1)
	fake := newFakeHealthCtrl()
	fake.set(1, healthMetrics(true, 1, 1, 1, 1, 1, []any{0.1}, 1, 1, nil))
	h.ctrl = fake.ctrlFor
	a.monitor.apiLimit = newLimiter(1, time.Minute, nil)

	if w, _ := healthCall(t, a, h, "awgk_health"); w.Code != 200 {
		t.Fatalf("первый запрос: код %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", "awgk_health")
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) {
		t.Fatal("ключ не принят")
	}
	h.handle(c)
	w := c.W.(*httptest.ResponseRecorder)
	if w.Code != 429 {
		t.Fatalf("второй запрос: код %d, ожидали 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q", got)
	}
}
