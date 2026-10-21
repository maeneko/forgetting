package migrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func testDB(t *testing.T, file string) {
	t.Helper()
	db, err := OpenDB(file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE sub_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"INSERT INTO sub_meta VALUES ('panel_id', 'abcdef123456')",
		"CREATE TABLE nodes (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, pub TEXT, join_hash BLOB, join_expires INTEGER, created_at INTEGER NOT NULL, last_seen INTEGER, version TEXT)",
		"CREATE TABLE master_keys (id INTEGER PRIMARY KEY)",
		"INSERT INTO master_keys VALUES (1)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// Архив → раскладка: всё, что зашито в ссылки и у нод, едет как есть; база — целостным
// снимком, в котором новая панель заводит ноду #0.
func TestBundleRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	paths := func(dir string) Paths {
		return PathsFrom(func(env string) string { return filepath.Join(dir, strings.ToLower(env)) })
	}
	from, to := paths(src), paths(dst)
	testDB(t, from["ui.db"])
	for name, v := range map[string]string{"sub_sign.key": "sign", "sub_tls.key": "tlskey", "sub_tls.crt": "tlscrt", "node_tls.key": "nodekey"} {
		os.WriteFile(from[name], []byte(v), 0o600)
	}
	os.WriteFile(to["sub_sign.key"], []byte("прежний"), 0o600)

	db, _ := OpenDB(from["ui.db"])
	var buf bytes.Buffer
	err := Write(&buf, db, from, jsjson.Object{{Key: "server_ip", Val: "198.51.100.1"}, {Key: "sub_port", Val: 40123},
		{Key: "sub_tls", Val: true}, {Key: "node_port", Val: 40500}})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Str("panel_id") != "abcdef123456" || b.Num("sub_port") != 40123 || b.Has["node_tls.crt"] {
		t.Errorf("meta %s, has %v", jsjson.MustMarshal(b.Meta), b.Has)
	}
	if n, err := PanelData(to["ui.db"]); err != nil || n != 0 {
		t.Errorf("PanelData(нет файла) = %d, %v", n, err)
	}

	plan, err := NewPlan(b, func(string) int { return 0 }, 33000, "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	got := regexp.MustCompile(`"MOVED_IN_AT","\d+"`).ReplaceAllString(string(jsjson.MustMarshal(plan.Env)), `"MOVED_IN_AT","…"`)
	if got != `[["SUB_PORT","40123"],["NODE_PORT","40500"],["SUB_HOST","203.0.113.7"],["SUB_TLS","on"],["MOVED_IN_AT","…"],["RELAY_FROM","198.51.100.1"]]` {
		t.Errorf("env: %s", got)
	}
	if _, err := NewPlan(b, func(string) int { return 0 }, 40500, "h"); err == nil {
		t.Error("UI_PORT = порт хаба: ожидали ошибку")
	}

	if err := Install(b, to); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"sub_sign.key": "sign", "sub_tls.crt": "tlscrt", "node_tls.key": "nodekey"} {
		if raw, _ := os.ReadFile(to[name]); string(raw) != want {
			t.Errorf("%s: %q", name, raw)
		}
	}
	if baks, _ := filepath.Glob(to["sub_sign.key"] + ".bak-*"); len(baks) != 1 {
		t.Errorf("прежний ключ не отложен: %v", baks)
	}
	if n, _ := PanelData(to["ui.db"]); n != 1 {
		t.Errorf("PanelData(снимок) = %d", n)
	}
	if err := AddNodeZero(to["ui.db"], "VPN\n<script>", []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	db, _ = OpenDB(to["ui.db"])
	var name string
	db.QueryRow("SELECT name FROM nodes WHERE id = 0").Scan(&name)
	db.Close()
	if name != "VPN" {
		t.Errorf("нода #0: %q", name)
	}
	if err := AddNodeZero(to["ui.db"], "VPN", []byte("x")); err == nil {
		t.Error("вторая нода #0: ожидали ошибку")
	}
}

func TestReadRejectsForeignEntries(t *testing.T) {
	for name, entries := range map[string][]string{
		"лишний файл": {"meta.json", "ui.db", "../../etc/passwd"},
		"без базы":    {"meta.json"},
		"повтор":      {"meta.json", "ui.db", "ui.db"},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			data := []byte("x")
			if e == "meta.json" {
				data = []byte(`{"v":1}`)
			}
			tw.WriteHeader(&tar.Header{Name: e, Mode: 0o600, Size: int64(len(data))})
			tw.Write(data)
		}
		tw.Close()
		gz.Close()
		if b, err := Read(&buf); err == nil {
			b.Close()
			t.Errorf("%s: архив принят", name)
		}
	}
}

func TestJoinString(t *testing.T) {
	s := JoinString("203.0.113.7", 40500, strings.Repeat("A", 43), 0, []byte("0123456789abcdef"), 33100, 40123)
	raw, _ := b64url.DecodeString(strings.TrimPrefix(s, "awgjoin://"))
	// Поля сверх v1 — в конце: install.sh и старые CLI их не замечают.
	if want := `{"v":1,"h":"203.0.113.7","p":40500,"pin":"` + strings.Repeat("A", 43) + `","n":0,"s":"MDEyMzQ1Njc4OWFiY2RlZg","ui":33100,"sub":40123}`; string(raw) != want {
		t.Errorf("join: %s", raw)
	}
}

func TestCodeAndSeal(t *testing.T) {
	c := Code{Host: "2001:db8::7", Port: 33000, Key: NewKey()}
	got, err := ParseCode("  " + c.String() + "\n")
	if err != nil || got.Host != c.Host || got.Port != c.Port || !bytes.Equal(got.Key, c.Key) {
		t.Fatalf("ParseCode: %+v %v", got, err)
	}
	if u := got.URL(); u != "http://[2001:db8::7]:33000/migrate/v1/import" {
		t.Errorf("URL: %s", u)
	}
	for _, bad := range []string{"", "awgmove://", "awgjoin://e30", CodePrefix + "e30",
		Code{Host: "h/../x", Port: 1, Key: c.Key}.String(), Code{Host: "h", Port: 0, Key: c.Key}.String(), Code{Host: "h", Port: 1, Key: []byte("short")}.String()} {
		if _, err := ParseCode(bad); err == nil {
			t.Errorf("ParseCode(%q): ожидали ошибку", bad)
		}
	}

	box := SealRequest(c.Key, []byte("архив"))
	if plain, err := OpenRequest(c.Key, box); err != nil || string(plain) != "архив" {
		t.Errorf("OpenRequest: %q %v", plain, err)
	}
	if _, err := OpenResponse(c.Key, box); err == nil {
		t.Error("запрос открылся как ответ")
	}
	if _, err := OpenRequest(NewKey(), box); err == nil {
		t.Error("открылся чужим ключом")
	}
	box[len(box)-1] ^= 1
	if _, err := OpenRequest(c.Key, box); err == nil {
		t.Error("открылся повреждённый")
	}
}
