// Package agent — агент ноды (порт awg-agent/index.ts). Сам открывает исходящее
// WSS-соединение к core (awg-ui), доказывает свою личность подписью Ed25519 и исполняет
// присланные запросы на локальном awg-ctrl (127.0.0.1). Входящих портов у ноды нет,
// awg-ctrl остаётся закрытым от сети. Если на ноде стоит Prometheus (`awg-ctrl monitor install`),
// агент отдаёт ему метрики панели, взятые у core, и пропускает к нему запросы вкладки «Монитор».
// Протокол и формат join-строки — docs/node-protocol.md.
package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maeneko/forgetting/internal/envfile"
	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
)

var b64 = base64.RawURLEncoding

const (
	handshakeTimeout = 10 * time.Second
	deadAfter        = 60 * time.Second // core пингует чаще; тишина дольше — соединение мёртвое
	maxPayload       = 4 * 1024 * 1024
	ctrlTimeout      = 110 * time.Second
	maxBackoff       = 60 * time.Second
	closeAuthFailed  = 4003
	promTimeout      = 8 * time.Second // как у панели к своему Prometheus
	metricsTimeout   = 9 * time.Second // меньше scrape_timeout Prometheus (10 с по умолчанию)
)

type Config struct {
	CoreHost     string
	CorePort     int
	CoreEndpoint string // «host:port», который core сообщил сам (после переезда панели); пробуется первым
	CorePin      string // SHA-256 SPKI сертификата core, base64url
	NodeID       int
	Ctrl         string // http://127.0.0.1:PORT
	NodeKeyFile  string
	InternalKey  string
	CLIEnvFile   string
	JoinSecret   string // одноразовый, живёт до первого успешного подключения
	Version      string
	PromURL      string // Prometheus этой машины: к нему агент пропускает запросы панели (/prom/…)
	MetricsPort  int    // 127.0.0.1:порт, где Prometheus ноды скрейпит метрики панели; 0 — мониторинга на ноде нет
	MonitorHash  string // хэш токена этого Prometheus (его пишет `awg-ctrl monitor install`)
	Logf, Errorf func(format string, a ...any)
}

