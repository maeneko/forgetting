// Package cli — менеджер процессов Forgetting (порт cli/src): start/stop/restart/status,
// логин и пароль панели, join/unjoin ноды, пиры подписки, встроенный Prometheus и
// интерактивное меню. Сервисы — тот же бинарник с ролью: `awg-control ctrl|ui|agent`.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/maeneko/forgetting/internal/envfile"
	"github.com/maeneko/forgetting/internal/jsjson"
)

// ── Цвета ───────────────────────────────────────────────────────────────────

const (
	cReset = "\x1b[0m"
	cBold  = "\x1b[1m"
	cDim   = "\x1b[2m"
	cGreen = "\x1b[32m"
	cGrey  = "\x1b[90m"
	cBlue  = "\x1b[34m"
)

func bold(s string) string  { return cBold + s + cReset }
func green(s string) string { return cGreen + s + cReset }
func grey(s string) string  { return cGrey + s + cReset }
func dim(s string) string   { return cDim + s + cReset }

func errorf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

// ── Окружение ───────────────────────────────────────────────────────────────

// findRoot — корень установки: AWG_ROOT, иначе ближайший предок каталога бинарника, где есть
// cli/ или .env, иначе /opt/awg-control.
func findRoot() string {
	if r := os.Getenv("AWG_ROOT"); r != "" {
		return r
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		for d := filepath.Dir(exe); d != "/" && d != "."; d = filepath.Dir(d) {
			if exists(filepath.Join(d, "cli")) || exists(filepath.Join(d, ".env")) {
				return d
			}
		}
	}
	return "/opt/awg-control"
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// loadEnvFile — уже заданное окружение не перетираем (??= в Node), поэтому порядок =
// приоритет: реальное окружение → cli.env → .env в корне проекта.
func loadEnvFile(file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		i := strings.Index(t, "=")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(t[:i])
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.TrimSpace(t[i+1:]))
		}
	}
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// envOr — `process.env.X || def`.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type service struct {
	name    string // ctrl | ui | agent — роль бинарника
	label   string
	cwd     string
	legacy  string // entry Node-версии: её процессы тоже узнаём (переход с TS)
	env     [][2]string
	pidFile string
	logFile string
}

type CLI struct {
	root    string
	logs    string
	envFile string
	origEnv []string // окружение до чтения cli.env и .env — для свежего перезапуска
	exe     string

	brand, channel, version string

	svc    map[string]*service
	all    []string
	isNode bool

	in *bufio.Reader
}

