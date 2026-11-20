package ui

// Наблюдаемость хаба: после перезапуска нода offline до нового соединения, а упавшее
// соединение оставляет причину в OnOffline и в last_error у GET /ui/nodes.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// addNode — строка реестра с уже привязанным ключом (привязку не проверяем — она в hub_preauth_test).
func addNode(t *testing.T, p *movePanel, id int64, name string, priv ed25519.PrivateKey) {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	if _, err := p.a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (?, ?, ?, 1)",
		id, name, b64url.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
}

// dialNode — WebSocket + hello → auth → ready, как настоящий агент.
func dialNode(t *testing.T, srvURL string, id int64, priv ed25519.PrivateKey) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srvURL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	read := func() jsjson.Object {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("кадр от хаба: %v", err)
		}
		v, _ := jsjson.Decode(raw)
		o, _ := v.(jsjson.Object)
		return o
	}
	hello := read()
	ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(jsjson.Object{{Key: "t", Val: "auth"}, {Key: "node_id", Val: id},
		{Key: "sig", Val: b64url.EncodeToString(ed25519.Sign(priv, authMsg(hello.Get2("nonce").(string), id)))},
		{Key: "version", Val: MinNodeVersion}}))
	if ready := read(); ready.Get2("t") != "ready" {
		t.Fatalf("ждали ready, получили %v", ready)
	}
	return ws
}

func waitNodeOnline(n *Nodes, id int64, want bool) bool {
	for range 100 {
		if n.IsOnline(int(id)) == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func nodeInfo(t *testing.T, n *Nodes, id int) serverInfo {
	t.Helper()
	for _, s := range n.List() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("сервера #%d нет в List()", id)
	return serverInfo{}
}

// Перезапуск панели: соединений в памяти нет — все ноды offline, пока она не подключится снова.
func TestHubRestartNodeOfflineUntilReconnect(t *testing.T) {
	p := newMovePanel(t, "off", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	addNode(t, p, 3, "Frankfurt", priv)

	srv := httptest.NewServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()
	dialNode(t, srv.URL, 3, priv)
	if !waitNodeOnline(p.a.nodes, 3, true) {
		t.Fatal("нода не подключилась")
	}
	if si := nodeInfo(t, p.a.nodes, 3); !si.Online || si.ConnectedAt == nil || si.LastError != nil {
		t.Fatalf("на связи: %+v", si)
	}

	// Перезапуск: Stop закрывает соединения, newNodes на той же ui.db — пустая память.
	p.a.nodes.Stop()
	n2, err := newNodes(p.a)
	if err != nil {
		t.Fatal(err)
	}
	p.a.nodes = n2
	if si := nodeInfo(t, n2, 3); si.Online || si.ConnectedAt != nil || si.LastError != nil {
		t.Fatalf("после перезапуска нода не offline или помнит причину: %+v", si)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(n2.serveWS))
	defer srv2.Close()
	dialNode(t, srv2.URL, 3, priv)
	if !waitNodeOnline(n2, 3, true) {
		t.Fatal("нода не подключилась после перезапуска хаба")
	}
	if si := nodeInfo(t, n2, 3); si.ConnectedAt == nil || si.LastError != nil {
		t.Fatalf("после переподключения: %+v", si)
	}
}

// Оборванное соединение: OnOffline получает причину, а GET /ui/nodes — last_error.
func TestHubDropFiresOfflineWithReason(t *testing.T) {
	p := newMovePanel(t, "off", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	addNode(t, p, 4, "Riga", priv)

	offline := make(chan string, 1)
	p.a.nodes.OnOffline(func(id int, reason string) {
		if id == 4 {
			offline <- reason
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()
	ws := dialNode(t, srv.URL, 4, priv)
	if !waitNodeOnline(p.a.nodes, 4, true) {
		t.Fatal("нода не подключилась")
	}
	ws.Close(websocket.StatusNormalClosure, "")

	select {
	case reason := <-offline:
		if reason == "" {
			t.Error("OnOffline без причины")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnOffline не сработал")
	}
	if !waitNodeOnline(p.a.nodes, 4, false) {
		t.Fatal("нода осталась online после обрыва")
	}
	for range 100 {
		si := nodeInfo(t, p.a.nodes, 4)
		if si.Online {
			t.Fatal("нода снова online без соединения")
		}
		if s, _ := si.LastError.(string); s == "read error" {
			if si.ConnectedAt != nil {
				t.Errorf("у offline-ноды остался connected_at: %+v", si)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("last_error не выставлен: %+v", nodeInfo(t, p.a.nodes, 4))
}

// Без NODE_PORT хаб не слушает, но говорит об этом (раньше Start молча возвращался).
func TestStartWithoutNodePortLogsAndReturns(t *testing.T) {
	p := newMovePanel(t, "off", "")
	var logs []string
	old := hubLogf
	hubLogf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	defer func() { hubLogf = old }()

	p.a.nodes.Start() // не должно паниковать и не должно занять порт

	found := false
	for _, l := range logs {
		if strings.Contains(l, "hub выключен: NODE_PORT не задан") {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет сообщения о выключенном хабе: %q", logs)
	}
	if p.a.nodes.tls != nil || p.a.nodes.hubReady() {
		t.Error("хаб без NODE_PORT считает себя готовым")
	}
}

// Один файл идентичности без второго не достраивается: pin у нод сломался бы, а хаб об этом
// должен сказать, а не молча врать «нет связи».
func TestHubPartialTLSIdentityNotRegenerated(t *testing.T) {
	dir := t.TempDir()
	keyFile, crtFile := filepath.Join(dir, "node_tls.key"), filepath.Join(dir, "node_tls.crt")
	t.Setenv("NODE_TLS_KEY_FILE", keyFile)
	t.Setenv("NODE_TLS_CERT_FILE", crtFile)
	a := testApp(t, dir, "off")
	t.Setenv("NODE_PORT", "40511")
	if err := os.WriteFile(keyFile, []byte("ключ без сертификата"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := hubWarnf
	hubWarnf = func(string, ...any) {}
	defer func() { hubWarnf = old }()

	n, err := newNodes(a)
	if err != nil {
		t.Fatal(err)
	}
	if n.tls != nil || n.pin != nil || n.hubReady() {
		t.Fatal("хаб включился с половиной идентичности")
	}
	if raw, _ := os.ReadFile(keyFile); string(raw) != "ключ без сертификата" {
		t.Errorf("файл ключа перезаписан: %q", raw)
	}
	if _, err := os.Stat(crtFile); !os.IsNotExist(err) {
		t.Error("второй файл создан при живом первом")
	}
}
