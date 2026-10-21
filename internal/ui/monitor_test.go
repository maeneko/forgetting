package ui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/agent"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Панель без VPN, Prometheus — на ноде #0 (сервер, с которого панель переехала): вкладка
// читает его через агента, а сам он скрейпит метрики панели у агента, тот — у core.
func TestPrometheusOnNodeZero(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("нет openssl для сертификата хаба")
	}
	dir := t.TempDir()
	hubPort := freePort(t)
	t.Setenv("NODE_TLS_KEY_FILE", filepath.Join(dir, "node_tls.key"))
	t.Setenv("NODE_TLS_CERT_FILE", filepath.Join(dir, "node_tls.crt"))
	a := testApp(t, dir, "off")
	t.Setenv("NODE_PORT", strconv.Itoa(hubPort))
	var err error
	if a.nodes, err = newNodes(a); err != nil {
		t.Fatal(err)
	}
	if a.metrics, err = newMetrics(a); err != nil {
		t.Fatal(err)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyFile := filepath.Join(dir, "node.key")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	if _, err := a.db.Exec("INSERT INTO nodes (id, name, pub, created_at) VALUES (0, 'VPN', ?, 1)", b64url.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}

	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/metrics":
			io.WriteString(w, `{"up":true,"peers":{"total":2,"online":1}}`)
		case "/health":
			io.WriteString(w, `{"status":"ok","server":"VPN"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer ctrl.Close()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/ready":
			io.WriteString(w, "Prometheus Server is Ready.\n")
		case "/api/v1/query_range":
			io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"server":"VPN","server_id":"0"},"values":[[100,"3"],[115,"4"]]}]}}`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer prom.Close()

	a.nodes.Start()
	metricsPort := freePort(t)
	hashFile := filepath.Join(dir, "monitor_token.sha256")
	sum := sha256.Sum256([]byte("awgm_node"))
	os.WriteFile(hashFile, []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
	ag, err := agent.New(agent.Config{
		CoreHost: "127.0.0.1", CorePort: hubPort, CorePin: b64url.EncodeToString(a.nodes.pin), NodeID: 0,
		Ctrl: ctrl.URL, NodeKeyFile: keyFile, InternalKey: filepath.Join(dir, "none"),
		PromURL: prom.URL, MetricsPort: metricsPort, MonitorHash: hashFile,
		Logf: t.Logf, Errorf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go ag.Run(ctx)
	for !a.nodes.IsOnline(0) {
		if ctx.Err() != nil {
			t.Fatal("нода #0 не подключилась")
		}
		time.Sleep(20 * time.Millisecond)
	}

	m := newMonitor(a)
	m.promURL = "http://127.0.0.1:1" // своего Prometheus у панели нет
	src := m.source()
	if src == nil || src.local || src.node != 0 {
		t.Fatalf("Prometheus не найден на ноде #0: %+v", src)
	}
	s, err := m.rangeQuery(src, "awg_peers_online", 100, 115, 15)
	if err != nil || len(s.ids) != 1 || s.m[0].name != "VPN" || len(s.m[0].points) != 2 || s.m[0].points[1][1] != 4 {
		t.Fatalf("query_range через агента: %+v %v", s, err)
	}
	if h := m.host(m.target(src, nil)); h == nil {
		t.Error("вкладка не знает, на каком сервере Prometheus")
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(metricsPort)+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer awgm_node")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `awg_peers_online{server="VPN",server_id="0"} 1`) {
		t.Errorf("скрейп через агента: %d\n%s", resp.StatusCode, body)
	}
	// Второй скрейп сразу же — отказ: каждый опрашивает все серверы.
	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != 429 {
			t.Errorf("частый скрейп: %d", resp.StatusCode)
		}
	}
}
