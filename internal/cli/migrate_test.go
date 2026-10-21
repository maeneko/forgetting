package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/migrate"
)

func decodeJoin(t *testing.T, s string) jsjson.Object {
	t.Helper()
	v, err := jsjson.Decode(fromB64url(strings.TrimPrefix(s, "awgjoin://")))
	o, ok := v.(jsjson.Object)
	if err != nil || !ok {
		t.Fatalf("join-строка не разбирается: %v", err)
	}
	return o
}

// export берёт пути и порты из окружения сервисов, как их видит awg-ui.
func TestWriteExportFromEnv(t *testing.T) {
	dir := t.TempDir()
	sign := filepath.Join(dir, "sign")
	os.WriteFile(sign, []byte("sign"), 0o600)
	dbFile := filepath.Join(dir, "ui.db")
	db, err := migrate.OpenDB(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE sub_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
	db.Exec("INSERT INTO sub_meta VALUES ('panel_id', 'abcdef123456')")
	db.Close()
	for k, v := range map[string]string{
		"AWG_ROOT": dir, "UI_DB_FILE": dbFile, "SUB_SIGN_KEY_FILE": sign, "AWGCTRL_PORT": "1", "SERVER_IP": "198.51.100.1",
		"SUB_PORT": "40123", "SUB_TLS": "on", "NODE_PORT": "40500", "UI_PORT": "33000",
		"SUB_TLS_KEY_FILE": filepath.Join(dir, "нет"), "SUB_TLS_CERT_FILE": filepath.Join(dir, "нет"),
		"NODE_TLS_KEY_FILE": filepath.Join(dir, "нет"), "NODE_TLS_CERT_FILE": filepath.Join(dir, "нет"),
	} {
		t.Setenv(k, v)
	}
	out := filepath.Join(dir, "bundle.tar.gz")
	if err := New().writeExport(out, dbFile); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Errorf("права архива %v — в нём ключ подписи", st.Mode().Perm())
	}
	f, _ := os.Open(out)
	b, err := migrate.Read(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Str("panel_id") != "abcdef123456" || b.Str("server_ip") != "198.51.100.1" || b.Str("server_name") != "VPN" ||
		b.Num("sub_port") != 40123 || b.Num("node_port") != 40500 || b.Num("ui_port") != 33000 || b.Meta.Get2("sub_tls") != true ||
		!b.Has["sub_sign.key"] || b.Has["sub_tls.key"] {
		t.Errorf("meta %s, has %v", jsjson.MustMarshal(b.Meta), b.Has)
	}
}

func TestRelayRoutes(t *testing.T) {
	j := decodeJoin(t, migrate.JoinString("203.0.113.7", 40500, strings.Repeat("A", 43), 0, []byte("0123456789abcdef"), 33100, 40123))
	t.Setenv("SUB_PORT", "40123")
	t.Setenv("NODE_PORT", "40400")
	t.Setenv("UI_PORT", "32000")
	if got, want := relayRoutes("203.0.113.7", 40500, j), "40123=203.0.113.7:40123,40400=203.0.113.7:40500,32000=203.0.113.7:33100"; got != want {
		t.Errorf("relay = %s, ожидали %s", got, want)
	}
	// Строка, выданная панелью заново (без ui/sub): подписка — на тот же порт, панель не пересылаем.
	t.Setenv("NODE_PORT", "")
	plain := decodeJoin(t, migrate.JoinString("2001:db8::7", 40500, strings.Repeat("A", 43), 0, []byte("0123456789abcdef"), 0, 0))
	if got, want := relayRoutes("2001:db8::7", 40500, plain), "40123=[2001:db8::7]:40123"; got != want {
		t.Errorf("relay = %s, ожидали %s", got, want)
	}
}
