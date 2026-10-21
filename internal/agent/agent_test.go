package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
)

func TestPathAllowed(t *testing.T) {
	for p, want := range map[any]bool{
		"/health":                           true,
		"/api/users":                        true,
		"/api/users/a?x=1":                  true,
		"/awg/upgrade":                      true,
		"/healthz":                          false,
		"/api":                              false,
		"/etc/passwd":                       false,
		"//evil.com/api/x":                  false,
		`/\evil.com/api/x`:                  false,
		"/api/../etc":                       false,
		"/api/%2e%2E/etc":                   false,
		"/api/./x":                          false,
		"/api/a b":                          false,
		"api/users":                         false,
		42:                                  false,
		"/api/" + strings.Repeat("a", 2048): false,
	} {
		if _, got := pathAllowed(p); got != want {
			t.Errorf("pathAllowed(%v) = %v, want %v", p, got, want)
		}
	}
}

func writeKey(t *testing.T, file string, priv ed25519.PrivateKey) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

type core struct {
	srv    *httptest.Server
	frames chan jsjson.Object // всё, что прислал агент
	script func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object)
}

func newCore(t *testing.T, script func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object)) *core {
	c := &core{frames: make(chan jsjson.Object, 16), script: script}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/node/v1" {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		read := func() jsjson.Object {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				return nil
			}
			v, _ := jsjson.Decode(raw)
			o, _ := v.(jsjson.Object)
			c.frames <- o
			return o
		}
		c.script(ctx, ws, read)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *core) pin(t *testing.T) string {
	p, err := spkiPin(c.srv.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (c *core) hostPort() (string, int) {
	u, _ := url.Parse(c.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}

func newAgent(t *testing.T, c *core, pin, ctrlURL string) (*Agent, string) {
	dir := t.TempDir()
	host, port := c.hostPort()
	_, ik, _ := ed25519.GenerateKey(rand.Reader)
	writeKey(t, filepath.Join(dir, "internal.key"), ik)
	envFile := filepath.Join(dir, "cli.env")
	os.WriteFile(envFile, []byte("CORE_HOST=x\nJOIN_SECRET=s3cret\nNODE_ID=7\n"), 0o600)
	a, err := New(Config{
		CoreHost: host, CorePort: port, CorePin: pin, NodeID: 7, Ctrl: ctrlURL,
		NodeKeyFile: filepath.Join(dir, "node.key"), InternalKey: filepath.Join(dir, "internal.key"),
		CLIEnvFile: envFile, JoinSecret: "s3cret", Version: "Beta 0.3.4",
		Logf: t.Logf, Errorf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, envFile
}

func send(ctx context.Context, ws *websocket.Conn, v any) {
	ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(v))
}

func TestHandshakeAndRequests(t *testing.T) {
	var internalPub ed25519.PublicKey
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !iauth.Verify(internalPub, tok, time.Now()) {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"Неверная авторизация"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/users":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			io.WriteString(w, `{"got":`+string(body)+`, "q":"`+r.URL.RawQuery+`"}`)
		case "/health":
			io.WriteString(w, "ok")
		default:
			w.WriteHeader(204)
		}
	}))
	defer ctrl.Close()

	results := make(chan jsjson.Object, 8)
	c := newCore(t, func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object) {
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n0nce"}})
		auth := read()
		if auth == nil {
			return
		}
		join, _ := auth.Get2("join").(jsjson.Object)
		pubRaw, _ := b64.DecodeString(join.Get2("pub").(string))
		sig, _ := b64.DecodeString(auth.Get2("sig").(string))
		if !ed25519.Verify(pubRaw, authMsg("n0nce", 7), sig) {
			ws.Close(4003, "unauthorized")
			return
		}
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "ready"}})
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "req"}, {Key: "id", Val: 1}, {Key: "method", Val: "post"},
			{Key: "path", Val: "/api/users?x=1"}, {Key: "body", Val: jsjson.Object{{Key: "name", Val: "alice"}}}})
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "req"}, {Key: "id", Val: 2}, {Key: "method", Val: "GET"}, {Key: "path", Val: "/etc/passwd"}})
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "req"}, {Key: "id", Val: 3}, {Key: "method", Val: "GET"}, {Key: "path", Val: "/health"}})
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "req"}, {Key: "id", Val: 4}, {Key: "method", Val: "DELETE"}, {Key: "path", Val: "/api/x"}})
		for range 4 {
			if r := read(); r != nil {
				results <- r
			}
		}
		ws.Close(websocket.StatusNormalClosure, "")
	})

	a, envFile := newAgent(t, c, c.pin(t), ctrl.URL)
	internalPub = a.internalKey.Public().(ed25519.PublicKey)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.connectOnce(ctx, nil) {
		t.Fatal("authFailed on a good handshake")
	}

	auth := <-c.frames
	want := `{"t":"auth","node_id":7,"sig":"` + auth.Get2("sig").(string) + `","join":{"secret":"s3cret","pub":"` + a.nodePubRaw + `"},"version":"Beta 0.3.4"}`
	if got := string(jsjson.MustMarshal(auth)); got != want {
		t.Errorf("auth frame:\n got %s\nwant %s", got, want)
	}

	byID := map[float64]string{}
	for range 4 {
		r := <-results
		byID[r.Get2("id").(float64)] = string(jsjson.MustMarshal(r))
	}
	for id, want := range map[float64]string{
		1: `{"t":"res","id":1,"status":201,"data":{"got":{"name":"alice"},"q":"x=1"}}`,
		2: `{"t":"res","id":2,"status":400,"data":{"error":"Запрос не разрешён агентом"}}`,
		3: `{"t":"res","id":3,"status":200,"data":"ok"}`,
		4: `{"t":"res","id":4,"status":204,"data":""}`,
	} {
		if byID[id] != want {
			t.Errorf("res %v:\n got %s\nwant %s", id, byID[id], want)
		}
	}

	// Секрет одноразовый: после ready он исчезает из памяти и из cli.env.
	if a.secret() != "" {
		t.Error("join secret still in memory")
	}
	if raw, _ := os.ReadFile(envFile); strings.Contains(string(raw), "JOIN_SECRET") || !strings.Contains(string(raw), "NODE_ID=7") {
		t.Errorf("cli.env after join: %q", raw)
	}
}

