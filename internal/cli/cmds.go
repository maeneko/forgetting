package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
)

var b64url = base64.RawURLEncoding

// fromB64url — Buffer.from(s, "base64url"): нестрого, как в Node.
func fromB64url(s string) []byte {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	b, _ := b64url.DecodeString(s)
	return b
}

// ── join / unjoin ───────────────────────────────────────────────────────────

var (
	reJoinHost = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
	reJoinPin  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	reJoinSec  = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

func isPosInt(v any, max float64) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok && f == float64(int64(f)) && f > 0 && f < max
}

// ── endpoint ────────────────────────────────────────────────────────────────

// endpointCmd — `endpoint <хост:порт>`: какой адрес получают VPN-клиенты в ключах vpn:// и в
// /api/profile. По умолчанию — собственный адрес сервера; если подключение к серверу принимает
// релей (роль «релей»), здесь задают его адрес — релей пересылает UDP VPN-порт нод. Значение
// пишется в cli.env (CLIENT_ENDPOINT), awg-ctrl перечитывает его при перезапуске. `endpoint`
// без аргументов — посмотреть, `endpoint off` — вернуть собственный адрес сервера.
func (c *CLI) endpointCmd(addr string, hasAddr bool, args []string) {
	if c.isRelay {
		errorf("  Это релей — у него нет awg-ctrl и ключей; адрес задаётся на VPN-сервере, который релей принимает")
		os.Exit(1)
	}
	if !hasAddr {
		if cur := strings.TrimSpace(os.Getenv("CLIENT_ENDPOINT")); cur != "" {
			fmt.Println("  Клиенты получают: " + cur)
		} else {
			fmt.Println("  CLIENT_ENDPOINT не задан — клиенты получают собственный адрес сервера")
		}
		return
	}
	if addr == "off" {
		c.writeEnvFile(removeEnvVar(c.readEnvFile(), "CLIENT_ENDPOINT"))
		fmt.Printf("  %s Клиентам снова отдаётся собственный адрес сервера\n", green("✓"))
		c.restartFresh()
		return
	}
	host, port, err := net.SplitHostPort(addr)
	p, perr := strconv.Atoi(port)
	if err != nil || host == "" || !reJoinHost.MatchString(host) || perr != nil || p <= 0 || p > 65535 {
		errorf("  Usage: awg-ctrl endpoint <хост:порт> | off   (адрес для клиентов; IPv6 — [v6]:порт)")
		os.Exit(1)
	}
	c.writeEnvFile(updateEnvVar(c.readEnvFile(), "CLIENT_ENDPOINT", host+":"+port))
	fmt.Printf("  %s Клиенты будут получать endpoint %s:%d (awg-ctrl перезапущен)\n", green("✓"), host, p)
	c.restartFresh()
} // joinCore — `join <awgjoin://…> [--no-panel] | <awgrelay://…>`: подключить эту установку к
// главной панели как ноду или (по строке awgrelay://) настроить её как релей — пересылки из
// строки плюс агент для статуса в панели. awg-ctrl и пользователи остаются как были. Своя
// панель продолжает работать, если она здесь есть, — с --no-panel она останавливается.
func (c *CLI) joinCore(link string, hasLink bool, flag string, hasFlag bool) {
	if !hasLink || hasFlag && flag != "--no-panel" {
		errorf("  Usage: awg-ctrl join <awgjoin://… | awgrelay://…> [--no-panel]   (ссылки даёт панель: «Добавить сервер» / «Добавить релей»)")
		os.Exit(1)
	}
	var scheme string
	switch {
	case strings.HasPrefix(link, "awgjoin://"):
		scheme = "awgjoin"
	case strings.HasPrefix(link, "awgrelay://"):
		scheme = "awgrelay"
	default:
		errorf("  Usage: awg-ctrl join <awgjoin://… | awgrelay://…> [--no-panel]   (ссылки даёт панель: «Добавить сервер» / «Добавить релей»)")
		os.Exit(1)
	}
	v, err := jsjson.Decode(fromB64url(link[len(scheme)+3:]))
	j, isObj := v.(jsjson.Object)
	if err != nil || !isObj {
		errorf("  Ссылка повреждена")
		os.Exit(1)
	}
	if scheme == "awgrelay" {
		c.joinRelay(j)
		return
	}
	h, hok := j.Get2("h").(string)
	pin, pinok := j.Get2("pin").(string)
	sec, sok := j.Get2("s").(string)
	port, pok := isPosInt(j.Get2("p"), 65536)
	id, iok := isPosInt(j.Get2("n"), 9007199254740992)
	// Нода #0 — этот же сервер после переноса панели (`migrate import` выдаёт строку с n = 0).
	if n, ok := j.Get2("n").(float64); ok && n == 0 {
		id, iok = 0, true
	}
	if j.Get2("v") != 1.0 || !hok || !reJoinHost.MatchString(h) || !pok || !pinok || !reJoinPin.MatchString(pin) ||
		!iok || !sok || !reJoinSec.MatchString(sec) {
		errorf("  Ссылка неверного формата или от другой версии панели")
		os.Exit(1)
	}
	if c.isNode && !c.confirmYes(fmt.Sprintf("  Эта нода уже подключена к %s. Перепривязать?", os.Getenv("CORE_HOST"))) {
		return
	}

	hasPanel := os.Getenv("UI_PASS") != ""
	moved := c.panelMoved()
	keepPanel := !hasFlag && hasPanel && !moved
	content := c.readEnvFile()
	np := "off"
	if keepPanel {
		np = "on"
	}
	for _, kv := range [][2]string{{"CORE_HOST", h}, {"CORE_PORT", fmt.Sprint(port)}, {"CORE_PIN", pin}, {"NODE_ID", fmt.Sprint(id)},
		{"JOIN_SECRET", sec}, {"NODE_PANEL", np}} {
		content = updateEnvVar(content, kv[0], kv[1])
	}
	content = removeEnvVar(content, "CORE_ENDPOINT") // адрес прежней панели к новой не относится
	relay := ""
	if moved {
		relay = relayRoutes(h, port, j)
		content = updateEnvVar(content, "RELAY", relay)
	}
	c.writeEnvFile(content)

	// Панель останавливается, а пиры её устройств остаются на awg-ctrl — управлять ими больше
	// некому. Решает оператор; спрашиваем до остановки, снимаем после (иначе панель вернёт их).
	// После переноса этими пирами управляет новая панель: снимать нечего.
	dropPeers := !keepPanel && hasPanel && !moved && c.askDropPanelPeers()
	if !keepPanel {
		c.stop("ui")
	}
	if dropPeers {
		c.dropArea(c.localPanelID())
	}
	tail := ""
	if keepPanel {
		tail = " · своя панель продолжает работать"
	} else if hasPanel {
		tail = " · своя панель остановлена"
	}
	fmt.Printf("\n  %s Настроено: core %s:%d, нода #%d%s\n", green("✓"), h, port, id, tail)
	if relay != "" {
		fmt.Println("  " + dim("Ретранслятор прежних портов на новую панель: "+relay))
	}
	c.restartFresh()
}

