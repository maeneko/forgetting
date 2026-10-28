package cli

// `migrate export|import|cancel` — перенос панели с сервера «панель + VPN» на отдельную панель
// без VPN; архив и раскладка — internal/migrate (их же зовёт вкладка «Перенос»):
//
//	старый: migrate export   → архив: ui.db, ключи подписки и хаба, порты
//	новый:  migrate import   → раскладывает архив, заводит ноду #0 и печатает join-строку
//	старый: join '<строка>'  → становится нодой #0 и поднимает ретранслятор прежних портов

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/migrate"
)

func envPort(k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k)))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

func (c *CLI) paths() migrate.Paths {
	return migrate.PathsFrom(func(env string) string { return c.svcEnv("ui", env) })
}

func (c *CLI) migrateCmd(sub, arg string, hasArg bool) {
	switch sub {
	case "export":
		c.migrateExport(arg, hasArg)
	case "import":
		if !hasArg {
			errorf("  Usage: awg-ctrl migrate import <архив>")
			os.Exit(1)
		}
		c.migrateImport(arg)
	case "cancel":
		c.migrateCancel()
	default:
		errorf("  Usage: awg-ctrl migrate <export [файл]|import <архив>|cancel>")
		os.Exit(1)
	}
}

// ── export (старый сервер) ─────────────────────────────────────────────────

func (c *CLI) migrateExport(out string, hasOut bool) {
	if c.isNode {
		errorf("  Это нода: её панель — на core, переносить отсюда нечего")
		os.Exit(1)
	}
	if !contains(c.svcList(), "awgctrl") {
		errorf("  У этой панели нет своего VPN (LOCAL_NODE=off) — перенос рассчитан на сервер «панель + VPN»")
		os.Exit(1)
	}
	dbFile := c.paths()["ui.db"]
	if !exists(dbFile) {
		errorf("  Нет базы панели: %s", dbFile)
		os.Exit(1)
	}
	if !hasOut {
		out = fmt.Sprintf("forgetting-migrate-%s.tar.gz", time.Now().Format("20060102-150405"))
	}

	fmt.Println()
	fmt.Println("  Перенос панели на отдельный сервер. Сейчас:")
	fmt.Println("    · панель и подписка sen:// здесь остановятся и больше не запустятся сами;")
	fmt.Println("    · VPN продолжит работать: пиры и awg-ctrl не трогаются;")
	fmt.Println("    · архив с базой панели и ключами подписки — " + bold(out))
	if !c.confirmYes("  Продолжить?") {
		return
	}

	c.stop("ui")
	// Флаг — до снимка: перезапуск между ними поднял бы панель с базой, которая уже уехала.
	content := updateEnvVar(c.readEnvFile(), "PANEL_MOVED", "on")
	if err := c.writeEnvFile(content); err != nil {
		errorf("  Не удалось записать %s: %v", c.envFile, err)
		os.Exit(1)
	}
	if err := c.writeExport(out, dbFile); err != nil {
		errorf("  Архив не создан: %v", err)
		c.writeEnvFile(removeEnvVar(c.readEnvFile(), "PANEL_MOVED"))
		os.Remove(out)
		c.start("ui")
		os.Exit(1)
	}

	fmt.Printf("\n  %s Архив: %s %s\n", green("✓"), out, dim("(в нём ключ подписи — храните как пароль, после переноса удалите)"))
	fmt.Println("\n  Дальше:")
	fmt.Println("    1. Новая панель: install.sh, роль «только панель»")
	fmt.Println("    2. Скопируйте архив туда:    " + dim("scp "+out+" root@<новая-панель>:"))
	fmt.Println("    3. На новой панели:          " + bold("awg-ctrl migrate import "+filepath.Base(out)))
	fmt.Println("    4. Здесь — строку, которую она напечатает: " + bold("awg-ctrl join '<строка>'"))
	fmt.Println("  " + dim("Передумали до шага 4: awg-ctrl migrate cancel"))
}

// svcList — сервисы роли без поправки на PANEL_MOVED и ретранслятор.
func (c *CLI) svcList() []string {
	return without(without(c.all, "relay"), "ui")
}

