package cli

// Команды для новых возможностей панели — то же, что в веб-интерфейсе, но с сервера:
//   panel   [path new|off|<путь>]   — адрес панели, секретный путь (UI_PREFIX)
//   log     [seen] [поиск] [-n N]   — журнал VPN-подключений этого сервера, «кто когда был»
//   metrics [token|revoke]          — токен «только метрики» для своего Prometheus
//   hub     [legacy on|off|auto]    — хаб нод: подпись до апгрейда, кто из нод ещё старый
//   check                           — что на этом сервере выдаёт его сканеру и DPI

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/migrate"
)

// pad — дополнение пробелами по числу символов, не байт: в Printf %-Ns кириллица съезжает.
func pad(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

func yellow(s string) string { return "\x1b[33m" + s + cReset }

func (c *CLI) hasPanel() bool { return contains(c.all, "ui") || env("UI_PORT", "") != "" && !c.isNode }
func (c *CLI) hasVPN() bool   { return contains(c.all, "awgctrl") }

func (c *CLI) uiDB() (*sql.DB, error) {
	f := c.svcEnv("ui", "UI_DB_FILE")
	if !exists(f) {
		return nil, fmt.Errorf("базы панели нет (%s)", f)
	}
	return migrate.OpenDB(f)
}

// restartUI — cli.env изменён: панель перезапускается с новым значением, VPN не трогаем.
func (c *CLI) restartUI(key, value string) {
	c.setSvcEnv("ui", key, value)
	if c.isRunning("ui") {
		c.stop("ui")
		c.start("ui")
	}
}

func when(ts int64) string {
	t := time.Unix(ts, 0)
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "сегодня " + t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("02.01 15:04")
	}
	return t.Format("02.01.2006 15:04")
}

func dur(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%d сек", s)
	case s < 3600:
		return fmt.Sprintf("%d мин", s/60)
	case s < 86400:
		return fmt.Sprintf("%d ч %d мин", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%d дн %d ч", s/86400, s%86400/3600)
}

func bytesStr(n float64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%.0f B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", n/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", n/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", n/(1<<30))
}

// ── panel ───────────────────────────────────────────────────────────────────

var rePrefix = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func randPrefix() string {
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b)
}

func (c *CLI) panelCmd(args []string) {
	if !c.hasPanel() {
		fmt.Println("  Панели на этой машине нет — это нода")
		return
	}
	if len(args) == 0 {
		c.showPanel()
		return
	}
	if args[0] != "path" || len(args) < 2 {
		errorf("  Usage: awg-ctrl panel [path new|off|<путь>]")
		os.Exit(1)
	}
	c.setPanelPath(args[1])
}

func (c *CLI) showPanel() {
	fmt.Printf("\n  Панель   %s\n", bold(panelURL()))
	if p := env("UI_PREFIX", ""); p != "" {
		fmt.Println("  " + dim("Секретный путь: снаружи без него — 404. Сменить: awg-ctrl panel path new"))
	} else {
		fmt.Println("  " + yellow("Без секретного пути: сканер узнаёт панель по странице логина.") + " " + dim("Включить: awg-ctrl panel path new"))
	}
	if m := env("UI_PREFIX_MOVED", ""); m != "" {
		fmt.Println("  " + dim("Принимается и путь переехавшей панели: /"+m+"/"))
	}
	fmt.Println()
}

// setPanelPath — new (случайный), off (без пути) или свой. Закладки и адрес внешнего API
// со старым путём перестанут работать — предупреждаем.
func (c *CLI) setPanelPath(arg string) {
	v := arg
	switch arg {
	case "new":
		v = randPrefix()
	case "off":
		v = ""
	default:
		if !rePrefix.MatchString(arg) {
			errorf("  Путь: 8–64 символа A–Z a–z 0–9 _ -, или new / off")
			os.Exit(1)
		}
	}
	if err := c.writeEnvFile(updateEnvVar(c.readEnvFile(), "UI_PREFIX", v)); err != nil {
		errorf("  Не удалось записать %s: %v", c.envFile, err)
		os.Exit(1)
	}
	os.Setenv("UI_PREFIX", v)
	c.restartUI("UI_PREFIX", v)
	fmt.Printf("\n  %s Панель: %s\n", green("✓"), bold(panelURL()))
	fmt.Println("  " + dim("Старый адрес больше не открывается: обновите закладки и адрес внешнего API и Prometheus."))
	fmt.Println()
}