// relayRoutes — прежние порты этого сервера → новая панель. Подписка и хаб там на тех же
// портах (так их ставит `migrate import`), порт панели — из поля ui join-строки.
func relayRoutes(host string, hubPort int64, j jsjson.Object) string {
	hp := func(p any) string { return net.JoinHostPort(host, fmt.Sprint(p)) }
	var routes []string
	add := func(local int, target string) {
		if local > 0 {
			routes = append(routes, fmt.Sprintf("%d=%s", local, target))
		}
	}
	sub := envPort("SUB_PORT")
	if p, ok := isPosInt(j.Get2("sub"), 65536); ok {
		add(sub, hp(p))
	} else {
		add(sub, hp(sub))
	}
	add(envPort("NODE_PORT"), hp(hubPort))
	if p, ok := isPosInt(j.Get2("ui"), 65536); ok {
		add(envPort("UI_PORT"), hp(p))
	}
	return strings.Join(routes, ",")
}

// joinRelay — хвост команды `join 'awgrelay://…'`: подключение релея к панели. Строка без
// маршрутов: в cli.env пишутся только адрес панели и роль relay, привязки приходят по пушу
// от панели через агента (`awg-control relay-apply`). VPN и панель на реле не ставятся —
// это отдельная роль установки.
func (c *CLI) joinRelay(j jsjson.Object) {
	h, hok := j.Get2("h").(string)
	pin, pinok := j.Get2("pin").(string)
	sec, sok := j.Get2("s").(string)
	port, pok := isPosInt(j.Get2("p"), 65536)
	id, iok := isPosInt(j.Get2("n"), 9007199254740992)
	inv := func() {
		errorf("  Ссылка неверного формата или от другой версии панели")
		os.Exit(1)
	}
	if j.Get2("v") != 1.0 || !hok || !reJoinHost.MatchString(h) || !pok || !pinok || !reJoinPin.MatchString(pin) ||
		!iok || !sok || !reJoinSec.MatchString(sec) {
		inv()
	}
	if c.isNode && !c.confirmYes(fmt.Sprintf("  Релей уже настроен (core %s). Перепривязать?", os.Getenv("CORE_HOST"))) {
		return
	}
	if os.Getenv("UI_PASS") != "" {
		errorf("  На этом сервере есть панель — роль «релей» ставится на отдельную машину (install.sh, роль «Релей»)")
		os.Exit(1)
	}
	content := c.readEnvFile()
	for _, kv := range [][2]string{
		{"CORE_HOST", h}, {"CORE_PORT", fmt.Sprint(port)}, {"CORE_PIN", pin}, {"NODE_ID", fmt.Sprint(id)},
		{"JOIN_SECRET", sec}, {"NODE_ROLE", "relay"}, {"NODE_PANEL", "off"}, {"LOCAL_NODE", "off"},
		// Свежая привязка panel-маршрутов не содержит — приоритет за пушем, накопленное стираем.
		{"RELAY_UDP", ""}, {"RELAY_TCP", ""},
	} {
		content = updateEnvVar(content, kv[0], kv[1])
	}
	content = removeEnvVar(content, "CORE_ENDPOINT") // адрес прежней панели к новой не относится
	c.writeEnvFile(content)
	fmt.Printf("\n  %s Релей настроен: core %s:%d, релей #%d\n", green("✓"), h, port, id)
	fmt.Println("  " + dim("Привязки серверов агент получит от панели после подключения"))
	c.restartFresh()
}

