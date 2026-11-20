package agent

// Агент сам шлёт keepalive и переподключается, когда core рвёт установленное соединение;
// в журнал при этом попадают ровно две строки — потеря и восстановление связи.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func TestAgentKeepaliveAndReconnect(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	appendLog := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		mu.Lock()
		logs = append(logs, s)
		mu.Unlock()
	}
	hasLog := func(sub string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), logs...)
	}

	connected := make(chan int, 4)
	gotPing := make(chan struct{}, 4)
	var cmu sync.Mutex
	nconn := 0

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			return o
		}
		ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(jsjson.Object{{Key: "t", Val: "hello"}, {Key: "nonce", Val: "n0nce"}}))
		if read() == nil { // auth
			return
		}
		ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(jsjson.Object{{Key: "t", Val: "ready"}}))
		cmu.Lock()
		nconn++
		n := nconn
		cmu.Unlock()
		connected <- n
		if n > 1 {
			<-ctx.Done() // второе соединение держим до конца теста
			return
		}
		for {
			m := read()
			if m == nil {
				return
			}
			if m.Get2("t") == "ping" {
				gotPing <- struct{}{}
				ws.Write(ctx, websocket.MessageText, jsjson.MustMarshal(jsjson.Object{{Key: "t", Val: "pong"}}))
				ws.Close(websocket.StatusNormalClosure, "test drop")
				return
			}
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pin, err := spkiPin(srv.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, nodeKey, _ := ed25519.GenerateKey(rand.Reader)
	writeKey(t, filepath.Join(dir, "node.key"), nodeKey)
	_, ik, _ := ed25519.GenerateKey(rand.Reader)
	writeKey(t, filepath.Join(dir, "internal.key"), ik)
	a, err := New(Config{
		CoreHost: u.Hostname(), CorePort: port, CorePin: pin, NodeID: 7, Ctrl: "http://127.0.0.1:1",
		NodeKeyFile: filepath.Join(dir, "node.key"), InternalKey: filepath.Join(dir, "internal.key"),
		Version: "0.5.0", Logf: appendLog, Errorf: appendLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.keepalive, a.deadAfter = 40*time.Millisecond, 2*time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go a.Run(ctx)

	if n := <-connected; n != 1 {
		t.Fatalf("первое соединение — %d", n)
	}
	select {
	case <-gotPing:
	case <-ctx.Done():
		t.Fatal("агент не прислал свой keepalive")
	}
	select {
	case n := <-connected:
		if n != 2 {
			t.Fatalf("переподключение №%d", n)
		}
	case <-ctx.Done():
		t.Fatal("агент не переподключился после закрытия сокета")
	}

	for range 100 {
		if hasLog("соединение с core потеряно:") && hasLog("связь с core восстановлена") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasLog("соединение с core потеряно:") {
		t.Errorf("нет строки о потере связи: %q", snapshot())
	}
	if !hasLog("переподключение через 1 с") {
		t.Errorf("в строке о потере нет паузы до переподключения: %q", snapshot())
	}
	if !hasLog("связь с core восстановлена") {
		t.Errorf("нет строки о восстановлении связи: %q", snapshot())
	}
	if !hasLog("test drop") {
		t.Errorf("причина закрытия не попала в журнал: %q", snapshot())
	}
}
