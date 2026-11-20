package ui

// Публичный монитор GET /api/v1/monitor. Prometheus подменяется httptest-сервером, как в
// TestPrometheusOnNodeZero (monitor_test.go): Monitor читает /-/ready, /api/v1/query и
// /api/v1/query_range по m.promURL, поэтому другого источника тестам не нужно.
// Реестр (ключи/устройства) и области ключей — настоящие, в ui.db.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	fakeTS1 int64 = 1699996800 // первая точка фейковых рядов
	fakeTS2 int64 = 1700000060 // последняя точка; она же «сейчас» в golden/кэше
)

func fakeServerName(id int) string {
	switch id {
	case 0:
		return "VPN"
	case 1:
		return "nl-1"
	case 2:
		return "sg-1"
	}
	return "srv-" + strconv.Itoa(id)
}

func fakeNum(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Значения фейкового Prometheus. Правила просматриваются по порядку: более специфичный
// префикс запроса должен стоять раньше общего (awg_peers_received_bytes до awg_peers).
var (
	fakeRangeRules = []struct {
		match       string
		first, last float64
		empty       bool
	}{
		{"rate(awg_link_transmit_bytes_total", 1000, 1200, false},
		{"rate(awg_link_receive_bytes_total", 2000, 2200, false},
		{"awg_peers_online", 30, 31, false},
		{"awg_shaping_heavy_devices", 2, 3, false},
		{"rate(awg_shaping_qdisc_drops_total", 4, 5, false},
		// Пустой успешный ответ: congested остаётся null, но degraded не включается.
		{"awg_shaping_congested", 0, 0, true},
		{"awg_scrape_ok", 1, 1, false},
		{"awg_host_cpu_seconds_total", 12.5, 13.5, false},
		{"awg_host_memory_available_bytes", 40, 41, false},
		{"awg_host_cpu_cores", 4, 4, false},
		{"awg_host_memory_total_bytes", 8000000, 8000000, false},
	}
	fakeInstantRules = []struct {
		match string
		val   float64
	}{
		{"increase(awg_link_transmit_bytes_total", 5000},
		{"increase(awg_link_receive_bytes_total", 7000},
		{"awg_peers_received_bytes", 900000},
		{"awg_peers_sent_bytes", 800000},
		{"awg_up", 1},
		{"awg_shaping_bandwidth_bits", 1000000},
		{"awg_peers", 30},
	}
)

// promFake — Prometheus на httptest: ряды по server_id из ids, счётчики вызовов и
// возможность уронить один запрос или задержать ответ (single-flight).
type promFake struct {
	srv    *httptest.Server
	ids    []int
	spread bool          // значения сдвигаются на id*1000 — чтобы сумма по серверам была видна
	fail   string        // подстрока query, на которую отвечаем 500
	delay  time.Duration // пауза перед ответом

	mu      sync.Mutex
	ranges  int
	queries int
}

func newPromFake(t *testing.T, ids []int, spread bool) *promFake {
	t.Helper()
	p := &promFake{ids: ids, spread: spread}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *promFake) counts() (ranges, queries int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ranges, p.queries
}

func (p *promFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/-/ready" {
		io.WriteString(w, "Prometheus Server is Ready.\n")
		return
	}
	p.mu.Lock()
	d := p.delay
	switch r.URL.Path {
	case "/api/v1/query_range":
		p.ranges++
	case "/api/v1/query":
		p.queries++
	}
	p.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	q := r.URL.Query().Get("query")
	if p.fail != "" && strings.Contains(q, p.fail) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/api/v1/query_range":
		io.WriteString(w, p.matrix(q))
	case "/api/v1/query":
		io.WriteString(w, p.vector(q))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (p *promFake) matrix(q string) string {
	var first, last float64
	empty, matched := false, false
	for _, r := range fakeRangeRules {
		if strings.Contains(q, r.match) {
			first, last, empty, matched = r.first, r.last, r.empty, true
			break
		}
	}
	parts := []string{}
	if matched && !empty {
		for _, id := range p.ids {
			off := p.offset(id)
			parts = append(parts, fmt.Sprintf(
				`{"metric":{"server":%q,"server_id":%q},"values":[[%d,%s],[%d,%s]]}`,
				fakeServerName(id), strconv.Itoa(id), fakeTS1, fakeNum(first+off), fakeTS2, fakeNum(last+off)))
		}
	}
	return `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(parts, ",") + `]}}`
}

func (p *promFake) vector(q string) string {
	var val float64
	matched := false
	for _, r := range fakeInstantRules {
		if strings.Contains(q, r.match) {
			val, matched = r.val, true
			break
		}
	}
	parts := []string{}
	if matched {
		for _, id := range p.ids {
			parts = append(parts, fmt.Sprintf(
				`{"metric":{"server":%q,"server_id":%q},"value":[%d,%s]}`,
				fakeServerName(id), strconv.Itoa(id), fakeTS2, fakeNum(val+p.offset(id))))
		}
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(parts, ",") + `]}}`
}

func (p *promFake) offset(id int) float64 {
	if !p.spread {
		return 0
	}
	return float64(id) * 1000
}

// ── Панель, ключи и запросы ─────────────────────────────────────────────────

// newMonitorTest — панель с реестром sen:// (initSchema), таблицами ключей API и живым
// Monitor (newMonitor обязателен: без него лимитер и кэш — nil).
func newMonitorTest(t *testing.T) (*App, *Monitor) {
	t.Helper()
	t.Setenv("CORE_HOST", "")
	p := newMovePanel(t, "on", "")
	if err := p.a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	p.a.monitor = newMonitor(p.a)
	return p.a, p.a.monitor
}

func addMonitorNode(t *testing.T, a *App, id int, name string) {
	t.Helper()
	if _, err := a.db.Exec("INSERT INTO nodes (id, name, pub, created_at, version) VALUES (?, ?, 'pub', 1, '0.5.5')", id, name); err != nil {
		t.Fatal(err)
	}
}

func addMonitorKey(t *testing.T, a *App, id int64, token string, servers ...int) {
	t.Helper()
	if _, err := a.db.Exec("INSERT INTO api_keys (id, label, key_hash, prefix, created_at, all_servers) VALUES (?, 'mon', ?, 'awgk_…', 1, 0)", id, hashKey(token)); err != nil {
		t.Fatal(err)
	}
	for _, s := range servers {
		if _, err := a.db.Exec("INSERT INTO api_key_servers (key_id, server_id) VALUES (?, ?)", id, s); err != nil {
			t.Fatal(err)
		}
	}
}

func execMonitor(t *testing.T, a *App, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// monitorCall — запрос через requireAPIKey (как настоящий /api/v1) и обработчик.
func monitorCall(t *testing.T, a *App, token, target string) (*httptest.ResponseRecorder, jsjson.Object) {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	r.Header.Set("X-Api-Key", token)
	c := &Ctx{W: httptest.NewRecorder(), R: r}
	if !a.requireAPIKey(c) {
		t.Fatalf("ключ %q не принят", token)
	}
	a.monitor.apiMonitor(c)
	w := c.W.(*httptest.ResponseRecorder)
	v, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatalf("ответ не JSON (%d): %v\n%s", w.Code, err, w.Body)
	}
	doc, _ := v.(jsjson.Object)
	return w, doc
}

func useFake(t *testing.T, m *Monitor, p *promFake) {
	t.Helper()
	m.promURL = p.srv.URL
	m.localEnabled = true
	m.onNode = false
	m.forgetSource()
}

func useNoPrometheus(t *testing.T, m *Monitor) {
	t.Helper()
	m.promURL = "http://127.0.0.1:1" // закрытый порт: /-/ready не отвечает
	m.localEnabled = true
	m.onNode = false
	m.forgetSource()
}

func fixClock(t *testing.T, unix int64) {
	t.Helper()
	old := nowFn
	nowFn = func() time.Time { return time.Unix(unix, 0) }
	t.Cleanup(func() { nowFn = old })
}

func numAt(t *testing.T, o any, keys ...string) float64 {
	t.Helper()
	v := field(o, keys...)
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%v: ожидали число, получили %T (%v)", keys, v, v)
	}
	return f
}

func nullAt(t *testing.T, o any, keys ...string) {
	t.Helper()
	if v := field(o, keys...); v != nil {
		t.Fatalf("%v: ожидали null, получили %T (%v)", keys, v, v)
	}
}

func jsonAt(t *testing.T, o any, keys ...string) string {
	t.Helper()
	return string(jsjson.MustMarshal(field(o, keys...)))
}

func seedMonitorRegistry(t *testing.T, a *App) {
	t.Helper()
	execMonitor(t, a,
		// master 1 — только сервер 1, master 2 — {1,2}, master 3 — удаляемый.
		`INSERT INTO master_keys (id, uuid, label, secret, device_limit, created_at) VALUES
			(1, 'uuid-1', 'label-one', x'01', 3, 1),
			(2, 'uuid-2', 'label-two', x'02', 2, 1),
			(3, 'uuid-3', 'label-revoked', x'03', 7, 1)`,
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1), (2, 1), (2, 2), (3, 1)",
		"UPDATE master_keys SET revoked_at = 42 WHERE id = 3",
		`INSERT INTO devices (id, master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, last_seen) VALUES
			(1, 1, 'dev-1', 'Dana', 'Android 14', 'WK1', 'AU1', 1, 111),
			(2, 2, 'dev-2', 'Bob', 'iOS', 'WK2', 'AU2', 1, 222),
			(3, 3, 'dev-3', 'Rev', '', 'WK3', 'AU3', 1, 333)`,
	)
}

// ── 3. Область ключа ────────────────────────────────────────────────────────

func TestMonitorAPIScope(t *testing.T) {
	a, m := newMonitorTest(t)
	// Серверы 1 и 2 существуют (иначе {1,2} считался бы {1}: висячий id — «удалён»).
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorNode(t, a, 2, "sg-1")
	seedMonitorRegistry(t, a)
	addMonitorKey(t, a, 1, "awgk_scope1", 1)

	fake := newPromFake(t, []int{0, 1, 2}, true) // Prometheus отдаёт и чужие серверы
	useFake(t, m, fake)

	w, doc := monitorCall(t, a, "awgk_scope1", "/api/v1/monitor?range=1h")
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}
	if got := jsonAt(t, doc, "scope"); got != "[1]" {
		t.Errorf("scope = %s", got)
	}

	// servers[] — только сервер 1, и он из реестра, а не из меток Prometheus.
	servers := arr(field(doc, "servers"))
	if len(servers) != 1 {
		t.Fatalf("servers: %d записей (%s)", len(servers), jsonAt(t, doc, "servers"))
	}
	if id := numAt(t, servers[0], "id"); id != 1 {
		t.Errorf("servers[0].id = %v", id)
	}
	if name, _ := field(servers[0], "name").(string); name != "nl-1" {
		t.Errorf("servers[0].name = %q", name)
	}

	// Ряды и суммы — только сервер 1 (base + id*1000), а не 0 и 2.
	for _, c := range []struct {
		key  string
		want string
	}{
		{"down_bps", "[[1699996800,2000],[1700000060,2200]]"},
		{"up_bps", "[[1699996800,3000],[1700000060,3200]]"},
		{"online_peers", "[[1699996800,1030],[1700000060,1031]]"},
		{"cpu_percent", "[[1699996800,1012.5],[1700000060,1013.5]]"},
		{"mem_percent", "[[1699996800,1040],[1700000060,1041]]"},
	} {
		if got := jsonAt(t, doc, "series", c.key); got != c.want {
			t.Errorf("series.%s = %s, ожидали %s", c.key, got, c.want)
		}
	}

	// Итоги — по области, а не по всему, что вернул Prometheus.
	if n := numAt(t, doc, "totals", "peers"); n != 1030 {
		t.Errorf("totals.peers = %v", n)
	}
	if n := numAt(t, doc, "totals", "peers_online"); n != 1031 {
		t.Errorf("totals.peers_online = %v", n)
	}
	if n := numAt(t, doc, "traffic", "window", "rx_bytes"); n != 8000 {
		t.Errorf("traffic.window.rx_bytes = %v", n)
	}
	if n := numAt(t, doc, "traffic", "window", "tx_bytes"); n != 6000 {
		t.Errorf("traffic.window.tx_bytes = %v", n)
	}
	if n := numAt(t, doc, "traffic", "cumulative", "rx_bytes"); n != 901000 {
		t.Errorf("traffic.cumulative.rx_bytes = %v", n)
	}
	if n := numAt(t, doc, "traffic", "cumulative", "tx_bytes"); n != 801000 {
		t.Errorf("traffic.cumulative.tx_bytes = %v", n)
	}

	// Реестр: ключ {1} виден, {1,2} и удаляемый — нет; устройства идут за ключом.
	if n := numAt(t, doc, "totals", "master_keys"); n != 1 {
		t.Errorf("master_keys = %v (ожидали только ключ {1})", n)
	}
	if n := numAt(t, doc, "totals", "devices"); n != 1 {
		t.Errorf("devices = %v", n)
	}
	if n := numAt(t, doc, "totals", "device_slots"); n != 3 {
		t.Errorf("device_slots = %v", n)
	}
	if n := numAt(t, doc, "platforms", "total"); n != 1 {
		t.Errorf("platforms.total = %v", n)
	}
	if got := jsonAt(t, doc, "platforms", "by_os"); got != `[{"os":"android","devices":1},{"os":"windows","devices":0},{"os":"macos","devices":0},{"os":"linux","devices":0},{"os":"ios","devices":0},{"os":"other","devices":0},{"os":"unknown","devices":0}]` {
		t.Errorf("platforms.by_os = %s", got)
	}

	// Сервер в servers[] один — значит и серверные поля только его.
	if n := numAt(t, servers[0], "rx_bytes"); n != 901000 {
		t.Errorf("servers[0].rx_bytes = %v", n)
	}
	if n := numAt(t, servers[0], "down_bps"); n != 2200 {
		t.Errorf("servers[0].down_bps = %v", n)
	}
	if n := numAt(t, servers[0], "bandwidth_bits"); n != 1001000 {
		t.Errorf("servers[0].bandwidth_bits = %v", n)
	}
}

func TestMonitorAPINilScope(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	seedMonitorRegistry(t, a)
	// Ключ без серверов: в БД строк нет — c.servers пустой (не nil).
	addMonitorKey(t, a, 1, "awgk_noscope")
	useNoPrometheus(t, m)

	w, doc := monitorCall(t, a, "awgk_noscope", "/api/v1/monitor")
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}
	if got := jsonAt(t, doc, "scope"); got != "[]" {
		t.Errorf("scope = %s", got)
	}
	for _, k := range []string{"master_keys", "devices", "device_slots", "servers_total", "servers_online"} {
		if n := numAt(t, doc, "totals", k); n != 0 {
			t.Errorf("totals.%s = %v, ожидали 0", k, n)
		}
	}
	nullAt(t, doc, "totals", "peers")
	nullAt(t, doc, "totals", "peers_online")
	if got := jsonAt(t, doc, "servers"); got != "[]" {
		t.Errorf("servers = %s", got)
	}
	if n := numAt(t, doc, "platforms", "total"); n != 0 {
		t.Errorf("platforms.total = %v", n)
	}

	// И буквальный nil в Ctx (панельный visible(nil) значит «всё» — здесь обязан стать []).
	c := &Ctx{W: httptest.NewRecorder(), R: httptest.NewRequest("GET", "/api/v1/monitor", nil),
		apiKey: &apiKeyRow{ID: 99}, servers: nil}
	m.apiMonitor(c)
	if c.W.(*httptest.ResponseRecorder).Code != 200 {
		t.Fatalf("nil scope: код %d", c.W.(*httptest.ResponseRecorder).Code)
	}
	v, _ := jsjson.Decode(c.W.(*httptest.ResponseRecorder).Body.Bytes())
	if got := jsonAt(t, v, "scope"); got != "[]" {
		t.Errorf("nil scope → %s, ожидали []", got)
	}
	if got := jsonAt(t, v, "servers"); got != "[]" {
		t.Errorf("nil scope servers = %s", got)
	}
	if n := numAt(t, v, "totals", "master_keys"); n != 0 {
		t.Errorf("nil scope master_keys = %v", n)
	}
}

// ── 3b. Выбор серверов (?server=) ──────────────────────────────────────────

func TestMonitorAPIParseServerFilter(t *testing.T) {
	key := []int{1, 2}
	ok := []struct {
		values []string
		want   string
	}{
		{nil, "[1 2]"}, // без параметра — область ключа как есть
		{[]string{"1"}, "[1]"},
		{[]string{"2", "1"}, "[1 2]"},
		{[]string{"1,2"}, "[1 2]"},
		{[]string{"2,2,1"}, "[1 2]"},
		{[]string{" 1 , 2 "}, "[1 2]"},
		{[]string{"all"}, "[1 2]"},
		{[]string{"all", "1"}, "[1 2]"},
	}
	for _, c := range ok {
		ids, msg := parseServerFilter(c.values, key)
		if msg != "" || fmt.Sprint(ids) != c.want {
			t.Errorf("parseServerFilter(%v) = %v, %q; ожидали %s", c.values, ids, msg, c.want)
		}
	}
	bad := []struct {
		values []string
		key    []int
		want   string
	}{
		{[]string{"3"}, key, "server: нет доступа"},
		{[]string{"999"}, key, "server: нет доступа"},
		{[]string{"1"}, []int{}, "server: нет доступа"}, // пустая область — ничего не расширяет
		{[]string{""}, key, "server: укажите id или all"},
		{[]string{"x"}, key, "server: укажите id или all"},
		{[]string{"0"}, key, "server: укажите id или all"},
		{[]string{"-1"}, key, "server: укажите id или all"},
		{[]string{"1,,2"}, key, "server: укажите id или all"},
	}
	for _, c := range bad {
		if ids, msg := parseServerFilter(c.values, c.key); ids != nil || msg != c.want {
			t.Errorf("parseServerFilter(%v, %v) = %v, %q; ожидали ошибку %q", c.values, c.key, ids, msg, c.want)
		}
	}
	if ids, msg := parseServerFilter([]string{"all"}, []int{}); msg != "" || len(ids) != 0 || ids == nil {
		t.Errorf("all на пустой области = %v, %q; ожидали []", ids, msg)
	}
}

func TestMonitorAPIServerFilter(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorNode(t, a, 2, "sg-1")
	addMonitorNode(t, a, 3, "us-1") // существует, но ключу не выдан
	execMonitor(t, a,
		`INSERT INTO master_keys (id, uuid, label, secret, device_limit, created_at) VALUES
			(1, 'u1', 'k1', x'01', 2, 1),
			(2, 'u2', 'k2', x'02', 3, 1),
			(3, 'u3', 'k3', x'03', 4, 1)`,
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1), (2, 2), (3, 1), (3, 2)",
		`INSERT INTO devices (id, master_id, device_id, platform, pub_key, auth_pub, created_at) VALUES
			(1, 1, 'd1', 'Android 14', 'P1', 'A1', 1),
			(2, 2, 'd2', 'iOS', 'P2', 'A2', 1),
			(3, 3, 'd3', '', 'P3', 'A3', 1)`,
	)
	addMonitorKey(t, a, 1, "awgk_filter", 1, 2)
	addMonitorKey(t, a, 2, "awgk_filter_empty") // ключ без серверов
	fixClock(t, fakeTS2)
	useFake(t, m, newPromFake(t, []int{1, 2, 3}, true))

	ids := func(t *testing.T, doc jsjson.Object) string {
		t.Helper()
		var out []int
		for _, s := range arr(field(doc, "servers")) {
			out = append(out, int(numAt(t, s, "id")))
		}
		return fmt.Sprint(out)
	}
	expect400 := func(t *testing.T, token, target, want string) {
		t.Helper()
		w, _ := monitorCall(t, a, token, target)
		if w.Code != 400 {
			t.Fatalf("%s: код %d (%s)", target, w.Code, w.Body)
		}
		if got := w.Body.String(); got != want {
			t.Errorf("%s: тело %s, ожидали %s", target, got, want)
		}
	}
	const (
		errAccess = `{"error":"server: нет доступа"}`
		errForm   = `{"error":"server: укажите id или all"}`
		errRange  = `{"error":"range: 1h, 6h, 24h, 7d или 30d"}`
	)

	// Без параметра — вся область ключа (это же и есть all).
	_, full := monitorCall(t, a, "awgk_filter", "/api/v1/monitor?range=1h")
	if got := jsonAt(t, full, "scope"); got != "[1,2]" {
		t.Errorf("без параметра scope = %s", got)
	}
	if got := ids(t, full); got != "[1 2]" {
		t.Errorf("без параметра servers = %s", got)
	}
	if n := numAt(t, full, "totals", "master_keys"); n != 3 {
		t.Errorf("без параметра master_keys = %v", n)
	}
	if n := numAt(t, full, "totals", "peers"); n != 3060 {
		t.Errorf("без параметра peers = %v", n)
	}
	if got := jsonAt(t, full, "series", "down_bps"); got != "[[1699996800,5000],[1700000060,5400]]" {
		t.Errorf("без параметра down_bps = %s", got)
	}

	t.Run("один id сужает всё", func(t *testing.T) {
		w, doc := monitorCall(t, a, "awgk_filter", "/api/v1/monitor?range=1h&server=1")
		if w.Code != 200 {
			t.Fatalf("код %d: %s", w.Code, w.Body)
		}
		if got := jsonAt(t, doc, "scope"); got != "[1]" {
			t.Errorf("scope = %s", got)
		}
		if got := ids(t, doc); got != "[1]" {
			t.Errorf("servers = %s", got)
		}
		if got := jsonAt(t, doc, "series", "down_bps"); got != "[[1699996800,2000],[1700000060,2200]]" {
			t.Errorf("down_bps = %s (ожидали только сервер 1)", got)
		}
		if n := numAt(t, doc, "totals", "peers"); n != 1030 {
			t.Errorf("peers = %v", n)
		}
		if n := numAt(t, doc, "totals", "peers_online"); n != 1031 {
			t.Errorf("peers_online = %v", n)
		}
		if n := numAt(t, doc, "traffic", "window", "rx_bytes"); n != 8000 {
			t.Errorf("window.rx_bytes = %v", n)
		}
		// Реестр — по области сервера: {1} входит только ключ, все серверы которого здесь.
		if n := numAt(t, doc, "totals", "master_keys"); n != 1 {
			t.Errorf("master_keys = %v", n)
		}
		if n := numAt(t, doc, "totals", "devices"); n != 1 {
			t.Errorf("devices = %v", n)
		}
		if n := numAt(t, doc, "totals", "device_slots"); n != 2 {
			t.Errorf("device_slots = %v", n)
		}
		if n := numAt(t, doc, "platforms", "known"); n != 1 {
			t.Errorf("platforms.known = %v", n)
		}
		if n := numAt(t, doc, "platforms", "total"); n != 1 {
			t.Errorf("platforms.total = %v", n)
		}
	})

	t.Run("повтор и запятые одинаковы", func(t *testing.T) {
		_, repeated := monitorCall(t, a, "awgk_filter", "/api/v1/monitor?range=1h&server=2&server=1")
		_, comma := monitorCall(t, a, "awgk_filter", "/api/v1/monitor?range=1h&server=1,2")
		r, c := string(jsjson.MustMarshal(repeated)), string(jsjson.MustMarshal(comma))
		if r != c {
			t.Errorf("разные ответы:\n%s\n%s", r, c)
		}
		if got := jsonAt(t, repeated, "scope"); got != "[1,2]" {
			t.Errorf("scope = %s", got)
		}
	})

	t.Run("all равен отсутствию параметра", func(t *testing.T) {
		w, _ := monitorCall(t, a, "awgk_filter", "/api/v1/monitor?range=1h&server=all")
		if w.Code != 200 {
			t.Fatalf("код %d: %s", w.Code, w.Body)
		}
		if got, want := w.Body.String(), string(jsjson.MustMarshal(full)); got != want {
			t.Errorf("all ≠ без параметра:\n%s\n%s", got, want)
		}
	})

	t.Run("чужой сервер не отбрасывается молча", func(t *testing.T) {
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=3", errAccess)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=999", errAccess)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=1,3", errAccess)
	})

	t.Run("пустое и мусорное значение", func(t *testing.T) {
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=", errForm)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=x", errForm)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=0", errForm)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=-1", errForm)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=1h&server=1,,2", errForm)
	})

	t.Run("пустая область ключа не расширяется", func(t *testing.T) {
		expect400(t, "awgk_filter_empty", "/api/v1/monitor?range=1h&server=1", errAccess)
		w, doc := monitorCall(t, a, "awgk_filter_empty", "/api/v1/monitor?range=1h&server=all")
		if w.Code != 200 {
			t.Fatalf("all на пустой области: код %d (%s)", w.Code, w.Body)
		}
		if got := jsonAt(t, doc, "scope"); got != "[]" {
			t.Errorf("scope = %s", got)
		}
		if got := jsonAt(t, doc, "servers"); got != "[]" {
			t.Errorf("servers = %s", got)
		}
		if n := numAt(t, doc, "totals", "master_keys"); n != 0 {
			t.Errorf("master_keys = %v", n)
		}
		if n := numAt(t, doc, "totals", "servers_total"); n != 0 {
			t.Errorf("servers_total = %v", n)
		}
	})

	t.Run("range проверяется первым", func(t *testing.T) {
		expect400(t, "awgk_filter", "/api/v1/monitor?range=2h&server=3", errRange)
		expect400(t, "awgk_filter", "/api/v1/monitor?range=2h&server=", errRange)
	})
}

// ── 4. Без Prometheus ───────────────────────────────────────────────────────

func TestMonitorAPINoPrometheus(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	// Сервер 2 тоже существует: иначе master {1,2} считался бы {1} и попал в итоги.
	addMonitorNode(t, a, 2, "sg-1")
	seedMonitorRegistry(t, a)
	addMonitorKey(t, a, 1, "awgk_noprom", 1)
	useNoPrometheus(t, m)

	w, doc := monitorCall(t, a, "awgk_noprom", "/api/v1/monitor?range=24h")
	if w.Code != 200 {
		t.Fatalf("без Prometheus код %d (ожидали 200): %s", w.Code, w.Body)
	}
	if v, _ := field(doc, "prometheus", "installed").(bool); v {
		t.Error("prometheus.installed = true")
	}
	if v, _ := field(doc, "prometheus", "degraded").(bool); v {
		t.Error("prometheus.degraded = true")
	}
	nullAt(t, doc, "prometheus", "source")
	nullAt(t, doc, "traffic")
	nullAt(t, doc, "series")
	nullAt(t, doc, "totals", "peers")
	nullAt(t, doc, "totals", "peers_online")
	if v, _ := field(doc, "partial").(bool); !v {
		t.Error("partial = false без Prometheus")
	}

	// Реестр и ОС от Prometheus не зависят.
	if n := numAt(t, doc, "totals", "master_keys"); n != 1 {
		t.Errorf("master_keys = %v", n)
	}
	if n := numAt(t, doc, "totals", "devices"); n != 1 {
		t.Errorf("devices = %v", n)
	}
	if n := numAt(t, doc, "totals", "device_slots"); n != 3 {
		t.Errorf("device_slots = %v", n)
	}
	if n := numAt(t, doc, "platforms", "known"); n != 1 {
		t.Errorf("platforms.known = %v", n)
	}
	if n := numAt(t, doc, "totals", "servers_total"); n != 1 {
		t.Errorf("servers_total = %v", n)
	}

	servers := arr(field(doc, "servers"))
	if len(servers) != 1 {
		t.Fatalf("servers: %d", len(servers))
	}
	if v, _ := field(servers[0], "ok").(bool); v {
		t.Error("ok = true без Prometheus")
	}
	for _, k := range []string{"up", "peers", "peers_online", "rx_bytes", "tx_bytes", "down_bps", "up_bps",
		"window_rx_bytes", "window_tx_bytes", "bandwidth_bits", "cpu_percent", "mem_percent", "cores",
		"mem_total_bytes", "heavy", "congested"} {
		nullAt(t, servers[0], k)
	}
	// version и name — из реестра серверов, не из Prometheus.
	if v, _ := field(servers[0], "version").(string); v != "0.5.5" {
		t.Errorf("version = %q", v)
	}
}

// ── 5. Degraded ─────────────────────────────────────────────────────────────

func TestMonitorAPIDegraded(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	seedMonitorRegistry(t, a)
	addMonitorKey(t, a, 1, "awgk_degraded", 1)

	fake := newPromFake(t, []int{1}, false)
	fake.fail = "awg_peers_online"
	useFake(t, m, fake)

	w, doc := monitorCall(t, a, "awgk_degraded", "/api/v1/monitor?range=1h")
	if w.Code != 200 {
		t.Fatalf("degraded код %d: %s", w.Code, w.Body)
	}
	if v, _ := field(doc, "prometheus", "degraded").(bool); !v {
		t.Error("degraded = false")
	}
	if v, _ := field(doc, "prometheus", "installed").(bool); !v {
		t.Error("installed = false")
	}
	if v, _ := field(doc, "partial").(bool); !v {
		t.Error("partial = false при degraded")
	}
	nullAt(t, doc, "series", "online_peers")
	nullAt(t, doc, "totals", "peers_online")
	if got := jsonAt(t, doc, "series", "down_bps"); got != "[[1699996800,1000],[1700000060,1200]]" {
		t.Errorf("down_bps = %s (не должен пострадать)", got)
	}
	if n := numAt(t, doc, "totals", "peers"); n != 30 {
		t.Errorf("totals.peers = %v", n)
	}
	if n := numAt(t, doc, "traffic", "window", "rx_bytes"); n != 7000 {
		t.Errorf("traffic.window.rx_bytes = %v", n)
	}
	servers := arr(field(doc, "servers"))
	if len(servers) != 1 {
		t.Fatalf("servers: %d", len(servers))
	}
	nullAt(t, servers[0], "peers_online")
	if v, _ := field(servers[0], "ok").(bool); !v {
		t.Error("ok = false: scrape_ok не падал")
	}
	if n := numAt(t, servers[0], "down_bps"); n != 1200 {
		t.Errorf("servers[0].down_bps = %v", n)
	}
}

// ── 6. Плохой range ─────────────────────────────────────────────────────────

func TestMonitorAPIBadRange(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorKey(t, a, 1, "awgk_range", 0)
	useNoPrometheus(t, m)

	w, _ := monitorCall(t, a, "awgk_range", "/api/v1/monitor?range=2h")
	if w.Code != 400 {
		t.Fatalf("код %d, ожидали 400", w.Code)
	}
	if got, want := w.Body.String(), `{"error":"range: 1h, 6h, 24h, 7d или 30d"}`; got != want {
		t.Errorf("тело:\n получили %s\n ожидали %s", got, want)
	}
}

// ── 7. Лимит ────────────────────────────────────────────────────────────────

func TestMonitorAPIRateLimit(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorKey(t, a, 1, "awgk_rate1", 0)
	addMonitorKey(t, a, 2, "awgk_rate2", 0)
	useNoPrometheus(t, m)

	for i := 0; i < 60; i++ {
		w, _ := monitorCall(t, a, "awgk_rate1", "/api/v1/monitor?range=6h")
		if w.Code != 200 {
			t.Fatalf("запрос %d: код %d (%s)", i+1, w.Code, w.Body)
		}
	}
	w, _ := monitorCall(t, a, "awgk_rate1", "/api/v1/monitor?range=6h")
	if w.Code != 429 {
		t.Fatalf("61-й запрос: код %d, ожидали 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q", got)
	}
	if got, want := w.Body.String(), `{"error":"Too many requests"}`; got != want {
		t.Errorf("тело 429: %s", got)
	}

	// Другой ключ не затронут.
	if w, _ := monitorCall(t, a, "awgk_rate2", "/api/v1/monitor?range=6h"); w.Code != 200 {
		t.Errorf("второй ключ: код %d (%s)", w.Code, w.Body)
	}
}

// ── 8. Кэш и single-flight ─────────────────────────────────────────────────

func TestMonitorAPICache(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	addMonitorKey(t, a, 1, "awgk_cache", 1)
	fixClock(t, fakeTS2)
	fake := newPromFake(t, []int{1}, false)
	fake.delay = 30 * time.Millisecond
	useFake(t, m, fake)

	type res struct {
		doc jsjson.Object
		err error
	}
	ch := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			doc, err := m.monitorDoc([]int{1}, "1h")
			ch <- res{doc, err}
		}()
	}
	r1, r2 := <-ch, <-ch
	if r1.err != nil || r2.err != nil {
		t.Fatalf("ошибки: %v %v", r1.err, r2.err)
	}
	if v, _ := r1.doc.Get2("generated_at").(int64); v != fakeTS2 {
		t.Errorf("generated_at = %v (%T)", r1.doc.Get2("generated_at"), r1.doc.Get2("generated_at"))
	}
	if r1.doc.Get2("generated_at") != r2.doc.Get2("generated_at") {
		t.Errorf("два параллельных запроса дали разный снимок: %v / %v",
			r1.doc.Get2("generated_at"), r2.doc.Get2("generated_at"))
	}
	if ranges, queries := fake.counts(); ranges != 11 || queries != 7 {
		// 11 range-запросов вкладки + (2 increase + 5 только монитора) мгновенных.
		t.Errorf("single-flight: query_range %d, query %d (ожидали 11/7)", ranges, queries)
	}

	// Тот же ключ кэша — без новых запросов; другое окно и другая область — новые.
	if _, err := m.monitorDoc([]int{1}, "1h"); err != nil {
		t.Fatal(err)
	}
	if ranges, queries := fake.counts(); ranges != 11 || queries != 7 {
		t.Errorf("кэш-хит сходил в Prometheus: %d/%d", ranges, queries)
	}
	if _, err := m.monitorDoc([]int{1}, "6h"); err != nil {
		t.Fatal(err)
	}
	if ranges, _ := fake.counts(); ranges != 22 {
		t.Errorf("другое окно: query_range %d, ожидали 22", ranges)
	}
	if _, err := m.monitorDoc([]int{0}, "1h"); err != nil {
		t.Fatal(err)
	}
	if ranges, _ := fake.counts(); ranges != 33 {
		t.Errorf("другая область: query_range %d, ожидали 33", ranges)
	}
}

// ── 9. Golden ───────────────────────────────────────────────────────────────

func TestMonitorAPIGolden(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	execMonitor(t, a,
		"INSERT INTO master_keys (id, uuid, label, secret, device_limit, created_at) VALUES (1, 'uuid-1', 'label-one', x'01', 5, 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1)",
		"INSERT INTO devices (id, master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, last_seen) VALUES (1, 1, 'dev-1', 'Dana', 'Android 14', 'WK1', 'AU1', 1, 111)",
	)
	addMonitorKey(t, a, 1, "awgk_golden", 1)
	fixClock(t, fakeTS2)
	useFake(t, m, newPromFake(t, []int{1}, false))

	w, doc := monitorCall(t, a, "awgk_golden", "/api/v1/monitor?range=1h")
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}

	// Хотя бы одно null и полный (не degraded) снимок — иначе фикстура не проверяет
	// «nulls present rather than omitted».
	servers := arr(field(doc, "servers"))
	if len(servers) != 1 {
		t.Fatalf("servers: %d", len(servers))
	}
	nullAt(t, servers[0], "congested")
	if v, _ := field(doc, "partial").(bool); v {
		t.Error("golden обязан быть полным: partial = true")
	}
	if v, _ := field(doc, "prometheus", "degraded").(bool); v {
		t.Error("golden обязан быть не degraded")
	}

	raw, err := os.ReadFile("testdata/monitor_api.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jsjson.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		gotB, _ := jsjson.MarshalIndent(got, "  ")
		wantB, _ := jsjson.MarshalIndent(want, "  ")
		t.Errorf("ответ разошёлся с testdata/monitor_api.json:\n получили:\n%s\n ожидали:\n%s", gotB, wantB)
	}
}

// ── 10. Приватность ────────────────────────────────────────────────────────

func TestMonitorAPIPrivacy(t *testing.T) {
	a, m := newMonitorTest(t)
	addMonitorNode(t, a, 1, "nl-1")
	execMonitor(t, a,
		`INSERT INTO master_keys (id, uuid, label, secret, device_limit, created_at) VALUES
			(1, 'uuid-SECRET-AAA', 'label-SECRET-BBB', x'0102', 4, 1)`,
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1)",
		`INSERT INTO devices (id, master_id, device_id, device_name, platform, pub_key, auth_pub, created_at, last_seen) VALUES
			(1, 1, 'device-SECRET-CCC', 'Dana-SECRET-DDD', 'Pixel 7 Pro', 'pub-SECRET-EEE', 'auth-SECRET-FFF', 1, 1700000000)`,
		`INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES
			(1, 1, 'peer-SECRET-GGG', '10.9.0.5', 'psk-SECRET-HHH')`,
	)
	addMonitorKey(t, a, 1, "awgk_privacy", 1)
	useNoPrometheus(t, m)

	w, doc := monitorCall(t, a, "awgk_privacy", "/api/v1/monitor?range=1h")
	if w.Code != 200 {
		t.Fatalf("код %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, forbidden := range []string{
		"SECRET", "Dana", "Pixel", "10.9.", "192.168.",
		`"label"`, `"uuid"`, `"device_name"`, `"device_id"`, `"last_seen"`, `"platform":`,
		"peer_pub", "psk", "auth_pub", "pub_key",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("в ответе есть запрещённое %q:\n%s", forbidden, body)
		}
	}
	// При этом гистограмма на месте — и только бакет, без сырой строки.
	if got := jsonAt(t, doc, "platforms", "by_os"); !strings.Contains(got, `{"os":"android","devices":1}`) {
		t.Errorf("android не посчитан: %s", got)
	}
	if n := numAt(t, doc, "totals", "devices"); n != 1 {
		t.Errorf("devices = %v", n)
	}
}