// relayApply — хвост команды `relay-apply <b64>` (запускает агент по пушу панели, оператору
// не нужна): на машине релея перезаписать RELAY_UDP/RELAY_TCP в cli.env и перезапустить
// awg-relay. Пустые строки — привязок больше нет: awg-relay остановить. Сервисная карта env
// обновляется по ходу: start() читает её же, а не cli.env.
func (c *CLI) relayApply(payload string, hasPayload bool) {
	if !hasPayload {
		errorf("  Usage: awg-control relay-apply <b64<{udp,tcp}>>")
		os.Exit(1)
	}
	if !c.isRelay {
		errorf("  Нода не релей (NODE_ROLE=relay в cli.env) — привязки не к чему применять")
		os.Exit(1)
	}
	v, err := jsjson.Decode(fromB64url(payload))
	j, isObj := v.(jsjson.Object)
	if err != nil || !isObj {
		errorf("  Привязки повреждены")
		os.Exit(1)
	}
	udp, _ := j.Get2("udp").(string)
	tcp, _ := j.Get2("tcp").(string)
	if !parseRoutes(udp) || !parseRoutes(tcp) {
		errorf("  Привязки неверного формата или от другой версии панели")
		os.Exit(1)
	}
	content := c.readEnvFile()
	content = updateEnvVar(content, "RELAY_UDP", udp)
	content = updateEnvVar(content, "RELAY_TCP", tcp)
	c.writeEnvFile(content)
	c.setSvcEnv("relay", "RELAY_UDP", udp)
	c.setSvcEnv("relay", "RELAY_TCP", tcp)
	c.stop("relay")
	if udp != "" || tcp != "" {
		c.start("relay")
		fmt.Printf("  %s Привязки применены\n", green("✓"))
	} else {
		fmt.Printf("  %s Привязок нет: awg-relay остановлен\n", green("✓"))
	}
}

// parseRoutes — строка маршрутов «порт=target,порт=target»: у частей валидный порт и адрес.
// Пустая строка легальна (так стираются все привязки).
func parseRoutes(list string) bool {
	if list == "" {
		return true
	}
	for _, part := range strings.Split(list, ",") {
		port, rest, ok := strings.Cut(part, "=")
		if !ok || !posPort(port) || !reJoinHost.MatchString(rest) || !strings.Contains(rest, ":") {
			return false
		}
	}
	return true
}

// posPort — порт «1..65535» в строке.
func posPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n < 65536
}

