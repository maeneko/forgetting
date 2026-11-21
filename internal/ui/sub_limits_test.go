package ui

// Ограничения мастер-ключей: срок, лимит трафика и блокировка. Харнесс как у monitor_api_test.go и
// health_api_test.go: настоящая ui.db, а awg-ctrl каждого сервера — фейк (CtrlFor подменяется
// через Sub.ctrlFor); ноды «в сети» — запись в conns, чтобы IsOnline не ходил в WSS.

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type fakeSubCtrl struct {
	mu    sync.Mutex
	peers map[int][]any    // сервер → пиры из GET /api/peers
	down  map[int]bool     // сервер не отвечает
	kept  map[int][]any    // последний keep из PUT /api/peers/sync
	added map[int][]string // ключи, которые панель просила добавить (POST /api/peers)
}

func newFakeSubCtrl() *fakeSubCtrl {
	return &fakeSubCtrl{peers: map[int][]any{}, down: map[int]bool{}, kept: map[int][]any{}, added: map[int][]string{}}
}

func (f *fakeSubCtrl) setPeers(id int, peers ...any) {
	f.mu.Lock()
	f.peers[id] = peers
	f.mu.Unlock()
}

func (f *fakeSubCtrl) setDown(id int) {
	f.mu.Lock()
	f.down[id] = true
	f.mu.Unlock()
}

func (f *fakeSubCtrl) keep(id int) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kept[id]
}

func (f *fakeSubCtrl) addedKeys(id int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.added[id]...)
}

func (f *fakeSubCtrl) ctrlFor(id int) Ctrl {
	return func(method, path string, body any) (*CtrlResp, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down[id] {
			return nil, errors.New("сервер не отвечает")
		}
		switch {
		case method == "GET" && path == "/api/peers":
			peers := f.peers[id]
			if peers == nil {
				peers = []any{}
			}
			return &CtrlResp{Status: 200, Data: jsjson.Object{{Key: "peers", Val: peers}}}, nil
		case method == "GET" && path == "/api/profile":
			return &CtrlResp{Status: 200, Data: jsjson.Object{
				{Key: "name", Val: fmt.Sprintf("s%d", id)}, {Key: "endpoint", Val: fmt.Sprintf("198.51.100.%d:47619", id+1)},
				{Key: "server_pub", Val: fmt.Sprintf("S%d", id)}, {Key: "gen", Val: "3.1"}}}, nil
		case method == "PUT" && path == "/api/peers/sync":
			o, _ := body.(jsjson.Object)
			keep := arr(o.Get2("keep"))
			f.kept[id] = keep
			return &CtrlResp{Status: 200, Data: jsjson.Object{{Key: "present", Val: keep}}}, nil
		case method == "POST" && path == "/api/peers":
			o, _ := body.(jsjson.Object)
			pub, _ := o.Get2("pub_key").(string)
			f.added[id] = append(f.added[id], pub)
			return &CtrlResp{Status: 200, Data: jsjson.Object{
				{Key: "ip", Val: fmt.Sprintf("10.9.0.%d", len(f.added[id])+1)}, {Key: "psk_key", Val: "PSK"}}}, nil
		}
		return &CtrlResp{Status: 404, Data: errObj("нет такого пути")}, nil
	}
}

// newLimitsTest — панель с ui.db и Sub; servers — сколько серверов (0 — локальный, дальше ноды),
// все в сети. Пиры серверов задаются фейком.
func newLimitsTest(t *testing.T, servers int) (*App, *Sub, *fakeSubCtrl) {
	t.Helper()
	a := testApp(t, t.TempDir(), "on")
	s := &Sub{a: a, host: "203.0.113.7", port: 40123,
		locks: map[int]*sync.Mutex{}, peerCalls: map[int]*peersCall{}, seenSigs: map[string]time.Time{}}
	if err := s.initSchema(); err != nil {
		t.Fatal(err)
	}
	fake := newFakeSubCtrl()
	s.ctrlFor = fake.ctrlFor
	for id := 1; id < servers; id++ {
		if _, err := a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (?, ?, 'pub', 1)", id, fmt.Sprintf("нода %d", id)); err != nil {
			t.Fatal(err)
		}
		a.nodes.mu.Lock()
		a.nodes.conns[int64(id)] = &nodeConn{} // только для IsOnline: запросы идут в фейк
		a.nodes.mu.Unlock()
	}
	return a, s, fake
}

