package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrefixGate(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(200)
	})
	allow := func(r *http.Request) bool { return r.Method == "POST" && r.URL.Path == "/migrate/v1/import" }
	h := prefixGate([]string{"s3cretPath", "0ldPanel_x"}, next, allow)

	cases := []struct {
		method, remote, path string
		code                 int
		seen, location       string
	}{
		{"GET", "203.0.113.5:4000", "/", 404, "", ""},
		{"GET", "203.0.113.5:4000", "/ui/brand", 404, "", ""},
		{"GET", "203.0.113.5:4000", "/s3cretPathX/", 404, "", ""},
		{"GET", "203.0.113.5:4000", "/s3cretPath", 301, "", "s3cretPath/"},
		{"GET", "203.0.113.5:4000", "/s3cretPath/", 200, "/", ""},
		{"GET", "203.0.113.5:4000", "/s3cretPath/assets/a.js", 200, "/assets/a.js", ""},
		{"GET", "203.0.113.5:4000", "/0ldPanel_x/api/v1/users", 200, "/api/v1/users", ""},
		{"POST", "203.0.113.5:4000", "/migrate/v1/import", 200, "/migrate/v1/import", ""},
		{"GET", "203.0.113.5:4000", "/migrate/v1/import", 404, "", ""},
		{"GET", "127.0.0.1:4000", "/metrics", 200, "/metrics", ""},
		{"GET", "[::1]:4000", "/", 200, "/", ""},
	}
	for _, c := range cases {
		got = ""
		r := httptest.NewRequest(c.method, c.path, nil)
		r.RemoteAddr = c.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code || got != c.seen || w.Header().Get("Location") != c.location {
			t.Errorf("%s %s от %s: %d, роутер видит %q, Location %q; ждали %d, %q, %q",
				c.method, c.path, c.remote, w.Code, got, w.Header().Get("Location"), c.code, c.seen, c.location)
		}
		if c.code == 404 && w.Body.String() != "404 page not found\n" {
			t.Errorf("%s: тело 404 не как у пустого Go-сервера: %q", c.path, w.Body.String())
		}
	}

	got = ""
	r := httptest.NewRequest("GET", "/ui/brand", nil)
	r.RemoteAddr = "203.0.113.5:4000"
	prefixGate(nil, next, nil).ServeHTTP(httptest.NewRecorder(), r)
	if got != "/ui/brand" {
		t.Errorf("без префикса запрос должен доходить до роутера как есть, дошёл %q", got)
	}
}