func (c *CLI) unjoinCore() {
	if !c.isNode {
		fmt.Println("  Эта установка не подключена к core")
		return
	}
	c.stop("agent")
	if !c.isRelay {
		c.stop("relay")
	}
	content := c.readEnvFile()
	keys := []string{"CORE_HOST", "CORE_PORT", "CORE_PIN", "NODE_ID", "JOIN_SECRET", "NODE_PANEL", "CORE_ENDPOINT"}
	if c.isRelay {
		keys = append(keys, "NODE_ROLE", "RELAY_UDP", "RELAY_TCP")
	} else {
		// Роуты «релея» могли остаться от migrate — их ролью не трогаем.
	}
	for _, k := range keys {
		content = removeEnvVar(content, k)
	}
	c.writeEnvFile(content)
	fmt.Printf("  %s Нода отвязана. Удалите её в панели (сервер) и запустите: awg-ctrl restart\n", green("✓"))
	// Нода, поставленная install.sh с ролью «нода», панели не имеет — без пароля в неё не войти.
	if os.Getenv("UI_PASS") == "" {
		fmt.Println("  " + dim("Панель на этой машине не настроена: задайте логин и пароль — awg-ctrl credentials"))
	}
}

// ── Пиры подписки (sen://) на локальном awg-ctrl ────────────────────────────
// Пира ставит панель, owner = «<id панели>/m<ключ>/d<устройство>»: id — область панели. Пиры
// без области («m1/d2») выданы до областей. Здесь — ручной рычаг для оператора: панель
// выключена или core пропал насовсем. По времени ничего не удаляется.

var (
	reArea        = regexp.MustCompile(`^[a-z0-9]{8,16}$`)
	reLegacyOwner = regexp.MustCompile(`^m\d+/d\d+$`)
)

func (c *CLI) localPanelID() string {
	raw, err := os.ReadFile(filepath.Join(c.root, "awg-ui", "panel.id"))
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(raw)); reArea.MatchString(id) {
		return id
	}
	return ""
}

type ctrlResp struct {
	status int
	data   any
}

// ctrlCall — запрос к локальному awg-ctrl с тем же короткоживущим токеном, что у панели и агента.
func (c *CLI) ctrlCall(method, path string, body any) (*ctrlResp, error) {
	key, err := iauth.LoadPrivate(c.svcEnv("ui", "INTERNAL_AUTH_KEY_FILE"))
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(jsjson.MustMarshal(body))
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:"+c.envOf("awgctrl")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+iauth.Mint(key, "awg-cli", time.Now()))
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	data, err := jsjson.Decode(raw)
	if err != nil {
		data = nil
	}
	return &ctrlResp{resp.StatusCode, data}, nil
}

type ctrlPeer struct {
	pub, ip, owner string
	online         bool
}

func ctrlError(r *ctrlResp) error {
	if e, ok := get(r.data, "error").(string); ok {
		return fmt.Errorf("%s", e)
	}
	return fmt.Errorf("awg-ctrl ответил %d", r.status)
}

func (c *CLI) listPeers() ([]ctrlPeer, error) {
	r, err := c.ctrlCall("GET", "/api/peers", nil)
	if err != nil {
		return nil, err
	}
	if r.status != 200 {
		return nil, ctrlError(r)
	}
	var out []ctrlPeer
	list, _ := get(r.data, "peers").([]any)
	for _, p := range list {
		on, _ := get(p, "online").(bool)
		out = append(out, ctrlPeer{pub: strOr(get(p, "pub_key"), ""), ip: strOr(get(p, "ip"), ""), owner: strOr(get(p, "owner"), ""), online: on})
	}
	return out, nil
}

// areaOf — область пира: id панели, «legacy» (до областей) или "" (чужой формат — не наш).
func areaOf(owner string) string {
	head := strings.SplitN(owner, "/", 2)[0]
	if strings.Contains(owner, "/") && reArea.MatchString(head) {
		return head
	}
	if reLegacyOwner.MatchString(owner) {
		return "legacy"
	}
	return ""
}

func peersWord(n int) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return "пир"
	case n%10 >= 2 && n%10 <= 4 && !(n%100 >= 12 && n%100 <= 14):
		return "пира"
	}
	return "пиров"
}

