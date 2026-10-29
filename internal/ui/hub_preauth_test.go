package ui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/agent"
	"github.com/maeneko/forgetting/internal/jsjson"
)

// Хаб нод: WebSocket — только агенту с подписью в заголовках; остальным 404 пустого сервера.
// Старых агентов (без подписи) пускают, пока не все ноды подключались по-новому.
func TestHubPreauth(t *testing.T) {
	p := newMovePanel(t, "on", "")
	pub3, priv3, _ := ed25519.GenerateKey(rand.Reader)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := p.a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (3, 'Frankfurt', ?, 1)", b64url.EncodeToString(pub3)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hdr := func(id int64, key ed25519.PrivateKey, ts int64) http.Header {
		h := http.Header{}
		h.Set("X-Node-Id", strconv.FormatInt(id, 10))
		h.Set("X-Node-Ts", strconv.FormatInt(ts, 10))
		h.Set("X-Node-Sig", b64url.EncodeToString(ed25519.Sign(key, preauthMsg(id, ts))))
		return h
	}
	// dial — код ответа на апгрейд (101 — WebSocket принят) и первый кадр.
	dial := func(h http.Header) (int, *websocket.Conn) {
		ws, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			if resp == nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == 404 && string(body) != "404 page not found\n" {
				t.Errorf("тело 404 не как у пустого сервера: %q", body)
			}
			return resp.StatusCode, nil
		}
		return 101, ws
	}
	read := func(ws *websocket.Conn) jsjson.Object {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return nil
		}
		v, _ := jsjson.Decode(raw)
		o, _ := v.(jsjson.Object)
		return o
	}
	auth := func(ws *websocket.Conn, id int64, key ed25519.PrivateKey) jsjson.Object {
		nonce := read(ws).Get2("nonce").(string)
		ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(jsjson.Object{{Key: "t", Val: "auth"}, {Key: "node_id", Val: id},
			{Key: "sig", Val: b64url.EncodeToString(ed25519.Sign(key, authMsg(nonce, id)))}, {Key: "version", Val: MinNodeVersion}}))
		return read(ws)
	}
	now := time.Now().Unix()

	if code, _ := dial(hdr(3, stranger, now)); code != 404 {
		t.Errorf("чужая подпись: %d, ждали 404", code)
	}
	if code, _ := dial(hdr(99, stranger, now)); code != 404 {
		t.Errorf("несуществующая нода: %d, ждали 404", code)
	}
	if code, _ := dial(hdr(3, priv3, now-2*preauthWindow)); code != 404 {
		t.Errorf("подпись вне окна: %d, ждали 404", code)
	}

	// Переходный режим: нода 3 ещё не подключалась с подписью — старый агент проходит.
	code, ws := dial(nil)
	if code != 101 {
		t.Fatalf("старый агент в переходном режиме: %d", code)
	}
	if r := auth(ws, 3, priv3); r.Get2("t") != "ready" {
		t.Fatalf("старый агент: %v", r)
	}
	ws.CloseNow()

	// Новый агент: подпись в заголовках, затем прежний auth внутри.
	code, ws = dial(hdr(3, priv3, now))
	if code != 101 {
		t.Fatalf("новый агент: %d", code)
	}
	if r := auth(ws, 3, priv3); r.Get2("t") != "ready" {
		t.Fatalf("новый агент: %v", r)
	}
	ws.CloseNow()

	// Все ноды подключались с подписью — без неё больше не пускают.
	if code, _ := dial(nil); code != 404 {
		t.Errorf("без подписи после перехода: %d, ждали 404", code)
	}
	resp, err := http.Get(srv.URL + "/node/v1")
	if err != nil || resp.StatusCode != 404 {
		t.Errorf("обычный GET: %v %v", resp, err)
	}
	t.Setenv("HUB_LEGACY", "on")
	if code, _ := dial(nil); code != 101 {
		t.Errorf("HUB_LEGACY=on: %d", code)
	}
	t.Setenv("HUB_LEGACY", "")

	// Подпись одной ноды, auth от имени другой — отказ.
	pub5, priv5, _ := ed25519.GenerateKey(rand.Reader)
	p.a.db.Exec("INSERT INTO nodes (id, name, pub, created_at, preauth) VALUES (5, 'Riga', ?, 1, 1)", b64url.EncodeToString(pub5))
	code, ws = dial(hdr(3, priv3, now+1))
	if code != 101 {
		t.Fatalf("нода 3: %d", code)
	}
	if r := auth(ws, 5, priv5); r != nil {
		t.Errorf("auth чужой ноды после подписи ноды 3 принят: %v", r)
	}
	ws.CloseNow()
}

// Настоящий агент привязывается к настоящему хабу по строке join и подключается снова уже
// своим ключом — в обоих случаях с подписью до апгрейда.
func TestHubAgentEndToEnd(t *testing.T) {
	t.Setenv("HUB_LEGACY", "off")
	p := newMovePanel(t, "on", "")
	secretRaw := make([]byte, 16)
	rand.Read(secretRaw)
	secret := b64url.EncodeToString(secretRaw)
	h := sha256.Sum256(secretRaw)
	if _, err := p.a.db.Exec("INSERT INTO nodes (id, name, join_hash, join_expires, created_at) VALUES (7, 'Новая', ?, ?, 1)",
		h[:], time.Now().Unix()+600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(p.a.nodes.serveWS))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pin := certPin(t, srv.Certificate().Raw)

	dir := t.TempDir()
	_, ik, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ik)
	os.WriteFile(filepath.Join(dir, "internal.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	env := filepath.Join(dir, "cli.env")
	os.WriteFile(env, []byte("JOIN_SECRET="+secret+"\nNODE_ID=7\n"), 0o600)
	run := func() {
		a, err := agent.New(agent.Config{
			CoreHost: u.Hostname(), CorePort: port, CorePin: pin, NodeID: 7, Ctrl: "http://127.0.0.1:1",
			NodeKeyFile: filepath.Join(dir, "node.key"), InternalKey: filepath.Join(dir, "internal.key"),
			CLIEnvFile: env, JoinSecret: envValue(env, "JOIN_SECRET"), Version: MinNodeVersion,
			Logf: t.Logf, Errorf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go a.Run(ctx)
		for range 100 {
			if p.a.nodes.IsOnline(7) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("агент не подключился")
	}

	run() // привязка по секрету
	row := p.a.nodes.row(7)
	if !row.Pub.Valid {
		t.Fatal("ключ ноды не сохранён после привязки")
	}
	var pre int
	p.a.db.QueryRow("SELECT preauth FROM nodes WHERE id = 7").Scan(&pre)
	if pre != 1 {
		t.Error("подключение с подписью не отмечено (preauth)")
	}
	for range 100 { // прежнее соединение снимается при отмене контекста
		if !p.a.nodes.IsOnline(7) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(envValue(env, "JOIN_SECRET"), secret) {
		t.Error("секрет привязки не стёрт после первого подключения")
	}
	run() // повторное подключение — уже своим ключом
}

func certPin(t *testing.T, raw []byte) string {
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return b64url.EncodeToString(sum[:])
}

func envValue(file, key string) string {
	raw, _ := os.ReadFile(file)
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}