func New() *CLI {
	c := &CLI{root: findRoot(), origEnv: os.Environ(), in: bufio.NewReader(os.Stdin)}
	c.exe, _ = os.Executable()
	c.logs = filepath.Join(c.root, "logs")
	os.MkdirAll(c.logs, 0o755)
	c.envFile = filepath.Join(c.root, "cli", "cli.env")
	loadEnvFile(c.envFile)                     // машинные настройки
	loadEnvFile(filepath.Join(c.root, ".env")) // имя панели и версия

	c.brand = envOr("BRAND", "Forgetting")
	c.channel = envOr("CHANNEL", "Beta")
	c.version = envOr("VERSION", "")

	cliEnv := c.envFile
	coreHost := os.Getenv("CORE_HOST")
	audit := "off"
	if coreHost == "" {
		audit = env("AUDIT", "on")
	}
	c.svc = map[string]*service{
		"awgctrl": {
			name: "ctrl", label: "awg-ctrl", cwd: filepath.Join(c.root, "awg-ctrl"), legacy: "index.ts",
			env: [][2]string{
				{"PORT", env("AWGCTRL_PORT", "3005")},
				{"SERVER_IP", env("SERVER_IP", "")},
				{"SERVER_PORT", env("SERVER_PORT", "47619")},
				{"SERVER_NAME", env("SERVER_NAME", "VPN")},
				// Публичный ключ внутренней авторизации (awg-ctrl проверяет им токены awg-ui).
				{"INTERNAL_AUTH_PUB_FILE", env("INTERNAL_AUTH_PUB_FILE", "/etc/amnezia/amneziawg/internal_auth_public.key")},
			},
			pidFile: "/tmp/awg-ctrl.pid", logFile: filepath.Join(c.logs, "awg-ctrl.log"),
		},
		"ui": {
			name: "ui", label: "awg-ui", cwd: filepath.Join(c.root, "awg-ui"), legacy: "server.ts",
			env: [][2]string{
				{"PORT", env("UI_PORT", "8080")},
				// Секретный путь панели; _MOVED — путь панели, переехавшей сюда (принимаются оба).
				{"UI_PREFIX", env("UI_PREFIX", "")},
				{"UI_PREFIX_MOVED", env("UI_PREFIX_MOVED", "")},
				{"AWGCTRL_PORT", env("AWGCTRL_PORT", "3005")},
				{"INTERNAL_AUTH_KEY_FILE", env("INTERNAL_AUTH_KEY_FILE", "/etc/amnezia/amneziawg/internal_auth_private.key")},
				{"BRAND", c.brand}, {"CHANNEL", c.channel}, {"VERSION", c.version},
				{"UI_USER", env("UI_USER", "admin")},
				{"UI_PASS", env("UI_PASS", "")},
				{"JWT_SECRET", env("JWT_SECRET", "")},
				// ui.db в каталоге данных AWG (вне проекта) — переживает переустановку.
				{"UI_DB_FILE", env("UI_DB_FILE", "/etc/amnezia/amneziawg/ui.db")},
				// Подписка sen://: без SUB_PORT или ключа подписи awg-ui подписку не поднимает.
				{"SUB_PORT", env("SUB_PORT", "")},
				{"SUB_TLS", env("SUB_TLS", "off")},
				{"SUB_SIGN_KEY_FILE", env("SUB_SIGN_KEY_FILE", "/etc/amnezia/amneziawg/sub_sign.key")},
				{"SUB_TLS_KEY_FILE", env("SUB_TLS_KEY_FILE", "/etc/amnezia/amneziawg/sub_tls.key")},
				{"SUB_TLS_CERT_FILE", env("SUB_TLS_CERT_FILE", "/etc/amnezia/amneziawg/sub_tls.crt")},
				{"SUB_HOST", env("SUB_HOST", "")},
				// Приём нод (core): без NODE_PORT хаб выключен. LOCAL_NODE=off — core без своего VPN.
				{"NODE_PORT", env("NODE_PORT", "")},
				// IP ретрансляторов (старый сервер после переноса): им — доверие к заголовку PROXY.
				{"RELAY_FROM", env("RELAY_FROM", "")},
				// Reverse-proxy перед панелью (кроме loopback): от них — X-Forwarded-For / X-Real-IP.
				{"TRUST_PROXY", env("TRUST_PROXY", "")},
				// Когда панель приняла перенос: с этого момента считаются клиенты, узнавшие новый адрес.
				{"MOVED_IN_AT", env("MOVED_IN_AT", "")},
				// Вкладка «Перенос» пишет сюда порты, PANEL_MOVED и RELAY_FROM.
				{"CLI_ENV_FILE", cliEnv},
				// Часовой аудит пиров на всех серверах — только у core.
				{"AUDIT", audit},
				{"METRICS_PEERS", env("METRICS_PEERS", "off")},
				{"LOCAL_NODE", env("LOCAL_NODE", "on")},
				{"NODE_TLS_KEY_FILE", env("NODE_TLS_KEY_FILE", "/etc/amnezia/amneziawg/node_tls.key")},
				{"NODE_TLS_CERT_FILE", env("NODE_TLS_CERT_FILE", "/etc/amnezia/amneziawg/node_tls.crt")},
			},
			pidFile: "/tmp/awg-ui.pid", logFile: filepath.Join(c.logs, "awg-ui.log"),
		},
		// Агент ноды: сам звонит в core и исполняет запросы на локальном awg-ctrl.
		"agent": {
			name: "agent", label: "awg-agent", cwd: filepath.Join(c.root, "awg-agent"), legacy: "index.ts",
			env: [][2]string{
				{"CORE_HOST", env("CORE_HOST", "")},
				{"CORE_PORT", env("CORE_PORT", "")},
				{"CORE_ENDPOINT", env("CORE_ENDPOINT", "")},
				{"CORE_PIN", env("CORE_PIN", "")},
				{"NODE_ID", env("NODE_ID", "")},
				{"NODE_KEY_FILE", env("NODE_KEY_FILE", "/etc/amnezia/amneziawg/node.key")},
				{"JOIN_SECRET", env("JOIN_SECRET", "")},
				{"CLI_ENV_FILE", cliEnv},
				{"AWGCTRL_PORT", env("AWGCTRL_PORT", "3005")},
				{"INTERNAL_AUTH_KEY_FILE", env("INTERNAL_AUTH_KEY_FILE", "/etc/amnezia/amneziawg/internal_auth_private.key")},
				// Prometheus на ноде (`monitor install`): здесь он скрейпит метрики панели.
				{"AGENT_METRICS_PORT", env("AGENT_METRICS_PORT", "")},
			},
			pidFile: "/tmp/awg-agent.pid", logFile: filepath.Join(c.logs, "awg-agent.log"),
		},
		// Ретранслятор на сервере, с которого переехала панель: прежние порты → новая панель.
		"relay": {
			name: "relay", label: "awg-relay", cwd: c.root,
			env:     [][2]string{{"RELAY", env("RELAY", "")}},
			pidFile: "/tmp/awg-relay.pid", logFile: filepath.Join(c.logs, "awg-relay.log"),
		},
	}

	// Роль установки. node — VPN-сервер без панели (есть CORE_HOST, его пишет `join`); core с
	// LOCAL_NODE=off — панель без своего VPN; иначе — обычная установка. Нода со своей панелью
	// (join без --no-panel) держит и awg-ui: обе панели ходят в один awg-ctrl.
	c.isNode = coreHost != ""
	noLocal := strings.ToLower(env("LOCAL_NODE", "on")) == "off"
	nodePanel := c.isNode && os.Getenv("UI_PASS") != "" && strings.ToLower(env("NODE_PANEL", "on")) != "off"
	switch {
	case c.isNode && nodePanel:
		c.all = []string{"awgctrl", "ui", "agent"}
	case c.isNode:
		c.all = []string{"awgctrl", "agent"}
	case noLocal:
		c.all = []string{"ui"}
	default:
		c.all = []string{"awgctrl", "ui"}
	}
	// Панель переехала (`migrate export`): здесь её больше не запускаем — вторая копия ui.db
	// разошлась бы с перенесённой.
	if c.panelMoved() {
		c.all = without(c.all, "ui")
	}
	if os.Getenv("RELAY") != "" {
		c.all = append(c.all, "relay")
	}
	return c
}