func (c *CLI) writeExport(out, dbFile string) error {
	db, err := migrate.OpenDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()
	name, ip := "VPN", os.Getenv("SERVER_IP")
	if h := fetchHealth("http://127.0.0.1:" + c.envOf("awgctrl") + "/health"); h != nil {
		name = strOr(get(h, "server"), name)
		if ip == "" {
			ip = strOr(get(h, "ip"), "")
		}
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = migrate.Write(f, db, c.paths(), exportMeta(c.version, name, ip, envPort("UI_PORT"), envPort("SUB_PORT"),
		strings.ToLower(env("SUB_TLS", "off")) == "on", envPort("NODE_PORT"), env("UI_PREFIX", "")))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// exportMeta — meta.json архива (v и panel_id добавляет migrate.Write).
func exportMeta(version, name, ip string, uiPort, subPort int, subTLS bool, nodePort int, uiPrefix string) jsjson.Object {
	return jsjson.Object{
		{Key: "version", Val: version}, {Key: "created_at", Val: time.Now().Unix()},
		{Key: "server_name", Val: name}, {Key: "server_ip", Val: ip},
		{Key: "ui_port", Val: uiPort}, {Key: "sub_port", Val: subPort}, {Key: "sub_tls", Val: subTLS}, {Key: "node_port", Val: nodePort},
		{Key: "ui_prefix", Val: uiPrefix},
	}
}

// ── import (новая панель) ──────────────────────────────────────────────────

func (c *CLI) migrateImport(file string) {
	if c.isNode {
		errorf("  Это нода — импортировать панель нужно на core")
		os.Exit(1)
	}
	if contains(c.svcList(), "awgctrl") {
		errorf("  У этой панели свой VPN (LOCAL_NODE=on). Импорт — на панель без VPN: install.sh, роль «только панель»")
		os.Exit(1)
	}
	f, err := os.Open(file)
	if err != nil {
		errorf("  %v", err)
		os.Exit(1)
	}
	b, err := migrate.Read(f)
	f.Close()
	if err != nil {
		errorf("  %v", err)
		os.Exit(1)
	}
	defer b.Close()

	paths := c.paths()
	if n, err := migrate.PanelData(paths["ui.db"]); err != nil || n > 0 {
		errorf("  На этой панели уже есть данные (%s) — импорт их затёр бы. Нужна чистая панель.", paths["ui.db"])
		os.Exit(1)
	}
	host := strings.TrimSpace(os.Getenv("SUB_HOST"))
	for host == "" || !reJoinHost.MatchString(host) {
		host = strings.TrimSpace(c.ask("  Публичный адрес этой панели (IP или домен): "))
		if host == "" {
			return
		}
	}
	plan, err := migrate.NewPlan(b, envPort, envPort("UI_PORT"), host)
	if err != nil {
		errorf("  %v (%s)", err, c.envFile)
		os.Exit(1)
	}

	fmt.Println()
	from := b.Str("server_ip")
	if v := b.Str("version"); v != "" {
		from += ", версия " + v
	}
	fmt.Printf("  Импорт панели «%s» (%s)\n", b.Str("server_name"), from)
	fmt.Printf("    · подписка :%d, хаб нод :%d, адрес панели %s\n", plan.SubPort, plan.NodePort, host)
	fmt.Println("    · прежние файлы панели уйдут в *.bak-<время>")
	if !b.Has["sub_sign.key"] {
		fmt.Println("    · " + dim("ключа подписи в архиве нет — на старом сервере подписка не была настроена"))
	}
	if !c.confirmYes("  Продолжить?") {
		return
	}

	c.stop("ui")
	secret := make([]byte, 16)
	rand.Read(secret)
	if err := migrate.Install(b, paths); err != nil {
		errorf("  %v", err)
		os.Exit(1)
	}
	if err := migrate.AddNodeZero(paths["ui.db"], b.Str("server_name"), secret); err != nil {
		errorf("  ui.db: %v", err)
		os.Exit(1)
	}
	content := c.readEnvFile()
	for _, kv := range plan.Env {
		content = updateEnvVar(content, kv[0], kv[1])
	}
	if err := c.writeEnvFile(content); err != nil {
		errorf("  Не удалось записать %s: %v", c.envFile, err)
		os.Exit(1)
	}
	// Подписка и хаб переезжают на порты старого сервера — их надо открыть, как install.sh.
	opened, failed := migrate.OpenFirewall(plan.SubPort, plan.NodePort)
	if len(opened) > 0 {
		fmt.Printf("  %s UFW: открыты %s\n", green("✓"), strings.Join(opened, " "))
	}
	if len(failed) > 0 {
		errorf("  UFW: не удалось открыть %s — откройте вручную: ufw allow <порт>/tcp", strings.Join(failed, " "))
	}
	c.restartFresh()

	// Сертификат хаба приехал в архиве, а нет — awg-ui создаёт его при старте.
	var pin string
	for range 50 {
		if pin, err = migrate.CertPin(paths["node_tls.crt"]); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		errorf("  Нет сертификата хаба %s — проверьте лог awg-ui и выдайте строку в панели (сервер #0)", paths["node_tls.crt"])
		os.Exit(1)
	}
	join := migrate.JoinString(host, plan.NodePort, pin, 0, secret, plan.UIPort, plan.SubPort)
	fmt.Printf("\n  %s Панель перенесена. На старом сервере выполните (строка действует час):\n\n", green("✓"))
	fmt.Println("    awg-ctrl join '" + join + "'")
	fmt.Println()
	fmt.Println("  " + dim("Старый сервер станет нодой #0 и будет пересылать сюда прежние порты — ссылки sen://, ноды и"))
	fmt.Println("  " + dim("внешний API продолжат работать. Логин и пароль панели — этой установки, а не старой."))
	if !migrate.ValidUIPrefix(b.Str("ui_prefix")) && env("UI_PREFIX", "") != "" {
		fmt.Println("  " + dim("У старой панели не было секретного пути: её закладкам и внешнему API нужен путь этой — /"+env("UI_PREFIX", "")+"/"))
	}
}

// ── cancel (старый сервер) ─────────────────────────────────────────────────

func (c *CLI) migrateCancel() {
	if !c.panelMoved() {
		fmt.Println("  Переноса не было: панель на месте")
		return
	}
	if c.isNode {
		errorf("  Сервер уже нода новой панели — база там, и вернуть её сюда так нельзя.")
		errorf("  Если всё же нужно: awg-ctrl unjoin, затем верните ui.db с новой панели вручную.")
		os.Exit(1)
	}
	content := c.readEnvFile()
	for _, k := range []string{"PANEL_MOVED", "RELAY"} {
		content = removeEnvVar(content, k)
	}
	c.writeEnvFile(content)
	fmt.Printf("  %s Перенос отменён, панель запускается здесь снова\n", green("✓"))
	fmt.Println("  " + dim("Если архив уже импортирован на новую панель — не запускайте там ui, пока она не очищена"))
	c.restartFresh()
}

// ── rehome (нода) ──────────────────────────────────────────────────────────

// rehome — панель переехала на новый адрес: тот же pin и id, другой CORE_HOST[:CORE_PORT].
func (c *CLI) rehome(addr string, hasAddr bool) {
	if !c.isNode {
		errorf("  Это не нода — менять адрес core нечему")
		os.Exit(1)
	}
	host, port := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, port = h, p
	}
	if p, err := strconv.Atoi(port); !hasAddr || !reJoinHost.MatchString(host) || port != "" && (err != nil || p <= 0 || p > 65535) {
		errorf("  Usage: awg-ctrl rehome <хост[:порт]>   (адрес новой панели; pin и id ноды остаются)")
		os.Exit(1)
	}
	content := updateEnvVar(c.readEnvFile(), "CORE_HOST", host)
	if port != "" {
		content = updateEnvVar(content, "CORE_PORT", port)
	} else {
		port = os.Getenv("CORE_PORT")
	}
	// Адрес, указанный руками, главнее того, что сообщила панель.
	c.writeEnvFile(removeEnvVar(content, "CORE_ENDPOINT"))
	fmt.Printf("  %s Core: %s:%s\n", green("✓"), host, port)
	c.restartFresh()
}