// ── log ─────────────────────────────────────────────────────────────────────

var reOwner = regexp.MustCompile(`^(?:([a-z0-9]{8,16})/)?m(\d+)/d(\d+)$`)

// ownerName — «устройство · ключ» для пира sen://, если его панель — на этой машине.
func (c *CLI) ownerName(db *sql.DB, owner string) string {
	m := reOwner.FindStringSubmatch(owner)
	if m == nil || db == nil || m[1] != "" && m[1] != c.localPanelID() {
		return owner
	}
	var label, dev string
	db.QueryRow("SELECT label FROM master_keys WHERE id = ?", m[2]).Scan(&label)
	db.QueryRow("SELECT device_name FROM devices WHERE id = ?", m[3]).Scan(&dev)
	switch {
	case dev != "" && label != "":
		return dev + " · «" + label + "»"
	case label != "":
		return "удалённое устройство · «" + label + "»"
	}
	return "удалённое устройство"
}

func (c *CLI) logCmd(args []string) {
	if !c.hasVPN() {
		fmt.Println("  VPN на этой машине нет — журнал серверов во вкладке «Подключения» панели")
		return
	}
	seen, limit, q := false, 50, ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "seen":
			seen = true
		case a == "-n" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				errorf("  -n: число событий")
				os.Exit(1)
			}
			limit, i = min(n, 500), i+1
		default:
			q = strings.TrimSpace(q + " " + a)
		}
	}
	v := url.Values{"limit": {strconv.Itoa(limit)}}
	if q != "" && !seen {
		v.Set("q", q)
	}
	r, err := c.ctrlCall("GET", "/api/connlog?"+v.Encode(), nil)
	if err != nil {
		errorf("  awg-ctrl недоступен: %v", err)
		os.Exit(1)
	}
	if r.status == 404 {
		errorf("  awg-ctrl этой версии журнала не ведёт — обновите сервер: awg-ctrl update")
		os.Exit(1)
	}
	if r.status != 200 {
		errorf("  %v", ctrlError(r))
		os.Exit(1)
	}
	db, _ := c.uiDB()
	if db != nil {
		defer db.Close()
	}
	who := func(o any) (string, string) {
		name, _ := get(o, "name").(string)
		ip, _ := get(o, "ip").(string)
		if name == "" {
			if owner, _ := get(o, "owner").(string); owner != "" {
				name = c.ownerName(db, owner)
			} else {
				pub, _ := get(o, "pub_key").(string)
				name = "ключ " + pub[:min(10, len(pub))] + "…"
			}
		}
		return name, ip
	}
	fmt.Println()
	if since, ok := get(r.data, "since").(float64); ok {
		fmt.Printf("  %s\n\n", dim(fmt.Sprintf("Журнал ведётся с %s, хранится %v дней. Время отключения — последняя активность (±30 с).",
			when(int64(since)), get(r.data, "keep_days"))))
	}

	if seen {
		type row struct {
			name, ip string
			online   bool
			last     int64
		}
		var rows []row
		peers, _ := get(r.data, "peers").([]any)
		for _, p := range peers {
			name, ip := who(p)
			if q != "" && !strings.Contains(strings.ToLower(name+" "+ip), strings.ToLower(q)) {
				continue
			}
			last, _ := get(p, "last_seen").(float64)
			online, _ := get(p, "online").(bool)
			rows = append(rows, row{name, ip, online, int64(last)})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].online != rows[j].online {
				return rows[i].online
			}
			return rows[i].last > rows[j].last
		})
		for _, x := range rows {
			state := dim("не подключался с начала журнала")
			switch {
			case x.online:
				state = green("онлайн")
			case x.last > 0:
				state = "был " + when(x.last) + dim(" ("+dur(time.Now().Unix()-x.last)+" назад)")
			}
			fmt.Printf("  %s %s %s\n", pad(x.name, 32), dim(pad(x.ip, 12)), state)
		}
		if len(rows) == 0 {
			fmt.Println("  " + dim("Никого"))
		}
		fmt.Println()
		return
	}

	events, _ := get(r.data, "events").([]any)
	if len(events) == 0 {
		fmt.Println("  " + dim("Событий нет"))
	}
	// Старые сверху: читается как лента.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		at, _ := get(e, "at").(float64)
		name, ip := who(e)
		mark, tail := green("→ подключился"), ""
		if get(e, "event") == "disconnect" {
			mark = dim("← отключился ")
			d, _ := get(e, "duration").(float64)
			rx, _ := get(e, "rx").(float64)
			tx, _ := get(e, "tx").(float64)
			tail = dim(fmt.Sprintf("  %s · ↓ %s ↑ %s", dur(int64(d)), bytesStr(tx), bytesStr(rx)))
		}
		fmt.Printf("  %s %s  %s %s%s\n", pad(when(int64(at)), 15), mark, name, dim(ip), tail)
	}
	fmt.Printf("\n  %s\n\n", dim("Больше: awg-ctrl log -n 200 · по имени: awg-ctrl log <имя> · кто когда был: awg-ctrl log seen"))
}