// setSvcEnv — значение, записанное в cli.env уже после запуска CLI, для следующего start.
func (c *CLI) setSvcEnv(name, key, value string) {
	s := c.svc[name]
	for i := range s.env {
		if s.env[i][0] == key {
			s.env[i][1] = value
			return
		}
	}
	s.env = append(s.env, [2]string{key, value})
}

func (c *CLI) panelMoved() bool { return strings.ToLower(os.Getenv("PANEL_MOVED")) == "on" }

func without(s []string, v string) []string {
	var out []string
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (c *CLI) envOf(name string) string {
	for _, kv := range c.svc[name].env {
		if kv[0] == "PORT" {
			return kv[1]
		}
	}
	return ""
}

func (c *CLI) svcEnv(name, key string) string {
	for _, kv := range c.svc[name].env {
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

// ── Процессы ────────────────────────────────────────────────────────────────

func (c *CLI) readPid(name string) int {
	raw, err := os.ReadFile(c.svc[name].pidFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(firstInt(strings.TrimSpace(string(raw))))
	if err != nil {
		return 0
	}
	return n
}

var reLeadInt = regexp.MustCompile(`^[+-]?\d+`)

// firstInt — parseInt: ведущее целое, остальное игнорируется.
func firstInt(s string) string { return reLeadInt.FindString(s) }

// isAlive — EPERM: процесс есть, просто не наш (считаем живым).
func isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidMatches — защита от переиспользования PID: сверяем cmdline с сервисом. Узнаём и Go-сервис
// (`awg-control <роль>`), и процесс Node-версии (entry-файл в cmdline). Без procfs проверить нечем.
func (c *CLI) pidMatches(name string, pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return true
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	s := c.svc[name]
	if len(args) >= 2 && filepath.Base(args[0]) == filepath.Base(c.exe) && args[1] == s.name {
		return true
	}
	if s.legacy == "" {
		return false
	}
	// tsx запускается из node_modules сервиса: путь различает index.ts awg-ctrl и awg-agent.
	return strings.Contains(string(raw), s.legacy) && strings.Contains(string(raw), filepath.Base(s.cwd))
}

func (c *CLI) isRunning(name string) bool {
	pid := c.readPid(name)
	return pid != 0 && isAlive(pid) && c.pidMatches(name, pid)
}

func (c *CLI) start(name string) {
	s := c.svc[name]
	if c.isRunning(name) {
		fmt.Printf("  %s: already running  pid=%d\n", s.label, c.readPid(name))
		return
	}
	if name == "awgctrl" {
		if f := c.svcEnv("awgctrl", "INTERNAL_AUTH_PUB_FILE"); !exists(f) {
			errorf("  awgctrl: публичный ключ внутренней авторизации не найден: %s", f)
			os.Exit(1)
		}
	}
	logF, err := os.OpenFile(s.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		errorf("  %s: не удалось запустить — %v", s.label, err)
		return
	}
	defer logF.Close() // дескриптор унаследован ребёнком — родителю не нужен
	cmd := exec.Command(c.exe, s.name)
	cmd.Dir = s.cwd
	if !exists(s.cwd) {
		cmd.Dir = c.root
	}
	cmd.Env = os.Environ()
	for _, kv := range s.env {
		cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
	}
	cmd.Stdout, cmd.Stderr = logF, logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detached: переживает CLI
	if err := cmd.Start(); err != nil {
		errorf("  %s: не удалось запустить — %v", s.label, err)
		os.Remove(s.pidFile)
		return
	}
	go cmd.Wait() // пока CLI жив (меню), не оставляем зомби
	os.WriteFile(s.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	fmt.Printf("  %s %s: started  pid=%d\n", green("▶"), s.label, cmd.Process.Pid)
}

func (c *CLI) stop(name string) {
	s := c.svc[name]
	pid := c.readPid(name)
	if pid == 0 || !isAlive(pid) || !c.pidMatches(name, pid) {
		fmt.Printf("  %s: not running\n", s.label)
		os.Remove(s.pidFile)
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	// Ждём мягкого завершения до 5 с, иначе добиваем SIGKILL.
	deadline := time.Now().Add(5 * time.Second)
	for isAlive(pid) && !isZombie(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if isAlive(pid) && !isZombie(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(200 * time.Millisecond)
	}
	os.Remove(s.pidFile)
	fmt.Printf("  %s %s: stopped  pid=%d\n", grey("■"), s.label, pid)
}

// isZombie — процесс завершился, но его ещё не забрал родитель (наш же Wait в меню).
func isZombie(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(raw)
	if i := strings.LastIndex(s, ")"); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z'
	}
	return false
}

// fetchHealth — GET /health и разбор JSON. Любая ошибка — nil.
func fetchHealth(url string) any {
	cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.WriteString(string(buf[:n]))
		if err != nil {
			break
		}
	}
	v, err := jsjson.Decode([]byte(b.String()))
	if err != nil {
		return nil
	}
	return v
}

func get(v any, keys ...string) any {
	for _, k := range keys {
		o, ok := v.(jsjson.Object)
		if !ok {
			return nil
		}
		v = o.Get2(k)
	}
	return v
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func (c *CLI) serviceLine(name string, menu bool) string {
	running := c.isRunning(name)
	dot, info := grey("○"), grey("stopped")
	if running {
		dot = green("●")
		if menu {
			info = green("running") + dim(fmt.Sprintf("  pid %d", c.readPid(name)))
		} else {
			info = green("running") + fmt.Sprintf("  pid %d", c.readPid(name))
		}
	}
	return fmt.Sprintf("  %s %-12s %s", dot, c.svc[name].label, info)
}

// panelURL — адрес панели с секретным путём; без UI_PORT (нода) — пусто.
func panelURL() string {
	port := env("UI_PORT", "")
	if port == "" {
		return ""
	}
	host := env("SUB_HOST", env("SERVER_IP", "IP"))
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u := "http://" + host + ":" + port + "/"
	if p := env("UI_PREFIX", ""); p != "" {
		u += p + "/"
	}
	return u
}

func (c *CLI) status() {
	fmt.Println()
	for _, n := range c.all {
		fmt.Println(c.serviceLine(n, false))
	}
	if contains(c.all, "ui") {
		if u := panelURL(); u != "" {
			fmt.Println("  " + dim("Панель "+u))
		}
	}
	if c.isRunning("awgctrl") {
		h := fetchHealth("http://127.0.0.1:" + c.envOf("awgctrl") + "/health")
		if gen, ok := get(h, "gen").(string); ok && gen != "" {
			g := "2.0"
			if gen == "3.1" {
				g = "3.1"
			}
			fmt.Println("  " + dim(fmt.Sprintf("AmneziaWG %s · модуль %s · tools %s", g, strOr(get(h, "awg", "module"), "?"), strOr(get(h, "awg", "tools"), "?"))))
		}
	}
	fmt.Println()
}

// probe — любой HTTP-ответ (даже 401/503) значит, что порт слушается.
func probe(url string) bool {
	cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// waitForHealth — ждём, пока awg-ctrl начнёт отвечать; умер — сразу выходим.
func (c *CLI) waitForHealth() bool {
	url := "http://127.0.0.1:" + c.envOf("awgctrl") + "/health"
	for range 10 {
		if !c.isRunning("awgctrl") {
			return false
		}
		if probe(url) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func (c *CLI) startAll() {
	// Панель без своего VPN: awg-ctrl рядом нет, health-gate не нужен.
	if !contains(c.all, "awgctrl") {
		for _, n := range c.all {
			c.start(n)
		}
		return
	}
	// Ретранслятор от awg-ctrl не зависит: подписка и панель через него не должны ждать VPN.
	if contains(c.all, "relay") {
		c.start("relay")
	}
	c.start("awgctrl")
	var rest, labels []string
	for _, n := range c.all {
		if n != "awgctrl" && n != "relay" {
			rest = append(rest, n)
			labels = append(labels, c.svc[n].label)
		}
	}
	if !c.isRunning("awgctrl") || !c.waitForHealth() {
		errorf("  %s awg-ctrl не отвечает — %s не запущен (см. %s)", grey("■"), strings.Join(labels, ", "), c.svc["awgctrl"].logFile)
		return
	}
	for _, n := range rest {
		c.start(n)
	}
}

func (c *CLI) stopAll() {
	for _, n := range c.all {
		c.stop(n)
	}
}

func (c *CLI) restartAll() {
	c.stopAll()
	c.startAll()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ── cli.env ─────────────────────────────────────────────────────────────────

func updateEnvVar(content, key, value string) string { return envfile.Update(content, key, value) }

func removeEnvVar(content, key string) string { return envfile.Remove(content, key) }

func (c *CLI) readEnvFile() string {
	raw, _ := os.ReadFile(c.envFile)
	return string(raw)
}

// writeEnvFile — новый файл с правами 0600 (в нём пароль панели); у существующего права не трогаем.
func (c *CLI) writeEnvFile(content string) error {
	return os.WriteFile(c.envFile, []byte(content), 0o600)
}

// restartFresh — роль читается из окружения при старте процесса, поэтому после смены cli.env
// перезапуск — свежим процессом с исходным окружением (без значений из старого cli.env).
func (c *CLI) restartFresh() {
	cmd := exec.Command(c.exe, "restart")
	cmd.Env = c.origEnv
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Run()
}

// ── Ввод ────────────────────────────────────────────────────────────────────

func (c *CLI) ask(prompt string) string {
	fmt.Print(prompt)
	line, _ := c.in.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

var reYes = regexp.MustCompile(`(?i)^y`)

func (c *CLI) confirmYes(q string) bool {
	return reYes.MatchString(strings.TrimSpace(c.ask(q + " [y/N] ")))
}

// ── Точка входа ─────────────────────────────────────────────────────────────

const usage = "Usage: cli <start|stop|restart|status|credentials [user|pass]|join <awgjoin://…> [--no-panel]|unjoin|rehome <хост[:порт]>|peers [drop <область>]|monitor <install|status|remove>|migrate <export [файл]|import <файл>|cancel>>"

// Commands — команды оператора; main передаёт сюда всё, что не роль сервиса.
var Commands = []string{"start", "stop", "restart", "status", "credentials", "join", "unjoin", "rehome", "peers", "monitor", "migrate"}

// Main выполняет команду CLI (args — без имени бинарника). Пусто — интерактивное меню.
func Main(args []string) {
	c := New()
	if len(args) == 0 {
		c.interactiveMenu()
		return
	}
	arg := func(i int) (string, bool) {
		if i < len(args) {
			return args[i], true
		}
		return "", false
	}
	sub, hasSub := arg(1)
	a2, hasA2 := arg(2)
	switch args[0] {
	case "start":
		c.startAll()
	case "stop":
		c.stopAll()
	case "restart":
		c.restartAll()
	case "status":
		c.status()
	case "credentials":
		switch sub {
		case "user", "pass":
			c.setCredentials(sub)
		default:
			c.setCredentials("")
		}
	case "join":
		c.joinCore(sub, hasSub, a2, hasA2)
	case "unjoin":
		c.unjoinCore()
	case "rehome":
		c.rehome(sub, hasSub)
	case "migrate":
		c.migrateCmd(sub, a2, hasA2)
	case "peers":
		c.peersCmd(sub, hasSub, a2, hasA2)
	case "monitor":
		c.monitorCmd(sub)
	default:
		fmt.Println(usage)
		os.Exit(1)
	}
}