// envFromFile — значение KEY=… из .env (как в Node-версии: первая строка, обрезанная).
func envFromFile(file, key string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(.*)$`).FindSubmatch(raw)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// ConfigFromEnv собирает конфиг так же, как Node-агент. Каталог агента — рабочий каталог
// процесса (CLI запускает сервис с cwd = <root>/awg-agent): там node.key по умолчанию, а
// .env проекта — уровнем выше.
func ConfigFromEnv() (Config, error) {
	cwd, _ := os.Getwd()
	atoi := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); return n }
	// Нода №0 — сервер, с которого переехала панель: 0 здесь — значение, а не «не задано».
	nodeID, idErr := strconv.Atoi(strings.TrimSpace(os.Getenv("NODE_ID")))
	or := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return def
	}
	c := Config{
		CoreHost:     os.Getenv("CORE_HOST"),
		CorePort:     atoi("CORE_PORT"),
		CorePin:      os.Getenv("CORE_PIN"),
		CoreEndpoint: os.Getenv("CORE_ENDPOINT"),
		NodeID:       nodeID,
		Ctrl:         "http://127.0.0.1:" + or("AWGCTRL_PORT", "3005"),
		NodeKeyFile:  or("NODE_KEY_FILE", filepath.Join(cwd, "node.key")),
		InternalKey:  or("INTERNAL_AUTH_KEY_FILE", "/etc/amnezia/amneziawg/internal_auth_private.key"),
		CLIEnvFile:   os.Getenv("CLI_ENV_FILE"),
		JoinSecret:   os.Getenv("JOIN_SECRET"),
		Version:      os.Getenv("VERSION"),
		PromURL:      or("PROM_URL", "http://127.0.0.1:9090"),
		MetricsPort:  atoi("AGENT_METRICS_PORT"),
		MonitorHash:  or("MONITOR_HASH_FILE", "/etc/amnezia/amneziawg/monitor_token.sha256"),
	}
	if c.Version == "" {
		c.Version = envFromFile(filepath.Join(cwd, "..", ".env"), "VERSION")
	}
	if c.CoreHost == "" || c.CorePort == 0 || c.CorePin == "" || idErr != nil || c.NodeID < 0 {
		return c, errors.New("agent: не заданы CORE_HOST / CORE_PORT / CORE_PIN / NODE_ID — выполните `awg-ctrl join <ссылка>`")
	}
	return c, nil
}

type Agent struct {
	c           Config
	nodeKey     ed25519.PrivateKey
	nodePubRaw  string
	internalKey ed25519.PrivateKey // nil — запросы к awg-ctrl уйдут без токена и получат 401

	mu         sync.Mutex
	joinSecret string
	addrs      []string // куда звонить: адрес от core, затем из join (CORE_HOST:CORE_PORT)
	cur        int
	link       *coreLink // соединение после ready; nil — core сейчас недоступен

	http *http.Client
	prom *http.Client
}

// coreLink — запросы ноды к core (пока один: метрики панели для Prometheus ноды).
type coreLink struct {
	send    func(v any)
	done    <-chan struct{}
	mu      sync.Mutex
	seq     int64
	pending map[int64]chan reply
}

// loadNodeKey — личность ноды (Ed25519): создаётся один раз, приватная часть с ноды не уходит.
func loadNodeKey(file string, logf func(string, ...any)) (ed25519.PrivateKey, error) {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
		logf("agent: создан ключ ноды %s", file)
	}
	return iauth.LoadPrivate(file)
}

func New(c Config) (*Agent, error) {
	if c.Logf == nil {
		c.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stdout, f+"\n", a...) }
	}
	if c.Errorf == nil {
		c.Errorf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	}
	key, err := loadNodeKey(c.NodeKeyFile, c.Logf)
	if err != nil {
		return nil, fmt.Errorf("agent: ключ ноды %s: %w", c.NodeKeyFile, err)
	}
	a := &Agent{
		c: c, nodeKey: key, joinSecret: c.JoinSecret,
		nodePubRaw: b64.EncodeToString(key.Public().(ed25519.PublicKey)),
		// awg-ctrl на loopback: системный прокси (HTTPS_PROXY и т. п.) сюда не нужен.
		http: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: ctrlTimeout},
		prom: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: promTimeout},
	}
	a.setAddrs(c.CoreEndpoint)
	if ik, err := iauth.LoadPrivate(c.InternalKey); err == nil {
		a.internalKey = ik
	} else {
		c.Errorf("agent: приватный ключ внутренней авторизации не найден: %s", c.InternalKey)
	}
	return a, nil
}

// authMsg дублируется в awg-ui/nodes.ts.
func authMsg(nonce string, nodeID int) []byte {
	return []byte("awg-node-v1\n" + nonce + "\n" + strconv.Itoa(nodeID))
}

// ── Запросы core → awg-ctrl ────────────────────────────────────────────────

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true}

var rePath = regexp.MustCompile(`^/[\x21-\x7e]*$`)

// pathAllowed — агент не открытый прокси: только API самого awg-ctrl. Строже Node-версии
// там, где та полагалась на нормализацию WHATWG URL: обратный слэш и «//» в начале
// (смена хоста) отвергаются сразу, «.»/«..» — и в %2e-записи.
func pathAllowed(p any) (string, bool) {
	s, ok := p.(string)
	if !ok || len(s) > 2048 || !rePath.MatchString(s) || strings.HasPrefix(s, "//") || strings.Contains(s, `\`) {
		return "", false
	}
	pathname := s
	if i := strings.IndexAny(pathname, "?#"); i >= 0 {
		pathname = pathname[:i]
	}
	for _, seg := range strings.Split(pathname, "/") {
		switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(seg, "%2e", "."), "%2E", ".")) {
		case ".", "..":
			return "", false
		}
	}
	if pathname == "/health" || strings.HasPrefix(pathname, "/api/") || strings.HasPrefix(pathname, "/awg/") {
		return s, true
	}
	return "", false
}

type reply struct {
	status int
	data   any
}

func errReply(status int, msg string) reply {
	return reply{status, jsjson.Object{{Key: "error", Val: msg}}}
}

// execute выполняет запрос core на локальном awg-ctrl (или, для /prom/…, на Prometheus).
func (a *Agent) execute(m jsjson.Object) reply {
	method, _ := m.Get2("method").(string)
	method = strings.ToUpper(method)
	if s, _ := m.Get2("path").(string); strings.HasPrefix(s, "/prom/") {
		return a.execProm(method, s)
	}
	p, ok := pathAllowed(m.Get2("path"))
	if !methods[method] || !ok {
		return errReply(400, "Запрос не разрешён агентом")
	}
	var body io.Reader
	if v, has := m.Get("body"); has && v != nil {
		raw, err := jsjson.Marshal(v)
		if err != nil {
			return errReply(400, "Запрос не разрешён агентом")
		}
		body = bytes.NewReader(raw)
	}
	u, err := url.Parse(a.c.Ctrl + p)
	if err != nil {
		return errReply(400, "Запрос не разрешён агентом")
	}
	u.Fragment = ""
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return errReply(400, "Запрос не разрешён агентом")
	}
	req.Header.Set("Content-Type", "application/json")
	if a.internalKey != nil {
		req.Header.Set("Authorization", "Bearer "+iauth.Mint(a.internalKey, "awg-agent", time.Now()))
	}
	return do(a.http, req, "awg-ctrl недоступен")
}

// do — ответ как его разобрал бы axios: JSON — значением, иначе строкой (пустое тело — "").
func do(c *http.Client, req *http.Request, down string) reply {
	resp, err := c.Do(req)
	if err != nil {
		return errReply(502, down)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return errReply(502, down)
	}
	if len(raw) == 0 {
		return reply{resp.StatusCode, ""}
	}
	if v, err := jsjson.Decode(raw); err == nil {
		return reply{resp.StatusCode, v}
	}
	return reply{resp.StatusCode, string(raw)}
}

// promPaths — всё, что панель читает у Prometheus: готовность и фиксированные запросы вкладки.
var promPaths = map[string]bool{"/prom/-/ready": true, "/prom/api/v1/query": true, "/prom/api/v1/query_range": true}

// promAllowed — запрос панели к Prometheus этой машины: только GET и только пути из promPaths.
func promAllowed(method string, p any) (string, bool) {
	s, ok := p.(string)
	if method != "GET" || !ok || len(s) > 8192 || !rePath.MatchString(s) {
		return "", false
	}
	pathname, query, _ := strings.Cut(s, "?")
	if !promPaths[pathname] || strings.Contains(query, "#") {
		return "", false
	}
	return strings.TrimPrefix(s, "/prom"), true
}

// execProm — Prometheus ноды для вкладки «Монитор» панели. Пропускаем, только если он
// скрейпит панель через этого агента (AGENT_METRICS_PORT): чужой Prometheus на машине
// панели не отдаём, да и метрик панели в нём нет.
func (a *Agent) execProm(method string, p any) reply {
	if a.c.MetricsPort == 0 {
		return errReply(404, "Prometheus на этой ноде не подключён к панели")
	}
	rest, ok := promAllowed(method, p)
	if !ok {
		return errReply(400, "Запрос не разрешён агентом")
	}
	req, err := http.NewRequest("GET", strings.TrimSuffix(a.c.PromURL, "/")+rest, nil)
	if err != nil {
		return errReply(400, "Запрос не разрешён агентом")
	}
	return do(a.prom, req, "Prometheus не отвечает")
}

// ── Соединение с core ──────────────────────────────────────────────────────

func spkiPin(rawCert []byte) (string, error) {
	cert, err := x509.ParseCertificate(rawCert)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return b64.EncodeToString(sum[:]), nil
}

var errPin = errors.New("сертификат core не совпал с pin — соединение отклонено")

// tlsConfig — сертификат core самоподписанный: цепочку не проверяем, доверие даёт pin.
// Проверка внутри TLS-рукопожатия: секрет и подпись уходят только тому, чей сертификат
// совпал с pin из join-строки.
func (a *Agent) tlsConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errPin
			}
			got, err := spkiPin(rawCerts[0])
			if err != nil || got != a.c.CorePin {
				return errPin
			}
			return nil
		},
	}
}

// forgetJoinSecret — после первого успешного входа секрет больше не нужен, убираем с диска.
func (a *Agent) forgetJoinSecret() {
	a.mu.Lock()
	a.joinSecret = ""
	a.mu.Unlock()
	os.Unsetenv("JOIN_SECRET")
	if a.c.CLIEnvFile == "" {
		return
	}
	raw, err := os.ReadFile(a.c.CLIEnvFile)
	if err != nil {
		return // не критично: секрет одноразовый и с истекающим сроком
	}
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(l, "JOIN_SECRET=") {
			kept = append(kept, l)
		}
	}
	os.WriteFile(a.c.CLIEnvFile, []byte(strings.Join(kept, "\n")), 0o600)
}

// setAddrs — сначала адрес, который сообщил core, потом адрес из join: как endpoints у клиента
// sen://. Чужой сервер по нему не подменить — доверие даёт pin, а не адрес.
func (a *Agent) setAddrs(learned string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addrs = nil
	for _, s := range []string{learned, hostPort(a.c.CoreHost, a.c.CorePort)} {
		if s != "" && (len(a.addrs) == 0 || a.addrs[0] != s) {
			a.addrs = append(a.addrs, s)
		}
	}
	a.cur = 0
}

func (a *Agent) addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addrs[a.cur%len(a.addrs)]
}

// next — не дозвонились (или core не принял): следующий адрес.
func (a *Agent) next() {
	a.mu.Lock()
	a.cur = (a.cur + 1) % len(a.addrs)
	a.mu.Unlock()
}

// learnCore — core сообщил свой адрес (панель переехала, старый сервер скоро выключат):
// запоминаем в cli.env, чтобы после перезапуска звонить сразу туда. false — не запомнили.
func (a *Agent) learnCore(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if p, perr := strconv.Atoi(port); err != nil || perr != nil || p <= 0 || p > 65535 || !reCoreHost.MatchString(host) {
		return false
	}
	addr = hostPort(host, mustAtoi(port))
	if addr == a.c.CoreEndpoint {
		return true
	}
	if a.c.CLIEnvFile == "" {
		return false
	}
	if err := envfile.Set(a.c.CLIEnvFile, [][2]string{{"CORE_ENDPOINT", addr}}); err != nil {
		a.c.Errorf("agent: адрес core не сохранён: %v", err)
		return false
	}
	a.c.CoreEndpoint = addr
	a.mu.Lock()
	cur := a.addrs[a.cur%len(a.addrs)]
	a.mu.Unlock()
	a.setAddrs(addr)
	a.mu.Lock()
	for i, s := range a.addrs { // текущее соединение не рвём: адрес пригодится при следующем
		if s == cur {
			a.cur = i
		}
	}
	a.mu.Unlock()
	a.c.Logf("agent: core теперь доступен и по %s", addr)
	return true
}

var reCoreHost = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)

func mustAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

func (a *Agent) secret() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.joinSecret
}

// connectOnce держит одно соединение до его закрытия. authFailed — core закрыл с 4003.
func (a *Agent) connectOnce(ctx context.Context, onReady func()) (authFailed bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var dead *time.Timer
	var deadMu sync.Mutex
	armDead := func() {
		deadMu.Lock()
		defer deadMu.Unlock()
		if dead != nil {
			dead.Stop()
		}
		dead = time.AfterFunc(deadAfter, cancel)
	}
	defer func() {
		deadMu.Lock()
		if dead != nil {
			dead.Stop()
		}
		deadMu.Unlock()
	}()

	tr := &http.Transport{Proxy: nil, TLSClientConfig: a.tlsConfig(), TLSHandshakeTimeout: handshakeTimeout}
	dctx, dcancel := context.WithTimeout(ctx, handshakeTimeout)
	ws, _, err := websocket.Dial(dctx, fmt.Sprintf("wss://%s/node/v1", a.addr()), &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: tr},
		CompressionMode: websocket.CompressionDisabled,
		OnPingReceived:  func(context.Context, []byte) bool { armDead(); return true },
	})
	dcancel()
	if err != nil {
		if errors.Is(err, errPin) {
			a.c.Errorf("agent: %v", errPin)
		} else {
			a.c.Errorf("agent: %v", err)
		}
		return false
	}
	defer ws.CloseNow()
	defer a.setLink(nil)
	ws.SetReadLimit(maxPayload)

	send := func(v any) {
		if b, err := jsjson.Marshal(v); err == nil {
			ws.Write(ctx, websocket.MessageText, b)
		}
	}

	ready := false
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			code := websocket.CloseStatus(err)
			if !ready && code == -1 && ctx.Err() == nil {
				a.c.Errorf("agent: %v", err)
			}
			if code == closeAuthFailed {
				a.c.Errorf("agent: core отклонил ноду (неверный ключ, просроченная или использованная ссылка)")
			}
			return code == closeAuthFailed
		}
		v, err := jsjson.Decode(raw)
		if err != nil {
			return false
		}
		m, _ := v.(jsjson.Object)
		switch t, _ := m.Get2("t").(string); {
		case t == "hello":
			nonce, ok := m.Get2("nonce").(string)
			if !ok {
				continue
			}
			auth := jsjson.Object{
				{Key: "t", Val: "auth"}, {Key: "node_id", Val: a.c.NodeID},
				{Key: "sig", Val: b64.EncodeToString(ed25519.Sign(a.nodeKey, authMsg(nonce, a.c.NodeID)))},
			}
			if s := a.secret(); s != "" {
				auth = append(auth, jsjson.KV{Key: "join", Val: jsjson.Object{{Key: "secret", Val: s}, {Key: "pub", Val: a.nodePubRaw}}})
			}
			if a.c.Version != "" {
				auth = append(auth, jsjson.KV{Key: "version", Val: a.c.Version})
			}
			send(auth)
		case t == "ready":
			ready = true
			armDead()
			a.setLink(&coreLink{send: send, done: ctx.Done(), pending: map[int64]chan reply{}})
			if onReady != nil {
				onReady()
			}
			if a.secret() != "" {
				a.forgetJoinSecret()
				a.c.Logf("agent: нода привязана к core")
			}
			a.c.Logf("agent: подключено к core (%s)", a.addr())
			// Подтверждение — только когда адрес уже на диске: по нему панель решает, что
			// старый сервер можно выключать.
			if core, ok := m.Get2("core").(string); ok && a.learnCore(core) {
				send(jsjson.Object{{Key: "t", Val: "core_ok"}, {Key: "core", Val: core}})
			}
		case t == "metrics_res" && ready:
			a.deliver(m)
		case t == "req" && ready:
			id, ok := m.Get2("id").(float64)
			if !ok || id != float64(int64(id)) {
				continue
			}
			// Запросы независимы, как async-обработчики в Node: медленный /awg/… не держит очередь.
			go func() {
				r := a.execute(m)
				send(jsjson.Object{{Key: "t", Val: "res"}, {Key: "id", Val: id}, {Key: "status", Val: r.status}, {Key: "data", Val: r.data}})
			}()
		}
	}
}

// ── Метрики панели для Prometheus ноды ─────────────────────────────────────

func (a *Agent) setLink(l *coreLink) {
	a.mu.Lock()
	a.link = l
	a.mu.Unlock()
}

// deliver — ответ core на запрос метрик.
func (a *Agent) deliver(m jsjson.Object) {
	id, ok := m.Get2("id").(float64)
	a.mu.Lock()
	l := a.link
	a.mu.Unlock()
	if !ok || l == nil {
		return
	}
	l.mu.Lock()
	ch := l.pending[int64(id)]
	delete(l.pending, int64(id))
	l.mu.Unlock()
	if ch == nil {
		return
	}
	st, _ := m.Get2("status").(float64)
	text, _ := m.Get2("text").(string)
	ch <- reply{int(st), text}
}

// coreMetrics — экспозиция /metrics панели, как её отдал бы сам core.
func (a *Agent) coreMetrics(ctx context.Context) reply {
	a.mu.Lock()
	l := a.link
	a.mu.Unlock()
	if l == nil {
		return reply{503, "core недоступен\n"}
	}
	ch := make(chan reply, 1)
	l.mu.Lock()
	l.seq++
	id := l.seq
	l.pending[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()
	l.send(jsjson.Object{{Key: "t", Val: "metrics"}, {Key: "id", Val: id}})
	select {
	case r := <-ch:
		return r
	case <-l.done:
		return reply{503, "core недоступен\n"}
	case <-ctx.Done():
		return reply{504, "core не ответил\n"}
	}
}

// tokenOK — тот же токен, что встроенный Prometheus предъявил бы самой панели: хэш в файле,
// перечитываем на каждый скрейп (раз в 15 с), чтобы переустановка не требовала перезапуска.
func (a *Agent) tokenOK(r *http.Request) bool {
	given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	raw, err := os.ReadFile(a.c.MonitorHash)
	if !ok || !strings.HasPrefix(given, "awgm_") || err != nil {
		return false
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	sum := sha256.Sum256([]byte(given))
	return err == nil && len(want) == len(sum) && subtle.ConstantTimeCompare(sum[:], want) == 1
}

// serveMetrics — 127.0.0.1:AGENT_METRICS_PORT/metrics для Prometheus этой ноды: метрики всех
// серверов панели, полученные у core по уже открытому WSS. Наружу порт не открывается.
func (a *Agent) serveMetrics(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !a.tokenOK(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", 401)
			return
		}
		cctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
		defer cancel()
		res := a.coreMetrics(cctx)
		text, _ := res.data.(string)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(res.status)
		io.WriteString(w, text)
	})
	srv := &http.Server{Addr: "127.0.0.1:" + strconv.Itoa(a.c.MetricsPort), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	a.c.Logf("agent: метрики панели для Prometheus — http://%s/metrics", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.c.Errorf("agent: метрики для Prometheus не подняты: %v", err)
	}
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// Run переподключается вечно: экспоненциальная пауза до минуты, после отказа core — минута.
func (a *Agent) Run(ctx context.Context) error {
	a.c.Logf("agent: нода #%d → wss://%s", a.c.NodeID, a.addr())
	if a.c.MetricsPort > 0 {
		go a.serveMetrics(ctx)
	}
	backoff := time.Second
	for {
		ready := false
		authFailed := a.connectOnce(ctx, func() { backoff, ready = time.Second, true })
		if !ready {
			a.next()
		}
		wait := backoff
		if authFailed {
			wait = maxBackoff
		}
		backoff = min(backoff*2, maxBackoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
