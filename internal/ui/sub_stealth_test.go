package ui

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Публичный листенер подписки: ответ протокола (подписанный JSON) — только тому, кто знает
// секрет ссылки или ключ устройства. Остальным — 404 пустого Go-сервера.
func TestSubStealth(t *testing.T) {
	a := testApp(t, t.TempDir(), "on")
	_, signKey, _ := ed25519.GenerateKey(rand.Reader)
	s := &Sub{a: a, signKey: signKey, seenSigs: map[string]time.Time{}}
	s.registerLimit = newLimiter(30, time.Minute, func(string) {})
	s.failLimit = newLimiter(30, time.Minute, func(string) {})
	if err := s.initSchema(); err != nil {
		t.Fatal(err)
	}
	devPub, devKey, _ := ed25519.GenerateKey(rand.Reader)
	oldPub, oldKey, _ := ed25519.GenerateKey(rand.Reader)
	_, strangerKey, _ := ed25519.GenerateKey(rand.Reader)
	secret := make([]byte, 16)
	rand.Read(secret)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'k', ?, 1)", []any{secret}},
		{"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 1, 'd', 'WG', ?, 1)", []any{b64url.EncodeToString(devPub)}},
		{"INSERT INTO revoked_devices (id, auth_pub, revoked_at) VALUES (6, ?, 1)", []any{b64url.EncodeToString(oldPub)}},
	} {
		if _, err := a.db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	rt := s.publicRouter()
	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.5:4000"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w
	}
	signed := func(key ed25519.PrivateKey, dev int, method, path string, ts int64) map[string]string {
		sig := ed25519.Sign(key, []byte(requestSigString(method, path, ts, nil)))
		return map[string]string{"X-Sen-Device": strconv.Itoa(dev), "X-Sen-Ts": strconv.FormatInt(ts, 10),
			"X-Sen-Sig": b64url.EncodeToString(sig)}
	}
	stealthy := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 404 || w.Body.String() != "404 page not found\n" {
			t.Errorf("%s: ждали тихий 404, получили %d %q", name, w.Code, w.Body.String())
		}
	}
	protocol := func(name string, w *httptest.ResponseRecorder, code int) {
		t.Helper()
		if w.Code != code || !strings.Contains(w.Body.String(), `"sig"`) {
			t.Errorf("%s: ждали подписанный %d, получили %d %q", name, code, w.Code, w.Body.String())
		}
	}
	sec := `{"sub":"` + b64url.EncodeToString(secret) + `"}`
	other := make([]byte, 16)
	rand.Read(other)
	now := time.Now().Unix()

	stealthy("GET /", do("GET", "/", "", nil))
	stealthy("чужой путь", do("GET", "/sub/v1/nope", "", nil))
	stealthy("чужой метод", do("GET", "/sub/v1/peek", "", nil))
	stealthy("битый JSON", do("POST", "/sub/v1/peek", "{", nil))
	stealthy("peek без секрета", do("POST", "/sub/v1/peek", `{"sub":"x"}`, nil))
	stealthy("peek чужой секрет", do("POST", "/sub/v1/peek", `{"sub":"`+b64url.EncodeToString(other)+`"}`, nil))
	stealthy("register чужой секрет", do("POST", "/sub/v1/register", `{"sub":"`+b64url.EncodeToString(other)+`"}`, nil))
	stealthy("config без заголовков", do("GET", "/sub/v1/config", "", nil))
	stealthy("config чужой подписью", do("GET", "/sub/v1/config", "", signed(strangerKey, 5, "GET", "/sub/v1/config", now)))
	stealthy("config подобранный id", do("GET", "/sub/v1/config", "", signed(strangerKey, 999, "GET", "/sub/v1/config", now)))
	stealthy("config удалённого чужой подписью", do("GET", "/sub/v1/config", "", signed(strangerKey, 6, "GET", "/sub/v1/config", now)))

	protocol("peek своим секретом", do("POST", "/sub/v1/peek", sec, nil), 200)
	protocol("register своим секретом, без полей", do("POST", "/sub/v1/register", sec, nil), 400)
	protocol("удалённое устройство своей подписью", do("GET", "/sub/v1/config", "", signed(oldKey, 6, "GET", "/sub/v1/config", now)), 410)
	protocol("часы устройства уехали", do("GET", "/sub/v1/config", "", signed(devKey, 5, "GET", "/sub/v1/config", now-3600)), 401)

	s.frozen.Store(true)
	stealthy("перенос: чужой", do("POST", "/sub/v1/peek", `{"sub":"`+b64url.EncodeToString(other)+`"}`, nil))
	protocol("перенос: свой", do("POST", "/sub/v1/peek", sec, nil), 502)
	protocol("перенос: устройство", do("GET", "/sub/v1/devices", "", signed(devKey, 5, "GET", "/sub/v1/devices", now)), 502)
	s.frozen.Store(false)

	// Удаление устройства оставляет его ключ в revoked_devices: дальше — 410 только ему.
	s.forgetDeviceTx(s.deviceByID(a.db, 5))
	protocol("после удаления — своей подписью", do("GET", "/sub/v1/config", "", signed(devKey, 5, "GET", "/sub/v1/config", now+1)), 410)
	stealthy("после удаления — чужой", do("GET", "/sub/v1/config", "", signed(strangerKey, 5, "GET", "/sub/v1/config", now)))
}
