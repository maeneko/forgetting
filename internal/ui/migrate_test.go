package ui

import (
	"bufio"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func testApp(t *testing.T, dir, local string) *App {
	t.Helper()
	t.Setenv("LOCAL_NODE", local)
	t.Setenv("NODE_PORT", "")
	db, err := openUIDB(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &App{db: db, baseDir: dir}
	if a.nodes, err = newNodes(a); err != nil {
		t.Fatal(err)
	}
	return a
}

// Перенос панели (`awg-control migrate`): та же ui.db на панели без VPN, старый сервер — нода
// #0. Клиент должен получить тот же конфиг байт в байт, вместе с rev, иначе он пересоздал бы
// туннель, а сервис на macOS — ещё и ключ.
func TestMigratedConfigIsByteIdentical(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	old := testApp(t, oldDir, "on")
	s := &Sub{a: old, host: "203.0.113.7", port: 40123}
	if err := s.initSchema(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'Семья', x'01', 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 0), (1, 1)",
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (1, 'Нода', 'pub', 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 1, 'd', 'WG', 'AUTH', 1)",
		"INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (5, 0, 'WG', '10.9.0.5', 'PSK0'), (5, 1, 'WG', '10.9.0.9', 'PSK1')",
		`INSERT INTO server_profiles (server_id, profile, updated_at) VALUES
			(0, '{"name":"VPN","endpoint":"198.51.100.1:47619","server_pub":"S0","dns":["1.1.1.1"],"mtu":1376,"gen":"3.1","awg":{"Jc":"4"}}', 1),
			(1, '{"name":"Нода","endpoint":"192.0.2.2:47619","server_pub":"S1","gen":"2"}', 1)`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dev := s.deviceByID(old.db, 5)
	before := jsjson.MustMarshal(s.buildConfig(dev, nil))
	if !strings.Contains(string(before), `"id":0`) || !strings.Contains(string(before), `"id":1`) {
		t.Fatalf("в конфиге до переноса нет обоих серверов: %s", before)
	}

	if _, err := old.db.Exec("VACUUM INTO ?", filepath.Join(newDir, "ui.db")); err != nil {
		t.Fatal(err)
	}
	// Как migrate import: нода #0 ещё не подключалась.
	pre := testApp(t, newDir, "off")
	if _, err := pre.db.Exec("INSERT INTO nodes (id, name, join_hash, join_expires, created_at) VALUES (0, 'VPN', x'00', 9999999999, 1)"); err != nil {
		t.Fatal(err)
	}
	moved := testApp(t, newDir, "off")
	s2 := &Sub{a: moved, host: "203.0.113.7", port: 40123}
	if err := s2.initSchema(); err != nil {
		t.Fatal(err)
	}
	if s2.panelID != s.panelID {
		t.Errorf("panel_id: %s → %s — пиры на старом сервере остались бы без хозяина", s.panelID, s2.panelID)
	}
	after := jsjson.MustMarshal(s2.buildConfig(s2.deviceByID(moved.db, 5), nil))
	if string(before) != string(after) {
		t.Errorf("конфиг изменился:\n было %s\nстало %s", before, after)
	}
	if !moved.nodes.Exists(0) || moved.nodes.IsOnline(0) || moved.nodes.DefaultID() != 0 {
		t.Errorf("нода #0: exists=%v online=%v default=%d", moved.nodes.Exists(0), moved.nodes.IsOnline(0), moved.nodes.DefaultID())
	}
	if ids := moved.nodes.IDs(); len(ids) != 2 || ids[0] != 0 || ids[1] != 1 {
		t.Errorf("IDs = %v", ids)
	}
	if r, _ := moved.nodes.CtrlFor(0)("GET", "/health", nil); r.Status != 502 {
		t.Errorf("нода #0 не на связи: %d, ожидали 502", r.Status)
	}
	if l := moved.nodes.List(); len(l) != 2 || l[0].Local || !l[0].Pending {
		t.Errorf("List: %+v", l)
	}
	// Без SUB_HOST адрес подписки — не endpoint ноды #0: это другая машина.
	s2.host = ""
	if h := s2.subHost(nil); h != "" {
		t.Errorf("subHost = %q, ожидали пусто", h)
	}

	// Свой VPN и нода #0 — один id: такую базу с LOCAL_NODE=on не открываем.
	t.Setenv("LOCAL_NODE", "on")
	if _, err := newNodes(&App{db: moved.db, baseDir: newDir}); err == nil {
		t.Error("LOCAL_NODE=on при ноде #0: ожидали ошибку")
	}
}

func TestParseProxyV1(t *testing.T) {
	for line, want := range map[string]string{
		"PROXY TCP4 192.0.2.1 198.51.100.2 51000 40123":  "192.0.2.1:51000",
		"PROXY TCP6 2001:db8::1 2001:db8::2 51000 40123": "[2001:db8::1]:51000",
		"PROXY UNKNOWN":                                  "unknown",
		"PROXY UNKNOWN ffff::1 ffff::2 1 2":              "unknown",
		"PROXY TCP4 2001:db8::1 198.51.100.2 1 2":        "",
		"PROXY TCP4 192.0.2.1 198.51.100.2 70000 1":      "",
		"PROXY TCP4 192.0.2.1 198.51.100.2 1":            "",
		"PROXY TCP5 192.0.2.1 198.51.100.2 1 2":          "",
		"GET / HTTP/1.1":                                 "",
		"PROXY TCP4 192.0.2.1 not-an-ip 1 2":             "",
		"PROXY  TCP4 192.0.2.1 198.51.100.2 51000 40123": "",
	} {
		addr, ok := parseProxyV1(line)
		got := ""
		switch {
		case ok && addr == nil:
			got = "unknown"
		case ok:
			got = addr.String()
		}
		if got != want {
			t.Errorf("%q → %q, ожидали %q", line, got, want)
		}
	}
}

// Заголовок PROXY — только от доверенного адреса; соединение с него без заголовка проходит как
// есть, а от остальных заголовок остаётся байтами потока.
func TestProxyListener(t *testing.T) {
	try := func(trusted bool, send string) (remote, data string) {
		t.Helper()
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		ln := newProxyListener(raw, map[string]bool{"127.0.0.1": trusted})
		cl, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		io.WriteString(cl, send)
		cl.(*net.TCPConn).CloseWrite()
		done := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); done <- c }()
		select {
		case c := <-done:
			defer c.Close()
			b, _ := io.ReadAll(bufio.NewReader(c))
			return c.RemoteAddr().String(), string(b)
		case <-time.After(3 * time.Second):
			t.Fatal("Accept не вернулся")
		}
		return "", ""
	}
	if r, d := try(true, "PROXY TCP4 192.0.2.1 198.51.100.2 51000 40123\r\nhello"); r != "192.0.2.1:51000" || d != "hello" {
		t.Errorf("доверенный с заголовком: %s %q", r, d)
	}
	if r, d := try(true, "GET / HTTP/1.1\r\n"); !strings.HasPrefix(r, "127.0.0.1:") || d != "GET / HTTP/1.1\r\n" {
		t.Errorf("доверенный без заголовка: %s %q", r, d)
	}
	hdr := "PROXY TCP4 192.0.2.1 198.51.100.2 51000 40123\r\nhello"
	if r, d := try(false, hdr); !strings.HasPrefix(r, "127.0.0.1:") || d != hdr {
		t.Errorf("чужой с заголовком: %s %q", r, d)
	}
}
