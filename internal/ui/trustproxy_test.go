package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// За reverse-proxy адрес клиента берётся из его заголовков, но только когда соединение пришло от
// самого прокси (loopback или TRUST_PROXY): напрямую подставить себе чужой IP нельзя.
func TestClientIP(t *testing.T) {
	nets := parseTrustProxy("10.0.0.0/8, 192.0.2.7, мусор")
	for _, c := range []struct {
		name, peer string
		xff        []string
		real, want string
	}{
		{"напрямую, заголовки игнорируются", "203.0.113.5", []string{"1.2.3.4"}, "1.2.3.4", "203.0.113.5"},
		{"nginx на этой машине", "127.0.0.1", []string{"203.0.113.5"}, "", "203.0.113.5"},
		{"IPv6 loopback", "::1", []string{"2001:db8::5"}, "", "2001:db8::5"},
		{"клиент дописал себе адрес слева", "127.0.0.1", []string{"6.6.6.6, 203.0.113.5"}, "", "203.0.113.5"},
		{"цепочка доверенных прокси", "10.1.1.1", []string{"203.0.113.5, 10.2.2.2", "192.0.2.7"}, "", "203.0.113.5"},
		{"только X-Real-IP", "127.0.0.1", nil, "203.0.113.9", "203.0.113.9"},
		{"прокси без заголовков", "127.0.0.1", nil, "", "127.0.0.1"},
		{"мусор в X-Forwarded-For", "127.0.0.1", []string{"unknown"}, "203.0.113.9", "203.0.113.9"},
		{"чужой прокси не доверен", "198.51.100.1", []string{"203.0.113.5"}, "", "198.51.100.1"},
	} {
		h := http.Header{}
		for _, v := range c.xff {
			h.Add("X-Forwarded-For", v)
		}
		if c.real != "" {
			h.Set("X-Real-IP", c.real)
		}
		if got := clientIP(c.peer, h, nets); got != c.want {
			t.Errorf("%s: %s, ожидали %s", c.name, got, c.want)
		}
	}
	if len(nets) != 2 {
		t.Errorf("TRUST_PROXY разобран в %d сетей", len(nets))
	}
}

// Каталог без слэша — относительный редирект: за прокси в подпапке абсолютный увёл бы из неё.
func TestStaticRelativeRedirect(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "dist", "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "dist", "assets", "index.html"), []byte("x"), 0o644)
	a := &App{baseDir: dir}
	w := httptest.NewRecorder()
	a.static(&Ctx{W: w, R: httptest.NewRequest("GET", "/assets", nil)})
	if w.Code != 301 || w.Header().Get("Location") != "assets/" {
		t.Errorf("%d %q", w.Code, w.Header().Get("Location"))
	}
}