func execLimits(t *testing.T, a *App, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// peer — пир в том виде, в каком его отдаёт awg-ctrl.
func peer(pub string, rx, tx float64) jsjson.Object {
	return jsjson.Object{{Key: "pub_key", Val: pub}, {Key: "rx", Val: rx}, {Key: "tx", Val: tx}}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// chain — ключ 1 с серверами 0..n-1, устройство 5 с пиром на каждом сервере.
func chain(t *testing.T, a *App, servers int, pub string) {
	t.Helper()
	execLimits(t, a,
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'Семья', x'01', 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 1, 'd', '"+pub+"', 'AUTH', 1)")
	for id := 0; id < servers; id++ {
		execLimits(t, a,
			fmt.Sprintf("INSERT INTO master_servers (master_id, server_id) VALUES (1, %d)", id),
			fmt.Sprintf("INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (5, %d, '%s', '10.9.0.5', 'PSK')", id, pub),
			fmt.Sprintf(`INSERT INTO server_profiles (server_id, profile, updated_at) VALUES
                (%d, '{"name":"s%d","endpoint":"198.51.100.%d:47619","server_pub":"S%d","gen":"3.1"}', 1)`, id, id, id+1, id))
	}
}

// ── Агрегация ───────────────────────────────────────────────────────────────

// Трафик ключа складывается по всем его серверам. Учёт идёт по устройству (device_peers), а не
// по pub_key пира: ключ на сервере мог ещё не догнать текущий (rekey), трафик всё равно его.
func TestMasterUsageAcrossServers(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	// Сервер 1 ещё не догнал новый ключ устройства: на нём стоит старый, и трафик идёт по нему.
	execLimits(t, a, "UPDATE device_peers SET peer_pub = 'OLD' WHERE device_id = 5 AND server_id = 1")
	fake.setPeers(0, peer("WG", 100, 50))
	fake.setPeers(1, peer("OLD", 200, 25))

	rx, tx, complete := s.masterUsage(s.masterByID(a.db, 1))
	if !complete || rx != 300 || tx != 75 {
		t.Fatalf("использование: rx=%d tx=%d complete=%v, ожидали 300/75/true", rx, tx, complete)
	}
	s.enforceMasters()
	m := s.masterRowByID(a.db, 1)
	if m.UsedRx != 300 || m.UsedTx != 75 || m.usedTotal() != 375 {
		t.Fatalf("в ui.db used_rx=%d used_tx=%d", m.UsedRx, m.UsedTx)
	}
	if got := field(s.masterOut(m), "usedBytes"); got != int64(375) {
		t.Fatalf("usedBytes = %v (%T)", got, got)
	}
}

// ── keepOf: пиры снимаются ──────────────────────────────────────────────────

// Заблокированный и просроченный ключи не отдают пиров в keep — sync снимает их с серверов,
// но сами ключи и устройства остаются в реестре.
func TestBlockedKeyExcludedFromKeep(t *testing.T) {
	a, s, _ := newLimitsTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	if !containsStr(s.keepOf(0), "WG") {
		t.Fatalf("до блокировки keep = %v", s.keepOf(0))
	}
	m := s.masterByID(a.db, 1)
	if _, err := s.blockMaster(m, ""); err != nil {
		t.Fatal(err)
	}
	if keep := s.keepOf(0); len(keep) != 0 {
		t.Fatalf("после ручной блокировки keep = %v", keep)
	}
	if out := s.masterOutByID(1); field(out, "blocked") != true || field(out, "blockReason") != blockManual {
		t.Fatalf("GET: %v", out)
	}
	if s.deviceByID(a.db, 5) == nil {
		t.Fatal("устройство удалено — блокировка не должна трогать реестр")
	}

	// Без ручной блокировки, но просрочен — тоже не в keep (по часам, без тика).
	execLimits(t, a, "UPDATE master_keys SET blocked_at = NULL, blocked_reason = NULL, expires_at = "+fmt.Sprint(now())+" WHERE id = 1")
	if keep := s.keepOf(0); len(keep) != 0 {
		t.Fatalf("просроченный ключ в keep = %v", keep)
	}
}

// ── config ──────────────────────────────────────────────────────────────────

// Клиент узнаёт причину блокировки из config: servers[] остаются на месте (пустой список — 502,
// который клиент считает временным), rev меняется при смене ограничения и не дёргается от
// счётчика used_bytes.
func TestConfigCarriesBlockReason(t *testing.T) {
	a, s, fake := newLimitsTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100 WHERE id = 1")
	fake.setPeers(0, peer("WG", 130, 0))
	dev := s.deviceByID(a.db, 5)

	before := s.buildConfig(dev, nil)
	if n := configServers(before); n != 1 {
		t.Fatalf("до блокировки серверов %d: %s", n, jsjson.MustMarshal(before))
	}
	if field(before, "blocked") != nil || field(before, "limit_bytes") != int64(100) {
		t.Fatalf("до блокировки: %s", jsjson.MustMarshal(before))
	}

	s.enforceMasters()
	after := s.buildConfig(dev, nil)
	if n := configServers(after); n != 1 {
		t.Fatalf("заблокированный ключ потерял servers[] (%d): %s", n, jsjson.MustMarshal(after))
	}
	if field(after, "blocked") != blockTraffic || field(after, "limit_bytes") != int64(100) || field(after, "used_bytes") != int64(130) {
		t.Fatalf("после блокировки: %s", jsjson.MustMarshal(after))
	}
	t.Logf("config заблокированного ключа: %s", jsjson.MustMarshal(after))
	if before.Get2("rev") == after.Get2("rev") {
		t.Fatal("rev не изменился при блокировке — клиент не узнает")
	}

	// Сведение снимает пира с сервера, но config продолжает отдавать сервер и причину:
	// самолечение syncPeers заблокированные ключи не чистит.
	fake.setPeers(0)
	s.reconcile(0)
	if keep := fake.keep(0); len(keep) != 0 {
		t.Fatalf("заблокированный ключ остался в keep: %v", keep)
	}
	after = s.buildConfig(s.deviceByID(a.db, 5), nil)
	if n := configServers(after); n != 1 {
		t.Fatalf("после сведения серверов %d: %s", n, jsjson.MustMarshal(after))
	}
	if field(after, "blocked") != blockTraffic {
		t.Fatalf("после сведения: %s", jsjson.MustMarshal(after))
	}

	// Счётчик трафика растёт — rev не должен меняться, иначе клиент пересоздаёт туннель.
	rev := after.Get2("rev")
	execLimits(t, a, "UPDATE master_keys SET used_rx = 140 WHERE id = 1")
	if again := s.buildConfig(s.deviceByID(a.db, 5), nil); again.Get2("rev") != rev {
		t.Fatalf("rev изменился от счётчика: %s", jsjson.MustMarshal(again))
	}

	// Ключ без ограничений — прежний конфиг, без новых полей (rev старых клиентов не меняется).
	execLimits(t, a,
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (2, 'u2', 'Без лимита', x'02', 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (2, 0)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (6, 2, 'd2', 'WG2', 'AUTH2', 1)",
		"INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (6, 0, 'WG2', '10.9.0.6', 'PSK')")
	plain := s.buildConfig(s.deviceByID(a.db, 6), nil)
	for _, k := range []string{"blocked", "expires_at", "limit_bytes", "used_bytes"} {
		if _, has := plain.Get(k); has {
			t.Fatalf("у ключа без ограничений появилось %q: %s", k, jsjson.MustMarshal(plain))
		}
	}
}

// ── Валидаторы и PATCH ─────────────────────────────────────────────────────

func TestMasterLimitValidators(t *testing.T) {
	for _, c := range []struct {
		v    any
		want bool
	}{
		{float64(0), true}, {float64(1700000000), true}, {float64(-1), false}, {float64(1.5), false}, {"5", false}, {nil, false},
	} {
		if _, ok := unixOK(c.v); ok != c.want {
			t.Errorf("unixOK(%v) = %v, ожидали %v", c.v, ok, c.want)
		}
		if _, ok := bytesOK(c.v); ok != c.want {
			t.Errorf("bytesOK(%v) = %v, ожидали %v", c.v, ok, c.want)
		}
	}

	a, s, _ := newLimitsTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	status := func(err error) int {
		t.Helper()
		var ae *apiError
		if err == nil {
			return 200
		}
		if !errors.As(err, &ae) {
			t.Fatalf("не apiError: %v", err)
		}
		return ae.status
	}
	for _, c := range []struct {
		name string
		body jsjson.Object
		want int
	}{
		{"срок строкой", jsjson.Object{{Key: "expires_at", Val: "завтра"}}, 400},
		{"срок отрицательный", jsjson.Object{{Key: "expires_at", Val: float64(-1)}}, 400},
		{"лимит дробный", jsjson.Object{{Key: "traffic_limit_bytes", Val: 1.5}}, 400},
		{"лимит отрицательный", jsjson.Object{{Key: "traffic_limit_bytes", Val: float64(-5)}}, 400},
		{"unblock не bool", jsjson.Object{{Key: "unblock", Val: "да"}}, 400},
	} {
		_, err := s.createMaster(c.body, nil)
		if got := status(err); got != c.want {
			t.Errorf("%s (POST): %d, ожидали %d", c.name, got, c.want)
		}
		_, err = s.updateMaster(s.masterByID(a.db, 1), c.body, nil)
		if got := status(err); got != c.want {
			t.Errorf("%s (PATCH): %d, ожидали %d", c.name, got, c.want)
		}
	}

	// Внешний API срок и лимит принимает, но unblock — только панель.
	if _, err := s.updateMaster(s.masterByID(a.db, 1), jsjson.Object{
		{Key: "expires_at", Val: float64(1800000000)}, {Key: "traffic_limit_bytes", Val: float64(1000)},
		{Key: "unblock", Val: true}}, nil); err != nil {
		t.Fatal(err)
	}
	m := s.masterRowByID(a.db, 1)
	if m.ExpiresAt.Int64 != 1800000000 || m.TrafficLimit.Int64 != 1000 {
		t.Fatalf("PATCH не сохранил ограничения: %v / %v", m.ExpiresAt, m.TrafficLimit)
	}
	// null очищает.
	if _, err := s.updateMaster(m, jsjson.Object{{Key: "expires_at", Val: nil}, {Key: "traffic_limit_bytes", Val: nil}}, nil); err != nil {
		t.Fatal(err)
	}
	if m = s.masterRowByID(a.db, 1); m.ExpiresAt.Valid || m.TrafficLimit.Valid {
		t.Fatalf("null не очистил: %v / %v", m.ExpiresAt, m.TrafficLimit)
	}
}

// ── Разблокировка ──────────────────────────────────────────────────────────

// Снятие блокировки возвращает пиров: links обнуляются, reconcile ставит ключ устройства на
// сервер заново.
func TestUnblockRestoresPeers(t *testing.T) {
	a, s, fake := newLimitsTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	m := s.masterByID(a.db, 1)
	if _, err := s.blockMaster(m, ""); err != nil {
		t.Fatal(err)
	}
	if keep := s.keepOf(0); len(keep) != 0 {
		t.Fatalf("заблокированный ключ в keep: %v", keep)
	}
	out := s.unblockMaster(s.masterByID(a.db, 1))
	if field(out, "blocked") != false || field(out, "blockReason") != nil {
		t.Fatalf("после разблокировки: %v", out)
	}
	if added := fake.addedKeys(0); !containsStr(added, "WG") {
		t.Fatalf("пир не поставлен заново: добавлено %v", added)
	}
	links := s.linksOf(a.db, 5)
	if len(links) != 1 || !links[0].PeerPub.Valid || links[0].PeerPub.String != "WG" {
		t.Fatalf("links после разблокировки: %+v", links)
	}
	if !containsStr(s.keepOf(0), "WG") {
		t.Fatalf("keep после разблокировки: %v", s.keepOf(0))
	}
	if got := field(out, "usedBytes"); got != int64(0) {
		t.Fatalf("usedBytes = %v", got)
	}
}

// ── Роуты панели ────────────────────────────────────────────────────────────

// POST /:id/block, /:id/unblock и unblock в PATCH — на своих местах, ответы несут поля ограничений.
func TestMasterBlockRoutes(t *testing.T) {
	a, s, _ := newLimitsTest(t, 1)
	fixClock(t, 1700000000)
	chain(t, a, 1, "WG")
	app := newRouter()
	app.Mount("/ui/masterkeys", s.masterKeysRouter())
	do := func(method, path, body string) (int, jsjson.Object) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		v, err := jsjson.Decode(w.Body.Bytes())
		if err != nil {
			t.Fatalf("%s %s: не JSON (%d): %s", method, path, w.Code, w.Body)
		}
		o, _ := v.(jsjson.Object)
		return w.Code, o
	}

	if code, out := do("POST", "/ui/masterkeys/1/block", `{"reason":"manual"}`); code != 200 || field(out, "blocked") != true || field(out, "blockReason") != blockManual {
		t.Fatalf("block: %d %v", code, out)
	}
	if code, out := do("POST", "/ui/masterkeys/1/block", `{"reason":"каприз"}`); code != 400 || field(out, "blocked") != nil {
		t.Fatalf("block с чужой причиной: %d %v", code, out)
	}
	if code, out := do("POST", "/ui/masterkeys/1/unblock", ""); code != 200 || field(out, "blocked") != false {
		t.Fatalf("unblock: %d %v", code, out)
	}
	// unblock в PATCH — панельный: срок и лимит меняются, блокировка снимается тем же запросом.
	do("POST", "/ui/masterkeys/1/block", `{"reason":"manual"}`)
	if code, out := do("PATCH", "/ui/masterkeys/1", `{"expires_at":1800000000,"traffic_limit_bytes":5000,"unblock":true}`); code != 200 ||
		field(out, "blocked") != false || field(out, "expiresAt") != float64(1800000000) || field(out, "trafficLimitBytes") != float64(5000) {
		t.Fatalf("PATCH unblock: %d %v", code, out)
	}
	if code, out := do("PATCH", "/ui/masterkeys/1", `{"expires_at":null,"traffic_limit_bytes":null}`); code != 200 || field(out, "expiresAt") != nil || field(out, "trafficLimitBytes") != nil {
		t.Fatalf("PATCH null: %d %v", code, out)
	}
}