func (c *CLI) askDropPanelPeers() bool {
	own := c.localPanelID()
	if own == "" {
		return false
	}
	peers, err := c.listPeers()
	if err != nil {
		fmt.Fprintln(os.Stderr, "  "+dim("Не удалось посмотреть пиров подписки: "+err.Error()))
		return false
	}
	n := 0
	for _, p := range peers {
		if areaOf(p.owner) == own {
			n++
		}
	}
	if n == 0 {
		return false
	}
	switch strings.ToLower(os.Getenv("NODE_PEERS")) {
	case "keep":
		return false
	case "drop":
		return true
	}
	fmt.Printf("\n  На этой панели %d %s подписки (устройства мастер-ключей). Без панели ими некому управлять:\n", n, peersWord(n))
	fmt.Println("    1) Оставить — продолжат работать, отозвать их можно будет только после unjoin")
	fmt.Println("    2) Отключить — снять их с VPN (ключи останутся в ui.db и вернутся после unjoin)")
	return strings.TrimSpace(c.ask("  Выбор [1]: ")) == "2"
}

// dropArea — снять всю область: для панели — пустым списком через sync, для legacy — по одному.
func (c *CLI) dropArea(area string) {
	if area == "legacy" {
		peers, err := c.listPeers()
		if err != nil {
			errorf("  Не удалось снять пиров: %v", err)
			return
		}
		n := 0
		for _, p := range peers {
			if areaOf(p.owner) != "legacy" {
				continue
			}
			if _, err := c.ctrlCall("DELETE", "/api/peers/"+b64url.EncodeToString(stdB64(p.pub)), nil); err != nil {
				errorf("  Не удалось снять пиров: %v", err)
				return
			}
			n++
		}
		fmt.Printf("  %s Снято пиров без области: %d\n", green("✓"), n)
		return
	}
	r, err := c.ctrlCall("PUT", "/api/peers/sync", jsjson.Object{{Key: "owner", Val: area}, {Key: "keep", Val: []any{}}})
	if err == nil && r.status != 200 {
		err = ctrlError(r)
	}
	if err != nil {
		errorf("  Не удалось снять пиров: %v", err)
		return
	}
	fmt.Printf("  %s Снято пиров области %s: %s\n", green("✓"), area, jsString(get(r.data, "removed")))
}

func stdB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fromB64url(s)
	}
	return b
}

// jsString — `${v}` для вывода числа или строки из ответа.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case float64:
		return jsjson.Number(x)
	}
	return string(jsjson.MustMarshal(v))
}

func (c *CLI) peersCmd(sub string, hasSub bool, arg string, hasArg bool) {
	own := c.localPanelID()
	peers, err := c.listPeers()
	if err != nil {
		errorf("  awg-ctrl недоступен: %v", err)
		os.Exit(1)
	}
	var order []string
	areas := map[string][]ctrlPeer{}
	for _, p := range peers {
		if a := areaOf(p.owner); a != "" {
			if _, seen := areas[a]; !seen {
				order = append(order, a)
			}
			areas[a] = append(areas[a], p)
		}
	}

	if !hasSub {
		if len(areas) == 0 {
			fmt.Println("  Пиров подписки (sen://) на этом сервере нет")
			return
		}
		fmt.Print("\n  Пиры подписки (sen://) на этом сервере:\n\n")
		for _, a := range order {
			list := areas[a]
			mark := ""
			if a == own {
				mark = "эта панель"
			} else if a == "legacy" {
				mark = "без области (выданы до областей)"
			}
			online := 0
			for _, p := range list {
				if p.online {
					online++
				}
			}
			fmt.Printf("    %s %4d %-5s  онлайн %d  %s\n", bold(fmt.Sprintf("%-16s", a)), len(list), peersWord(len(list)), online, dim(mark))
		}
		fmt.Printf("\n  %s\n", dim("Снять область целиком: awg-ctrl peers drop <область>"))
		return
	}
	if sub != "drop" || !hasArg || arg == "" || !(arg == "legacy" || reArea.MatchString(arg)) {
		errorf("  Usage: awg-ctrl peers [drop <область|legacy>]")
		os.Exit(1)
	}
	n := len(areas[arg])
	if n == 0 {
		fmt.Printf("  В области %s пиров нет\n", arg)
		return
	}
	// Панель, которая ведёт эту область, вернёт пиров при следующем сведении — снимать бесполезно.
	if arg == own && c.isRunning("ui") {
		errorf("  Эту область ведёт панель на этой машине — удаляйте мастер-ключи или устройства в ней")
		os.Exit(1)
	}
	fmt.Println("  " + dim("Если панель этой области ещё ведёт сервер, она вернёт пиров при следующем сведении —"))
	fmt.Println("  " + dim("сперва удалите сервер в ней (или мастер-ключи)."))
	if !c.confirmYes(fmt.Sprintf("  Снять %d %s области %s? Их устройства потеряют доступ к этому серверу.", n, peersWord(n), arg)) {
		return
	}
	c.dropArea(arg)
}