// ── metrics ─────────────────────────────────────────────────────────────────

func (c *CLI) metricsCmd(args []string) {
	if !c.hasPanel() {
		fmt.Println("  Панели на этой машине нет — токен метрик выдаёт панель")
		return
	}
	db, err := c.uiDB()
	if err != nil {
		errorf("  %v", err)
		os.Exit(1)
	}
	defer db.Close()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		var prefix string
		var created int64
		var last sql.NullInt64
		if db.QueryRow("SELECT prefix, created_at, last_used FROM metrics_token WHERE id = 1").Scan(&prefix, &created, &last) != nil {
			fmt.Print("\n  Токена для внешнего Prometheus нет. Выдать: awg-ctrl metrics token\n\n")
			return
		}
		used := "ещё не было"
		if last.Valid && last.Int64 > 0 {
			used = when(last.Int64)
		}
		fmt.Printf("\n  Токен %s · выдан %s · последний скрейп %s\n", bold(prefix), when(created), used)
		fmt.Print("  " + dim("Перевыпустить: awg-ctrl metrics token · отозвать: awg-ctrl metrics revoke") + "\n\n")
	case "token":
		b := make([]byte, 24)
		rand.Read(b)
		tok := "awgm_" + b64url.EncodeToString(b)
		h := sha256.Sum256([]byte(tok))
		if _, err := db.Exec("INSERT OR REPLACE INTO metrics_token (id, token_hash, prefix, created_at, last_used) VALUES (1, ?, ?, ?, NULL)",
			h[:], tok[:13]+"…", time.Now().Unix()); err != nil {
			errorf("  ui.db: %v (панель этой версии ещё не создала таблицу — запустите её)", err)
			os.Exit(1)
		}
		u, _ := url.Parse(panelURL())
		fmt.Printf("\n  %s Токен (показывается один раз, прежний больше не работает):\n\n    %s\n\n", green("✓"), bold(tok))
		fmt.Print("  Фрагмент prometheus.yml:\n\n")
		fmt.Printf("    scrape_configs:\n      - job_name: vpn-panel\n        metrics_path: %smetrics\n        authorization:\n          credentials: %s\n        static_configs:\n          - targets: ['%s']\n\n",
			u.Path, tok, u.Host)
		fmt.Print("  " + dim("По голому HTTP токен и метрики идут открытым текстом — скрейпьте через nginx с TLS или по VPN.") + "\n\n")
	case "revoke":
		db.Exec("DELETE FROM metrics_token WHERE id = 1")
		fmt.Printf("  %s Токен отозван\n", green("✓"))
	default:
		errorf("  Usage: awg-ctrl metrics [token|revoke]")
		os.Exit(1)
	}
}

