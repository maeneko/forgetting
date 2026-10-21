package cli

// `awg-ctrl monitor install|status|remove` — встроенный Prometheus для вкладки «Монитор» (порт
// cli/src/monitor.ts). Работает от root (ставит пакет, пишет в /etc): панель сама пакетов не
// ставит, она только просит (через awg-ctrl) или показывает эту команду. Всё слушает
// 127.0.0.1: наружу Prometheus не торчит, а панель читает его по HTTP на той же машине.
// На ноде без своей панели и скрейп, и чтение идут через агента: метрики панели он берёт у
// core по WSS (AGENT_METRICS_PORT), а запросы вкладки «Монитор» пропускает к Prometheus.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	awgDir          = "/etc/amnezia/amneziawg"
	promDir         = "/etc/prometheus"
	promConf        = promDir + "/prometheus.yml"
	promToken       = promDir + "/forgetting.token"
	monitorHashFile = awgDir + "/monitor_token.sha256" // хэш локального токена: его читает awg-ui
	promDefaults    = "/etc/default/prometheus"
	promMarker      = "# managed by forgetting"
	promListen      = "127.0.0.1:9090"
	promRetention   = "31d" // с запасом на вид «30 дней» во вкладке «Монитор»
)

func mlog(s string) { fmt.Println("  " + s) }

func mfail(s string) {
	fmt.Fprintf(os.Stderr, "\n  ✗ %s\n\n", s)
	os.Exit(1)
}

type shResult struct {
	ok     bool
	output string
}

// sh — команда без stdin; quiet — вывод забираем себе, иначе он идёт в терминал.
func sh(quiet bool, name string, args ...string) shResult {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if quiet {
		out, err := cmd.CombinedOutput()
		return shResult{err == nil, string(out)}
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return shResult{cmd.Run() == nil, ""}
}

var (
	reOSID     = regexp.MustCompile(`(?m)^ID=(.*)$`)
	reOSIDLike = regexp.MustCompile(`(?m)^ID_LIKE=(.*)$`)
	reDebian   = regexp.MustCompile(`(?i)debian|ubuntu`)
	rePromVer  = regexp.MustCompile(`version (\d+)\.(\d+)`)
	reArgsLine = regexp.MustCompile(`(?m)^ARGS=.*$`)
)

func osFamily() string {
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	val := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(string(raw)); m != nil {
			return strings.ReplaceAll(m[1], `"`, "")
		}
		return ""
	}
	return val(reOSID) + " " + val(reOSIDLike)
}