// ── Логин и пароль панели ───────────────────────────────────────────────────

func (c *CLI) setCredentials(field string) {
	var user, pass *string
	if field == "" || field == "user" {
		cur := env("UI_USER", "admin")
		u := strings.TrimSpace(c.ask(fmt.Sprintf("\n  Логин [%s]: ", cur)))
		if u == "" {
			u = cur
		}
		user = &u
	}
	if field == "" || field == "pass" {
		b := make([]byte, 9)
		rand.Read(b)
		generated := b64url.EncodeToString(b)[:12]
		p := strings.TrimSpace(c.ask(fmt.Sprintf("  Пароль [%s]: ", generated)))
		if p == "" {
			p = generated
		}
		pass = &p
	}
	content := c.readEnvFile()
	if user != nil {
		content = updateEnvVar(content, "UI_USER", *user)
		os.Setenv("UI_USER", *user)
	}
	if pass != nil {
		content = updateEnvVar(content, "UI_PASS", *pass)
		os.Setenv("UI_PASS", *pass)
	}
	c.writeEnvFile(content)
	if user != nil {
		fmt.Printf("\n  %s Логин:  %s\n", green("✓"), *user)
	}
	if pass != nil {
		fmt.Printf("  %s Пароль: %s\n", green("✓"), *pass)
	}
	if u := panelURL(); u != "" {
		fmt.Printf("  %s Панель: %s\n", green("✓"), u)
	}
	fmt.Printf("\n  %s\n", dim("Перезапусти UI чтобы изменения вступили в силу."))
}

// ── Интерактивное меню ──────────────────────────────────────────────────────

func clearScreen() {
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Print("\x1b[1;1H\x1b[0J")
	}
}

func (c *CLI) interactiveMenu() {
	printMenu := func() {
		clearScreen()
		var parts []string
		for _, p := range []string{c.brand, c.channel, c.version} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		fmt.Printf("\n%s\n\n", bold(cBlue+"── "+strings.Join(parts, " ")+" ──"+cReset))
		for _, n := range c.all {
			fmt.Println(c.serviceLine(n, true))
		}
		sep := dim("──────────────")
		fmt.Printf(`
  %s  Запустить
  %s  Остановить
  %s  Перезапустить
  %s  Статус
  %s
  %s  Сменить логин UI
  %s  Сменить пароль UI
  %s
  %s  Обновить
  %s  Переустановить
  %s  Удалить
  %s
  %s  Адрес панели и секретный путь
  %s  Журнал подключений
  %s  Кто когда был в VPN
  %s  Проверка скрытности
  %s
  %s  Выход

`, bold("1"), bold("2"), bold("3"), bold("4"), sep, bold("5"), bold("6"), sep, bold("7"), bold("8"), bold("9"), sep,
			bold("10"), bold("11"), bold("12"), bold("13"), sep, bold("0"))
	}
	for {
		printMenu()
		fmt.Print("  › ")
		choice, err := c.in.ReadString('\n')
		if err != nil && choice == "" {
			fmt.Println()
			return // stdin закрыт
		}
		choice = strings.TrimSpace(choice)
		fmt.Println()
		switch choice {
		case "1":
			c.startAll()
		case "2":
			c.stopAll()
		case "3":
			c.restartAll()
		case "4":
			c.status()
		case "5":
			c.setCredentials("user")
		case "6":
			c.setCredentials("pass")
		case "7":
			c.runInstaller("update", true)
		case "8":
			c.runInstaller("reinstall", true)
		case "9":
			c.runInstaller("uninstall", true)
		case "10":
			c.panelMenu()
		case "11":
			c.logCmd(nil)
		case "12":
			c.logCmd([]string{"seen"})
		case "13":
			c.checkCmd()
		case "0":
			os.Exit(0)
		default:
			fmt.Println(grey("  Неверный выбор"))
		}
		c.ask("\n  " + dim("Enter для продолжения..."))
	}
}