// ── hub ─────────────────────────────────────────────────────────────────────

func (c *CLI) hubCmd(args []string) {
	if !c.hasPanel() || env("NODE_PORT", "") == "" {
		fmt.Println("  Хаб нод на этой машине не включён (NODE_PORT)")
		return
	}
	if len(args) > 0 {
		if args[0] != "legacy" || len(args) < 2 || !regexp.MustCompile(`^(on|off|auto)$`).MatchString(args[1]) {
			errorf("  Usage: awg-ctrl hub [legacy on|off|auto]")
			os.Exit(1)
		}
		v := args[1]
		if v == "auto" {
			v = ""
		}
		if err := c.writeEnvFile(updateEnvVar(c.readEnvFile(), "HUB_LEGACY", v)); err != nil {
			errorf("  %v", err)
			os.Exit(1)
		}
		os.Setenv("HUB_LEGACY", v)
		c.restartUI("HUB_LEGACY", v)
		fmt.Printf("  %s HUB_LEGACY=%s\n", green("✓"), args[1])
	}
	db, err := c.uiDB()
	if err != nil {
		errorf("  %v", err)
		os.Exit(1)
	}
	defer db.Close()
	rows, err := db.Query("SELECT id, name, COALESCE(version, ''), COALESCE(last_seen, 0), COALESCE(preauth, 0), pub IS NOT NULL FROM nodes ORDER BY id")
	if err != nil {
		errorf("  ui.db: %v", err)
		os.Exit(1)
	}
	type node struct {
		id              int64
		name, version   string
		seen            int64
		preauth, joined bool
	}
	var list []node
	for rows.Next() {
		var n node
		if rows.Scan(&n.id, &n.name, &n.version, &n.seen, &n.preauth, &n.joined) == nil {
			list = append(list, n)
		}
	}
	rows.Close()
	old := 0
	fmt.Printf("\n  Хаб нод :%s\n\n", env("NODE_PORT", ""))
	for _, n := range list {
		state := green(pad("с подписью", 28))
		if !n.preauth {
			state, old = yellow(pad("старый агент — без подписи", 28)), old+1
			if !n.joined {
				state = dim(pad("ещё не подключалась", 28))
			}
		}
		seen := dim("не подключалась")
		if n.seen > 0 {
			seen = "был " + when(n.seen)
		}
		fmt.Printf("  #%-3d %s %s %s %s\n", n.id, pad(n.name, 20), dim(pad(n.version, 12)), state, seen)
	}
	if len(list) == 0 {
		fmt.Println("  " + dim("Нод нет"))
	}
	mode := env("HUB_LEGACY", "")
	fmt.Println()
	switch {
	case mode == "on":
		fmt.Println("  " + yellow("HUB_LEGACY=on: хаб пускает агентов без подписи — сканер узнаёт его по /node/v1"))
	case mode == "off":
		fmt.Println("  HUB_LEGACY=off: только с подписью. Старые агенты не подключатся, пока ноду не обновят")
	case old > 0:
		fmt.Println("  " + yellow(fmt.Sprintf("Переходный режим: %d нод(ы) без подписи — хаб пускает и без неё. Обновите их: awg-ctrl update на ноде", old)))
	default:
		fmt.Println("  " + green("Все ноды с подписью — без неё хаб отвечает 404"))
	}
	fmt.Print("  " + dim("Принудительно: awg-ctrl hub legacy on|off|auto") + "\n\n")
}

// ── check ───────────────────────────────────────────────────────────────────

// Прежние общие параметры установок до рандомизации — по ним сервер узнаётся DPI.
var legacyAWG = map[string]string{"ListenPort": "47619", "S1": "90", "S2": "45", "H1": "1224800044-2116730834"}

func readConf(file string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	for _, l := range strings.Split(strings.SplitN(string(raw), "[Peer]", 2)[0], "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func certCN(file string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return ""
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return ""
	}
	return crt.Subject.CommonName
}