func promReady() bool {
	cl := &http.Client{Timeout: 1500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Get("http://" + promListen + "/-/ready")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == 200
}

func promInstalled() bool { return sh(true, "dpkg", "-s", "prometheus").ok }
func promActive() bool    { return sh(true, "systemctl", "is-active", "--quiet", "prometheus").ok }
func promManaged() bool {
	raw, err := os.ReadFile(promConf)
	return err == nil && strings.Contains(string(raw), promMarker)
}

// authBlock — новые схемы (authorization.credentials_file) есть с 2.26; в 3.x bearer_token_file убран.
func authBlock() string {
	r := sh(true, "prometheus", "--version")
	maj, min := 2, 0
	if m := rePromVer.FindStringSubmatch(r.output); m != nil {
		maj, _ = strconv.Atoi(m[1])
		min, _ = strconv.Atoi(m[2])
	}
	if maj > 2 || maj == 2 && min >= 26 {
		return "    authorization:\n      credentials_file: " + promToken + "\n"
	}
	return "    bearer_token_file: " + promToken + "\n"
}

// backupStamp — new Date().toISOString() с «:» и «.» → «-».
func backupStamp() string {
	return strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
}

func copyFile(src, dst string) {
	if raw, err := os.ReadFile(src); err == nil {
		os.WriteFile(dst, raw, 0o644)
	}
}

// writePromConfig — target: порт панели на этой машине или агента ноды.
func writePromConfig(target string) {
	if exists(promConf) {
		copyFile(promConf, promConf+".bak-"+backupStamp())
	}
	os.WriteFile(promConf, []byte(promMarker+`
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: forgetting
    scheme: http
    metrics_path: /metrics
`+authBlock()+`    static_configs:
      - targets: ['127.0.0.1:`+target+`']
`), 0o644)
}

// writePromToken — токен для самого Prometheus. Файл читает пользователь prometheus, в панели лежит только хэш.
func writePromToken() {
	b := make([]byte, 24)
	rand.Read(b)
	token := "awgm_" + b64url.EncodeToString(b)
	os.WriteFile(promToken, []byte(token), 0o640)
	sh(true, "chown", "root:prometheus", promToken)
	sum := sha256.Sum256([]byte(token))
	os.WriteFile(monitorHashFile, []byte(hex.EncodeToString(sum[:])+"\n"), 0o644)
}

func setPromArgs() {
	line := `ARGS="--web.listen-address=` + promListen + ` --storage.tsdb.retention.time=` + promRetention + `"`
	raw, _ := os.ReadFile(promDefaults)
	text := string(raw)
	if text != "" {
		copyFile(promDefaults, promDefaults+".bak-"+backupStamp())
	}
	if loc := reArgsLine.FindStringIndex(text); loc != nil {
		text = text[:loc[0]] + line + text[loc[1]:]
	} else {
		text = strings.TrimSuffix(text, "\n") + "\n" + line + "\n"
	}
	os.WriteFile(promDefaults, []byte(text), 0o644)
}

func (c *CLI) monitorInstall() {
	if os.Getuid() != 0 {
		mfail("Нужен root: sudo awg-ctrl monitor install")
	}
	if !reDebian.MatchString(osFamily()) {
		mfail("Автоустановка поддерживает только Debian и Ubuntu. На другом дистрибутиве поставьте Prometheus сами и подключите его кнопкой в панели (токен метрик).")
	}
	// Нода без своей панели (в том числе сервер, с которого панель переехала): метрики панели
	// Prometheus получает от агента, а тот — от core.
	viaAgent := c.isNode && !contains(c.all, "ui")
	target, newPort := os.Getenv("UI_PORT"), false
	if viaAgent {
		if target = os.Getenv("AGENT_METRICS_PORT"); target == "" {
			if target = freeLocalPort(); target == "" {
				mfail("Не нашлось свободного порта для агента")
			}
			if err := c.writeEnvFile(updateEnvVar(c.readEnvFile(), "AGENT_METRICS_PORT", target)); err != nil {
				mfail("Не удалось записать " + c.envFile + ": " + err.Error())
			}
			c.setSvcEnv("agent", "AGENT_METRICS_PORT", target)
			newPort = true
		}
	} else if target == "" {
		mfail("На этой машине нет панели (UI_PORT не задан): Prometheus скрейпит метрики панели.")
	}
	// Чужой Prometheus не трогаем: перезаписать его prometheus.yml значит сломать чужой мониторинг.
	if promInstalled() && exists(promConf) && !promManaged() {
		mfail("Prometheus уже установлен и настроен не нами (" + promConf + "). Подключите его кнопкой «Подключить Prometheus» во вкладке «Монитор»: она выдаст токен и готовый конфиг.")
	}

	mlog("1/4  установка пакета prometheus")
	if !promInstalled() {
		if !sh(false, "apt-get", "update", "-qq").ok {
			mfail("apt-get update завершился ошибкой")
		}
		if !sh(false, "apt-get", "install", "-y", "--no-install-recommends", "prometheus").ok {
			mfail("apt-get install prometheus завершился ошибкой")
		}
	} else {
		mlog("уже установлен")
	}

	mlog("2/4  токен и конфиг")
	writePromToken()
	writePromConfig(target)
	if newPort {
		mlog("агент отдаёт метрики панели на 127.0.0.1:" + target + " — перезапуск агента")
		c.stop("agent")
		c.start("agent")
	}

	mlog("3/4  слушаем только " + promListen + ", хранение " + promRetention)
	setPromArgs()

	mlog("4/4  запуск")
	sh(true, "systemctl", "enable", "prometheus")
	if !sh(false, "systemctl", "restart", "prometheus").ok {
		mfail("prometheus не запустился: journalctl -u prometheus")
	}
	for range 30 {
		if promReady() {
			fmt.Printf("\n  ✓ Prometheus работает на %s, метрики панели скрейпятся каждые 15 с.\n    Во вкладке «Монитор» графики появятся через минуту-две.\n\n", promListen)
			if viaAgent {
				fmt.Print("    Панель читает его через агента этой ноды.\n\n")
			}
			return
		}
		time.Sleep(time.Second)
	}
	mfail("Prometheus не ответил за 30 с: journalctl -u prometheus")
}

// freeLocalPort — свободный порт из того же диапазона, что у install.sh (32768–55535).
func freeLocalPort() string {
	for range 50 {
		n, err := rand.Int(rand.Reader, big.NewInt(55535-32768+1))
		if err != nil {
			return ""
		}
		p := strconv.Itoa(32768 + int(n.Int64()))
		if l, err := net.Listen("tcp", "127.0.0.1:"+p); err == nil {
			l.Close()
			return p
		}
	}
	return ""
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func monitorStatus() {
	ok := promReady()
	fmt.Println("  пакет:     " + yesNo(promInstalled(), "установлен", "нет"))
	fmt.Println("  сервис:    " + yesNo(promActive(), "работает", "остановлен"))
	fmt.Println("  наш конфиг: " + yesNo(promManaged(), "да", "нет"))
	fmt.Println("  готов:     " + yesNo(ok, "да, "+promListen, "нет"))
}

func monitorRemove() {
	if os.Getuid() != 0 {
		mfail("Нужен root: sudo awg-ctrl monitor remove")
	}
	// Чужой Prometheus не удаляем: у него могут быть свои данные и задачи, которых мы не ставили.
	if promInstalled() && exists(promConf) && !promManaged() {
		mfail("Prometheus настроен не нами (" + promConf + "), поэтому панель его не удаляет. Если он не нужен: sudo apt purge prometheus")
	}
	mlog("1/3  остановка сервиса")
	sh(true, "systemctl", "disable", "--now", "prometheus")
	mlog("2/3  токен")
	os.Remove(promToken)
	os.Remove(monitorHashFile)
	mlog("3/3  удаление пакета и накопленных данных")
	if promInstalled() && !sh(false, "apt-get", "purge", "-y", "prometheus").ok {
		mfail("apt-get purge prometheus завершился ошибкой")
	}
	fmt.Print("\n  ✓ Prometheus удалён вместе с накопленными графиками. Панель продолжает отдавать /metrics.\n\n")
}

func (c *CLI) monitorCmd(sub string) {
	switch sub {
	case "install":
		c.monitorInstall()
	case "status":
		monitorStatus()
	case "remove":
		monitorRemove()
	default:
		fmt.Println("Usage: awg-ctrl monitor <install|status|remove>")
		os.Exit(1)
	}
}
