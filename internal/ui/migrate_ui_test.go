package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/migrate"
)

type movePanel struct {
	a     *App
	s     *Sub
	env   string
	calls [][]string
}

func newMovePanel(t *testing.T, local, nodePort string) *movePanel {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCAL_NODE", local)
	t.Setenv("NODE_PORT", nodePort)
	t.Setenv("NODE_TLS_KEY_FILE", filepath.Join(dir, "node_tls.key"))
	t.Setenv("NODE_TLS_CERT_FILE", filepath.Join(dir, "node_tls.crt"))
	db, err := openUIDB(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &App{db: db, baseDir: dir, port: 34001}
	if a.nodes, err = newNodes(a); err != nil {
		t.Fatal(err)
	}
	a.sub = &Sub{a: a, host: "203.0.113.7", port: 40123, locks: map[int]*sync.Mutex{}}
	if err := a.sub.initSchema(); err != nil {
		t.Fatal(err)
	}
	p := &movePanel{a: a, s: a.sub, env: filepath.Join(dir, "cli.env")}
	a.migration = newMigration(a)
	a.migration.envFile = p.env
	a.migration.run = func(args ...string) { p.calls = append(p.calls, args) }
	return p
}

func (p *movePanel) call(t *testing.T, h handler, body jsjson.Object) (int, jsjson.Object) {
	t.Helper()
	w := httptest.NewRecorder()
	h(&Ctx{W: w, R: httptest.NewRequest("POST", "/", nil), Body: body})
	v, _ := jsjson.Decode(w.Body.Bytes())
	o, _ := v.(jsjson.Object)
	return w.Code, o
}

// Вкладка «Перенос» целиком: код новой панели → архив из старой по HTTP → нода #0, join и
// перезапуск. У клиента конфиг тот же, у старой панели подписка заморожена до join.
func TestMigrateBetweenPanels(t *testing.T) {
	old := newMovePanel(t, "on", "")
	os.WriteFile(filepath.Join(old.a.baseDir, "sub_sign.key"), []byte("ключ подписи"), 0o600)
	for _, q := range []string{
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'Семья', x'01', 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 0)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 1, 'd', 'WG', 'AUTH', 1)",
		"INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (5, 0, 'WG', '10.9.0.5', 'PSK0')",
		`INSERT INTO server_profiles (server_id, profile, updated_at) VALUES (0, '{"name":"VPN","endpoint":"198.51.100.1:47619","server_pub":"S0"}', 1)`,
	} {
		if _, err := old.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := jsjson.MustMarshal(old.s.buildConfig(old.s.deviceByID(old.a.db, 5), nil))

	fresh := newMovePanel(t, "off", "40500")
	m := fresh.a.migration
	if why := m.canAccept(); why != "" {
		t.Fatalf("чистая панель без VPN не принимает: %s", why)
	}
	if why := old.a.migration.canAccept(); why == "" {
		t.Error("панель со своим VPN принимает перенос")
	}
	if why := fresh.a.migration.canExport(); why == "" {
		t.Error("панель без VPN отдаёт перенос")
	}
	rt := newRouter()
	rt.Post("/migrate/v1/import", m.importHandler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, raw, _, _ := readBody(r, rt.bodyLimit)
		rt.routes[0].hs[0](&Ctx{W: w, R: r, Body: body, RawBody: raw})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	m.key, m.host, m.expires = migrate.NewKey(), "127.0.0.1", nowFn().Add(moveCodeTTL)

	// Чужой ключ: архив не принят, код живёт до пятой неудачи.
	wrong := migrate.Code{Host: "127.0.0.1", Port: port, Key: migrate.NewKey()}.String()
	if st, o := old.call(t, old.a.migration.export, jsjson.Object{{Key: "code", Val: wrong}}); st != 502 || !strings.Contains(o.Get2("error").(string), "Код не подходит") {
		t.Fatalf("чужой код: %d %v", st, o)
	}
	if old.s.frozen.Load() || m.key == nil {
		t.Fatal("после отказа подписка заморожена или код сгорел")
	}

	code := migrate.Code{Host: "127.0.0.1", Port: port, Key: m.key}.String()
	st, o := old.call(t, old.a.migration.export, jsjson.Object{{Key: "code", Val: code}})
	if st != 200 || o.Get2("target") != "http://127.0.0.1:"+u.Port() {
		t.Fatalf("export: %d %v", st, o)
	}
	if !old.s.frozen.Load() {
		t.Error("старая подписка не заморожена до join")
	}
	if len(old.calls) != 1 || old.calls[0][0] != "join" {
		t.Fatalf("старая панель: %v", old.calls)
	}
	j, _ := jsjson.Decode(fromB64url(strings.TrimPrefix(old.calls[0][1], "awgjoin://")))
	if field(j, "n") != 0.0 || field(j, "p") != 40500.0 || field(j, "sub") != 40123.0 || field(j, "ui") != 34001.0 {
		t.Errorf("join: %s", jsjson.MustMarshal(j))
	}
	if raw, _ := os.ReadFile(old.env); !strings.Contains(string(raw), "PANEL_MOVED=on") {
		t.Errorf("старый cli.env: %q", raw)
	}
	if len(fresh.calls) != 1 || fresh.calls[0][0] != "restart" || m.state != moveImported || m.key != nil {
		t.Errorf("новая панель: %v, state %q", fresh.calls, m.state)
	}
	raw, _ := os.ReadFile(fresh.env)
	for _, want := range []string{"SUB_PORT=40123", "NODE_PORT=40500", "SUB_HOST=127.0.0.1", "RELAY_FROM=127.0.0.1"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("новый cli.env без %s: %q", want, raw)
		}
	}
	if k, _ := os.ReadFile(filepath.Join(fresh.a.baseDir, "sub_sign.key")); string(k) != "ключ подписи" {
		t.Errorf("ключ подписи: %q", k)
	}

	// Новая панель после перезапуска: та же база + нода #0 → тот же конфиг.
	t.Setenv("NODE_PORT", "")
	db, err := openUIDB(filepath.Join(fresh.a.baseDir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	moved := &App{db: db, baseDir: fresh.a.baseDir}
	if moved.nodes, err = newNodes(moved); err != nil {
		t.Fatal(err)
	}
	s2 := &Sub{a: moved, host: "203.0.113.7", port: 40123}
	if err := s2.initSchema(); err != nil {
		t.Fatal(err)
	}
	if after := jsjson.MustMarshal(s2.buildConfig(s2.deviceByID(db, 5), nil)); string(after) != string(before) {
		t.Errorf("конфиг изменился:\n было %s\nстало %s", before, after)
	}
	if !moved.nodes.Exists(0) {
		t.Error("нет ноды #0")
	}

	// Повторный приём: кода уже нет.
	if st, _ := old.call(t, func(c *Ctx) bool { old.a.migration.state = moveIdle; return old.a.migration.export(c) },
		jsjson.Object{{Key: "code", Val: code}}); st != 502 {
		t.Errorf("второй перенос тем же кодом: %d", st)
	}
}