// checkCmd — сводка: что на этом сервере выдаёт его сканеру (активное зондирование) и DPI.
func (c *CLI) checkCmd() {
	ok := func(s string) { fmt.Println("  " + green("✓") + "  " + s) }
	bad := func(s, fix string) { fmt.Println("  " + yellow("⚠") + "  " + s + "\n       " + dim(fix)) }
	fmt.Println()
	if c.hasVPN() {
		conf := readConf(filepath.Join(env("AWG_CONF_DIR", "/etc/amnezia/amneziawg"), "awg1.conf"))
		switch {
		case conf == nil:
			bad("awg1.conf не прочитан", "запустите от root")
		default:
			var same []string
			for k, v := range legacyAWG {
				if conf[k] == v {
					same = append(same, k)
				}
			}
			sort.Strings(same)
			if len(same) > 0 {
				bad("VPN: общие для старых установок значения ("+strings.Join(same, ", ")+") — по ним DPI находит все такие серверы сразу",
					"переустановка без сохранения данных (awg-ctrl reinstall) и раздача новых ключей")
			} else {
				ok("VPN: порт и параметры обфускации свои у этого сервера")
			}
			if conf["I1"] == "" {
				bad("VPN: нет своего I1 — ключи получают общий пакет-маскировку", "awg-ctrl reinstall с сохранением данных выдаст свой — старые ключи продолжат работать")
			}
		}
	}
	if c.hasPanel() {
		if env("UI_PREFIX", "") != "" {
			ok("Панель: секретный путь — снаружи без него 404")
		} else {
			bad("Панель без секретного пути — сканер узнаёт её по странице логина", "awg-ctrl panel path new")
		}
		bad("Панель по голому HTTP: пути, JSON и токены видны DPI", "nginx с TLS перед панелью (README, «Панель за nginx») или вход по VPN")
		if strings.ToLower(env("SUB_TLS", "off")) == "on" {
			ok("Подписка sen:// по HTTPS")
		} else {
			bad("Подписка sen:// по HTTP: ответы подписаны, но видны DPI", "переустановка с SUB_TLS=on — ссылки придётся выдать заново")
		}
		if cn := certCN(c.svcEnv("ui", "SUB_TLS_CERT_FILE")); cn == "sen" {
			bad("Сертификат подписки с общим именем «sen»", "имя меняется только с ключом — при переустановке без сохранения данных")
		}
		if env("NODE_PORT", "") != "" {
			if cn := certCN(c.svcEnv("ui", "NODE_TLS_CERT_FILE")); cn == "awg-node" {
				bad("Сертификат хаба с общим именем «awg-node»", "имя меняется только с ключом — ноды придётся подключить заново")
			}
			switch strings.ToLower(env("HUB_LEGACY", "")) {
			case "on":
				bad("Хаб пускает агентов без подписи (HUB_LEGACY=on)", "awg-ctrl hub legacy auto")
			case "off":
				ok("Хаб: только агенты с подписью")
			default:
				ok("Хаб: подпись до апгрейда; кто из нод ещё без неё — awg-ctrl hub")
			}
		}
	}
	fmt.Print("\n  " + dim("Фаервол хостера (панель провайдера) отсюда не виден — открыты ли там лишние порты, проверьте сами.") + "\n\n")
}

// panelMenu — пункт меню: адрес панели и смена секретного пути.
func (c *CLI) panelMenu() {
	if !c.hasPanel() {
		fmt.Println("  Панели на этой машине нет — это нода")
		return
	}
	c.showPanel()
	fmt.Println("    1) Новый случайный путь   2) Свой путь   3) Без пути   Enter — назад")
	switch strings.TrimSpace(c.ask("  › ")) {
	case "1":
		c.setPanelPath("new")
	case "2":
		if p := strings.TrimSpace(c.ask("  Путь (8–64 символа A–Z a–z 0–9 _ -): ")); p != "" {
			c.setPanelPath(p)
		}
	case "3":
		if c.confirmYes("  Без пути сканер снова узнает панель по странице логина. Убрать путь?") {
			c.setPanelPath("off")
		}
	}
}
