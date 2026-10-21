package ui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// После переноса старый сервер выключают, когда новый адрес знают все: хаб сообщает его
// нодам в ready и запоминает подтверждение (core_ok), клиенты получают его в endpoints.
// Вкладка «Перенос» показывает, сколько осталось.
func TestRetireReadiness(t *testing.T) {
	t.Setenv("SUB_HOST", "198.51.100.20")
	p := newMovePanel(t, "off", "40500")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, q := range []string{
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (0, 'Старый', 'x', 1)",
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (3, 'Frankfurt', '" + b64url.EncodeToString(pub) + "', 1)",
		"INSERT INTO nodes (id, name, created_at) VALUES (4, 'Ещё не подключена', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'Семья', x'01', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (2, 'u2', 'Не открыта', x'02', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at, revoked_at) VALUES (3, 'u3', 'Отозвана', x'03', 1, 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at, last_seen) VALUES (5, 1, 'a', 'W5', 'A5', 1, 2000)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at, last_seen) VALUES (6, 1, 'b', 'W6', 'A6', 1, 500)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (7, 1, 'c', 'W7', 'A7', 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at, last_seen) VALUES (8, 3, 'd', 'W8', 'A8', 1, 2000)",
	} {
		if _, err := p.a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	read := func() jsjson.Object {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := jsjson.Decode(raw)
		o, _ := v.(jsjson.Object)
		return o
	}
	write := func(o jsjson.Object) { ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(o)) }
	nonce := read().Get2("nonce").(string)
	write(jsjson.Object{{Key: "t", Val: "auth"}, {Key: "node_id", Val: 3},
		{Key: "sig", Val: b64url.EncodeToString(ed25519.Sign(priv, authMsg(nonce, 3)))}})
	if got := string(jsjson.MustMarshal(read())); got != `{"t":"ready","core":"198.51.100.20:40500"}` {
		t.Fatalf("ready: %s", got)
	}

	retire := func() jsjson.Object {
		_, o := p.call(t, p.a.migration.status, nil)
		r, _ := o.Get2("retire").(jsjson.Object)
		return r
	}
	if retire() != nil {
		t.Error("готовность показана панели, которая ничего не принимала")
	}
	t.Setenv("MOVED_IN_AT", "1000")
	if n := string(jsjson.MustMarshal(retire().Get2("nodes"))); n != `[{"id":3,"name":"Frankfurt","online":true,"ok":false}]` {
		t.Errorf("до core_ok: %s", n)
	}
	write(jsjson.Object{{Key: "t", Val: "core_ok"}, {Key: "core", Val: "203.0.113.66:1"}}) // не наш адрес — мимо
	write(jsjson.Object{{Key: "t", Val: "core_ok"}, {Key: "core", Val: "198.51.100.20:40500"}})
	var r jsjson.Object
	for range 50 {
		if r = retire(); strings.Contains(string(jsjson.MustMarshal(r.Get2("nodes"))), `"ok":true`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	want := `{"since":1000,"home":"198.51.100.20:40500","old":{"exists":true,"online":false},` +
		`"devices":{"total":3,"fresh":1},"unused_links":1,"nodes":[{"id":3,"name":"Frankfurt","online":true,"ok":true}]}`
	if got := string(jsjson.MustMarshal(r)); got != want {
		t.Errorf("готовность:\n got %s\nwant %s", got, want)
	}

	os.WriteFile(p.env, []byte("UI_PASS=x\nMOVED_IN_AT=1000\n"), 0o600)
	w := httptest.NewRecorder()
	p.a.migration.router().ServeHTTP(w, httptest.NewRequest("DELETE", "/retire", nil))
	if raw, _ := os.ReadFile(p.env); strings.Contains(string(raw), "MOVED_IN_AT") || retire() != nil {
		t.Errorf("отметка не убрана: %q", raw)
	}
}
