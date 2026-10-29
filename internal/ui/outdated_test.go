package ui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// Нода старше MinNodeVersion (или без версии) с панелью не работает: хаб отклоняет её с 4026 и
// не считает подключённой, но версию запоминает — панель показывает, до чего обновлять.
func TestOutdatedNodeRejected(t *testing.T) {
	p := newMovePanel(t, "off", "40500")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := p.a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (3, 'Frankfurt', ?, 1)", b64url.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()

	connect := func(version string) (jsjson.Object, websocket.StatusCode) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.CloseNow()
		read := func() (jsjson.Object, error) {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				return nil, err
			}
			v, _ := jsjson.Decode(raw)
			o, _ := v.(jsjson.Object)
			return o, nil
		}
		hello, _ := read()
		auth := jsjson.Object{{Key: "t", Val: "auth"}, {Key: "node_id", Val: 3},
			{Key: "sig", Val: b64url.EncodeToString(ed25519.Sign(priv, authMsg(hello.Get2("nonce").(string), 3)))}}
		if version != "" {
			auth = append(auth, jsjson.KV{Key: "version", Val: version})
		}
		ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(auth))
		o, err := read()
		return o, websocket.CloseStatus(err)
	}

	for _, v := range []string{"0.4.9", ""} {
		if o, code := connect(v); code != closeOutdated {
			t.Fatalf("версия %q: ждали закрытия %d, получили %v / %v", v, closeOutdated, o, code)
		}
		p.a.nodes.mu.Lock()
		_, on := p.a.nodes.conns[3]
		p.a.nodes.mu.Unlock()
		if on {
			t.Fatalf("версия %q: нода считается подключённой", v)
		}
	}
	var stored string
	p.a.db.QueryRow("SELECT COALESCE(version, '') FROM nodes WHERE id = 3").Scan(&stored)
	if stored != "" { // последняя попытка — без версии
		t.Fatalf("версия в БД: %q", stored)
	}
	if o, code := connect(MinNodeVersion); o.Get2("t") != "ready" {
		t.Fatalf("актуальная версия: %v / %v", o, code)
	}
}

func TestCmpVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		sign int
	}{{"0.5.0", "0.5.0", 0}, {"0.4.9", "0.5.0", -1}, {"0.10.0", "0.9.9", 1}, {"0.5", "0.5.0", 0}, {"0.5.1-beta", "0.5.1", 0}, {"Beta 0.4.7", "0.5.0", -1}} {
		got := cmpVersion(c.a, c.b)
		if (got > 0) != (c.sign > 0) || (got < 0) != (c.sign < 0) {
			t.Errorf("cmpVersion(%q, %q) = %d", c.a, c.b, got)
		}
	}
}