func TestWrongPinSendsNothing(t *testing.T) {
	c := newCore(t, func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object) {
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n"}})
		read()
	})
	a, _ := newAgent(t, c, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.connectOnce(ctx, nil) {
		t.Fatal("pin mismatch reported as auth failure")
	}
	select {
	case f := <-c.frames:
		t.Fatalf("agent sent %v to a core with a foreign certificate", f)
	default:
	}
}

func TestAuthRejected(t *testing.T) {
	c := newCore(t, func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object) {
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n"}})
		read()
		ws.Close(4003, "unauthorized")
	})
	a, envFile := newAgent(t, c, c.pin(t), "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !a.connectOnce(ctx, nil) {
		t.Fatal("4003 not reported as auth failure")
	}
	if raw, _ := os.ReadFile(envFile); !strings.Contains(string(raw), "JOIN_SECRET=s3cret") {
		t.Error("join secret dropped without a successful join")
	}
}

func TestNodeKeyPersists(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.key")
	k1, err := loadNodeKey(file, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := loadNodeKey(file, t.Logf)
	if !k1.Equal(k2) {
		t.Fatal("node key regenerated")
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("node.key mode %v", st.Mode().Perm())
	}
}

// Нода #0 — сервер, с которого переехала панель: 0 — значение, а не «не задан».
func TestConfigNodeZero(t *testing.T) {
	t.Setenv("CORE_HOST", "203.0.113.7")
	t.Setenv("CORE_PORT", "40500")
	t.Setenv("CORE_PIN", "pin")
	for id, ok := range map[string]bool{"0": true, "3": true, "": false, "-1": false, "x": false} {
		t.Setenv("NODE_ID", id)
		c, err := ConfigFromEnv()
		if (err == nil) != ok {
			t.Errorf("NODE_ID=%q: %v", id, err)
		}
		if ok && strconv.Itoa(c.NodeID) != id {
			t.Errorf("NODE_ID=%q → %d", id, c.NodeID)
		}
	}
}

// Панель переехала: core сообщает свой адрес в ready, агент записывает его в cli.env,
// подтверждает (core_ok) и с тех пор звонит туда первым, а адрес из join — запасной.
func TestLearnsCoreAddress(t *testing.T) {
	c := newCore(t, func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object) {
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n"}})
		read()
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "ready"}, {Key: "core", Val: "198.51.100.9:40500"}})
		read()
		ws.Close(websocket.StatusNormalClosure, "")
	})
	a, envFile := newAgent(t, c, c.pin(t), "http://127.0.0.1:1")
	host, port := c.hostPort()
	join := hostPort(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.connectOnce(ctx, nil)

	<-c.frames // auth
	if ok := <-c.frames; string(jsjson.MustMarshal(ok)) != `{"t":"core_ok","core":"198.51.100.9:40500"}` {
		t.Errorf("подтверждение: %s", jsjson.MustMarshal(ok))
	}
	if raw, _ := os.ReadFile(envFile); !strings.Contains(string(raw), "CORE_ENDPOINT=198.51.100.9:40500") {
		t.Errorf("cli.env: %q", raw)
	}
	if strings.Join(a.addrs, " ") != "198.51.100.9:40500 "+join || a.addr() != join {
		t.Errorf("адреса %v, текущий %s", a.addrs, a.addr())
	}
}

func TestBadCoreAddressIgnored(t *testing.T) {
	a := &Agent{c: Config{CoreHost: "203.0.113.1", CorePort: 40500, CLIEnvFile: filepath.Join(t.TempDir(), "cli.env"), Logf: t.Logf, Errorf: t.Logf}}
	a.setAddrs("")
	for _, bad := range []string{"", "x", "host:0", "host:99999", "a b:1", "host:1/x", "[::1]:x"} {
		if a.learnCore(bad) {
			t.Errorf("принят адрес %q", bad)
		}
	}
	if !a.learnCore("[2001:db8::1]:443") || a.addrs[0] != "[2001:db8::1]:443" {
		t.Errorf("IPv6: %v", a.addrs)
	}
}

// Записанный адрес не отвечает (старый сервер выключен или новый ещё не открыт) — следующая
// попытка идёт на адрес из join.
func TestFallsBackToJoinAddress(t *testing.T) {
	connected := make(chan struct{}, 1)
	c := newCore(t, func(ctx context.Context, ws *websocket.Conn, read func() jsjson.Object) {
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n"}})
		read()
		send(ctx, ws, jsjson.Object{{Key: "t", Val: "ready"}})
		connected <- struct{}{}
		<-ctx.Done()
	})
	a, _ := newAgent(t, c, c.pin(t), "http://127.0.0.1:1")
	a.c.CoreEndpoint = "127.0.0.1:1"
	a.setAddrs(a.c.CoreEndpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.Run(ctx)
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("агент не перешёл на адрес из join")
	}
}
