package ctrl

// Telegram MTProto-прокси на ноде: awg-ctrl находит бинарник mtg (9seconds/mtg v2),
// генерирует секрет, пишет TOML-конфиг и systemd-юнит awg-mtproto.service, открывает
// порт в фаерволе сервера (ufw, firewalld или голый iptables — что найдётся) и запускает
// сервис. Ссылку t.me/proxy?… собирает из публичного адреса сервера (serverIp из config):
// её показывает панель, дальше админ раздаёт её пользователям сам.
//
// Состояние — в таблице config users.db (mtprotoPort, mtprotoSecret, mtprotoHost), как у
// шейпинга: переживает перезапуски, секрет не теряется. Сам mtg наружу ходит на
// Telegram напрямую; vpn-интерфейс и его правила он не трогает.
//
// Секрет — FakeTLS-серия: «ee» + 16 случайных байт в hex + hex(домена). Домен обязателен:
// mtg 2.x без него не стартует («hostname cannot be empty»), так что секрет старого
// формата («ee» + 32 hex, без хоста) перегенерируется при первом up/rotate. Домен —
// mtprotoHost, по умолчанию storage.googleapis.com. Бинарник ищется рядом с awg-ctrl,
// в /usr/local/bin, /usr/bin и PATH; нет его — оператору говорят поставить (install.sh
// качает его опционально).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	mtprotoService     = "awg-mtproto"
	mtprotoServicePath = "/etc/systemd/system/" + mtprotoService + ".service"

	// Версия mtg и место установки совпадают с install.sh (MTG_VERSION/MTG_BIN):
	// кнопка «Установить» в панели ставит тот же бинарник, что и установщик.
	mtgVersion     = "2.3.0"
	mtgBaseURL     = "https://github.com/9seconds/mtg/releases/download"
	mtgInstallPath = "/usr/local/bin/mtg"
	mtgMaxDownload = 64 << 20 // потолок распакованного бинарника; больше mtg не весит

	// mtprotoDefaultHost — FakeTLS-домен по умолчанию. mtg v2 требует непустой hostname
	// в секрете: «ee» + 32 hex без него бинарник отвергает на разборе конфига.
	mtprotoDefaultHost = "storage.googleapis.com"
)

// mtprotoRun / mtprotoSettleDelay — швы для тестов (в проде — runCmd из awg.go и 1 с
// ожидания после старта systemd). Поведение при этом не меняется.
var (
	mtprotoRun         = runCmd
	mtprotoSettleDelay = time.Second
)

// MTProtoFirewall — как порт прокси открыт на самом сервере. Фаервол хостера (панель
// провайдера, security group) отсюда не виден — о нём панель напоминает отдельно.
type MTProtoFirewall struct {
	Backend  string `json:"backend"` // ufw | firewalld | iptables | none
	PortOpen bool   `json:"port_open"`
	Error    string `json:"error,omitempty"`
}

type MTProtoStatus struct {
	Port       int             `json:"port"`                  // настроенный порт; 0 — прокси не поднимали
	Secret     string          `json:"secret"`                // «ee…» + hex(FakeTLS-домен)
	Host       string          `json:"host"`                  // FakeTLS-домен из mtprotoHost
	Running    bool            `json:"running"`               // systemd: active
	Link       string          `json:"link,omitempty"`        // https://t.me/proxy?… — только когда сервис работает
	LinkReason string          `json:"link_reason,omitempty"` // почему ссылки нет: «прокси остановлен» / «нужен бинарник mtg»
	Binary     string          `json:"binary"`                // найденный mtg; "" — не найден
	Managed    bool            `json:"managed"`               // бинарник стоит там, куда его кладёт панель; только его можно удалить
	Error      string          `json:"error,omitempty"`
	Firewall   MTProtoFirewall `json:"firewall"`
}

func mtprotoLink(ip string, port int, secret string) string {
	if ip == "" || port <= 0 || secret == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/proxy?server=%s&port=%d&secret=%s", ip, port, secret)
}

// mtprotoNewSecret — секрет mtg v2: «ee» + 16 случайных байт в hex + hex(домена).
// Домен внутри секрета — FakeTLS-hostname; без него mtg не стартует («hostname cannot be
// empty»). Ровно такой же секрет печатает `mtg generate-secret --hex <домен>`.
func mtprotoNewSecret(host string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("нет энтропии для секрета: %w", err)
	}
	return "ee" + hex.EncodeToString(b) + hex.EncodeToString([]byte(host)), nil
}

// mtprotoSecretHost — FakeTLS-домен, зашитый в секрет. ok=false — секрет старого формата
// («ee» + 32 hex, без хоста) или мусор: mtg такой конфиг не примет, а ссылка уже мертва,
// поэтому его надо перегенерировать, а не «починить».
func mtprotoSecretHost(secret string) (string, bool) {
	if !strings.HasPrefix(secret, "ee") {
		return "", false
	}
	body := secret[2:]
	if len(body) < 34 { // 32 hex случайных байт + хотя бы один байт домена
		return "", false
	}
	if _, err := hex.DecodeString(body[:32]); err != nil {
		return "", false
	}
	host, err := hex.DecodeString(body[32:])
	if err != nil || len(host) == 0 {
		return "", false
	}
	return string(host), true
}

// mtprotoHost — проверить и нормализовать FakeTLS-домен: ASCII-имя (a-z0-9.-), минимум
// одна точка, метки не начинаются и не заканчиваются дефисом, до 253 символов.
// Возвращает lowercase — в конфиг и секрет он попадает уже так.
func mtprotoHost(v string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(v))
	if h == "" {
		return "", errors.New("host: пустой")
	}
	if len(h) > 253 {
		return "", errors.New("host: длиннее 253 символов")
	}
	if strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return "", errors.New("host: не должен начинаться или заканчиваться точкой")
	}
	if !strings.Contains(h, ".") {
		return "", errors.New("host: нужен хотя бы один домен (точка)")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return "", errors.New("host: пустая метка домена")
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", errors.New("host: метка не должна начинаться или заканчиваться дефисом")
		}
		for _, r := range label {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
				continue
			}
			return "", fmt.Errorf("host: недопустимый символ %q", r)
		}
	}
	return h, nil
}

// mtprotoHostChoice — какой FakeTLS-домен брать: явный из запроса, иначе сохранённый
// mtprotoHost, иначе домен из уже лежащего секрета (ссылка продолжает работать),
// иначе mtprotoDefaultHost. Явный и сохранённый проходят валидацию (400 на кривой).
func (s *Server) mtprotoHostChoice(explicit, secret string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return mtprotoHost(explicit)
	}
	if v := s.getCfg("mtprotoHost", ""); v != "" {
		if h, err := mtprotoHost(v); err == nil {
			return h, nil
		}
	}
	if h, ok := mtprotoSecretHost(secret); ok {
		return h, nil
	}
	return mtprotoDefaultHost, nil
}

// mtprotoRepairSecret — секрет для конфига. Валидный и с тем же доменом остаётся как
// есть; старый (без FakeTLS-домена) или с другим доменом перегенерируется под host.
// regenerated=true — секрет сменился (up пишет об этом в лог и сохраняет новый).
func mtprotoRepairSecret(secret, host string) (string, bool, error) {
	if h, ok := mtprotoSecretHost(secret); ok && h == host {
		return secret, false, nil
	}
	s, err := mtprotoNewSecret(host)
	return s, secret != "", err
}

// mtprotoConf — TOML-конфиг mtg: секрет (FakeTLS-домен уже внутри) и адрес bind.
func mtprotoConf(secret string, port int) string {
	return fmt.Sprintf("secret = %q\nbind-to = %q\n", secret, fmt.Sprintf("0.0.0.0:%d", port))
}

// mtprotoUnit — systemd-юнит прокси. Restart=always: упавший mtg поднимается сам.
func mtprotoUnit(bin, conf string) string {
	return fmt.Sprintf(`[Unit]
Description=awg-mtproto — MTProto proxy (Telegram)
Documentation=https://github.com/9seconds/mtg
After=network.target

[Service]
ExecStart=%s run %s
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, bin, conf)
}

func mtprotoConfPath(s *Server) string { return filepath.Join(s.confDir, "mtproto.toml") }

// mtprotoFindBinary — где mtg: рядом с awg-ctrl, в стандартных путях, PATH.
func mtprotoFindBinary() string {
	if self, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(self), "mtg"); fileExec(p) {
			return p
		}
	}
	for _, p := range []string{"/usr/local/bin/mtg", "/usr/bin/mtg"} {
		if fileExec(p) {
			return p
		}
	}
	if p, err := exec.LookPath("mtg"); err == nil {
		return p
	}
	return ""
}

func fileExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// portTaken — LISTEN TCP на этом порту уже есть? (mtg слушает TCP.)
func (s *Server) portTaken(port int) bool {
	return portTakenFrom(mtprotoRun("ss", "-tln").Stdout, port)
}

// portTakenFrom — разбор вывода `ss -tln`; вынесен чистой функцией ради теста.
func portTakenFrom(out string, port int) bool {
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		// формат: LISTEN 0 128 0.0.0.0:443 0.0.0.0:* — нас интересует Local/Peer Address:Port
		for _, f := range strings.Fields(line) {
			if strings.HasSuffix(f, suffix) {
				return true
			}
		}
	}
	return false
}

// ───────── фаервол сервера ─────────

// mtprotoFirewallBackend — какой фаервол есть на сервере. Порядок тот же, что в
// install.sh: ufw → firewalld → голый iptables. Наличие, а не активность: правило,
// положенное в выключенный ufw, ничем не мешает и начнёт работать при включении.
func mtprotoFirewallBackend() string {
	for _, b := range []struct{ bin, backend string }{
		{"ufw", "ufw"}, {"firewall-cmd", "firewalld"}, {"iptables", "iptables"},
	} {
		if _, err := exec.LookPath(b.bin); err == nil {
			return b.backend
		}
	}
	return "none"
}

// mtprotoCmdErr — короткий текст отказа команды для оператора.
func mtprotoCmdErr(r cmdResult) string {
	if t := strings.Join(strings.Fields(r.Stderr), " "); t != "" {
		return t
	}
	if r.Code == -1 {
		return "команда не запустилась"
	}
	return fmt.Sprintf("код %d", r.Code)
}

// mtprotoFirewallOpen — открыть порт в фаерволе сервера. Отказ не валит запуск: он
// возвращается в статус (firewall) и пишется в лог — панель покажет предупреждение.
func (s *Server) mtprotoFirewallOpen(port int) MTProtoFirewall {
	fw := MTProtoFirewall{Backend: mtprotoFirewallBackend()}
	if port <= 0 {
		return fw
	}
	rule := fmt.Sprintf("%d/tcp", port)
	switch fw.Backend {
	case "ufw":
		if r := mtprotoRun("ufw", "allow", rule); !r.ok() {
			fw.Error = "ufw allow: " + mtprotoCmdErr(r)
			return fw
		}
	case "firewalld":
		if r := mtprotoRun("firewall-cmd", "--permanent", "--add-port="+rule); !r.ok() {
			fw.Error = "firewall-cmd --add-port: " + mtprotoCmdErr(r)
			return fw
		}
		if r := mtprotoRun("firewall-cmd", "--reload"); !r.ok() {
			fw.Error = "firewall-cmd --reload: " + mtprotoCmdErr(r)
			return fw
		}
	case "iptables":
		// -C не даёт дублировать правило при повторном POST на тот же порт.
		q := mtprotoRun("iptables", "-C", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT")
		if q.ok() {
			break
		}
		if q.Code != 1 {
			fw.Error = "iptables -C: " + mtprotoCmdErr(q)
			return fw
		}
		if r := mtprotoRun("iptables", "-I", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT"); !r.ok() {
			fw.Error = "iptables -I: " + mtprotoCmdErr(r)
			return fw
		}
	case "none":
		// Фаервола нет — порт открыт по умолчанию, открывать нечего.
	}
	fw.PortOpen = true
	return fw
}

// mtprotoFirewallClose — снять правило, открытое mtprotoFirewallOpen. Отказ не валит
// удаление прокси: правило просто останется, о нём пишем в лог.
func (s *Server) mtprotoFirewallClose(port int) {
	if port <= 0 {
		return
	}
	rule := fmt.Sprintf("%d/tcp", port)
	switch mtprotoFirewallBackend() {
	case "ufw":
		if r := mtprotoRun("ufw", "delete", "allow", rule); !r.ok() {
			s.log.Warn("ufw delete failed", "port", port, "stderr", r.Stderr)
		}
	case "firewalld":
		if r := mtprotoRun("firewall-cmd", "--permanent", "--remove-port="+rule); !r.ok() {
			s.log.Warn("firewall-cmd --remove-port failed", "port", port, "stderr", r.Stderr)
			return
		}
		if r := mtprotoRun("firewall-cmd", "--reload"); !r.ok() {
			s.log.Warn("firewall-cmd --reload failed", "port", port, "stderr", r.Stderr)
		}
	case "iptables":
		if !mtprotoRun("iptables", "-C", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT").ok() {
			return // правила нет — удалять нечего
		}
		if r := mtprotoRun("iptables", "-D", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT"); !r.ok() {
			s.log.Warn("iptables -D failed", "port", port, "stderr", r.Stderr)
		}
	}
}

// mtprotoFirewallCheck — что за фаервол и открыт ли порт. Правила не меняет: статус
// должен показывать реальное состояние, а не последний результат наших вызовов.
func (s *Server) mtprotoFirewallCheck(port int) MTProtoFirewall {
	fw := MTProtoFirewall{Backend: mtprotoFirewallBackend()}
	if port <= 0 {
		return fw
	}
	switch fw.Backend {
	case "ufw":
		r := mtprotoRun("ufw", "status")
		if !r.ok() {
			fw.Error = "ufw status: " + mtprotoCmdErr(r)
			return fw
		}
		rule, bare := fmt.Sprintf("%d/tcp", port), strconv.Itoa(port)
		for _, line := range strings.Split(r.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && (f[0] == rule || f[0] == bare) && f[1] == "ALLOW" {
				fw.PortOpen = true
				break
			}
		}
	case "firewalld":
		r := mtprotoRun("firewall-cmd", "--query-port="+fmt.Sprintf("%d/tcp", port))
		if r.ok() {
			fw.PortOpen = true
		} else if r.Code != 1 {
			fw.Error = "firewall-cmd --query-port: " + mtprotoCmdErr(r)
		}
	case "iptables":
		r := mtprotoRun("iptables", "-C", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT")
		if r.ok() {
			fw.PortOpen = true
		} else if r.Code != 1 {
			fw.Error = "iptables -C: " + mtprotoCmdErr(r)
		}
	case "none":
		fw.PortOpen = true // фаервола нет — входящие не закрыты
	}
	return fw
}

// ───────── состояние и управление ─────────

// mtprotoActive — юнит сейчас работает?
func mtprotoActive() bool {
	return strings.TrimSpace(mtprotoRun("systemctl", "is-active", mtprotoService).Stdout) == "active"
}

// mtprotoJournalTail — последние строки журнала юнита одной строкой (до ~500 символов):
// по ним видно, почему mtg умер — чаще всего отвергнутый конфиг/секрет.
func (s *Server) mtprotoJournalTail() string {
	r := mtprotoRun("journalctl", "-u", mtprotoService, "--no-pager", "-n", "20")
	out := r.Stdout
	if strings.TrimSpace(out) == "" {
		out = r.Stderr
	}
	out = strings.Join(strings.Fields(out), " ")
	if out == "" {
		return "служба не запустилась, журнал пуст"
	}
	if rs := []rune(out); len(rs) > 500 {
		out = string(rs[:500]) + "…"
	}
	return out
}

func (s *Server) mtprotoStatus() MTProtoStatus {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	secret := s.getCfg("mtprotoSecret", "")
	host := s.getCfg("mtprotoHost", mtprotoDefaultHost)
	bin := mtprotoFindBinary()
	st := MTProtoStatus{Port: port, Secret: secret, Host: host, Binary: bin,
		Managed: bin == mtgInstallPath, Running: mtprotoActive()}
	// Ссылка (и QR) только у работающего прокси: у остановленного она мертва, и панель
	// показывает причину вместо неё.
	if st.Running {
		st.Link = mtprotoLink(s.cfg().ServerIP, port, secret)
	} else if bin == "" {
		st.LinkReason = "нужен бинарник mtg"
	} else if port > 0 && secret != "" {
		st.LinkReason = "прокси остановлен"
	}
	if bin == "" && port > 0 {
		st.Error = "компонент прокси не установлен — установите его и повторите"
	} else if port > 0 && !st.Running {
		// Юнит настроен, но не работает: enable --now уже не считается доказательством,
		// а Type=simple может умереть сразу после exec — показываем хвост журнала.
		st.Error = s.mtprotoJournalTail()
	}
	st.Firewall = s.mtprotoFirewallCheck(port)
	return st
}

// mtprotoUp — поднять/обновить прокси на порту. Идемпотентно: повтор с тем же портом
// переписывает конфиг и перезапускает сервис; секрет не меняется (смена — mtprotoRotate).
// host — необязательный FakeTLS-домен из запроса; "" — сохранённый/по умолчанию.
func (s *Server) mtprotoUp(port int, host string) (MTProtoStatus, error) {
	bin := mtprotoFindBinary()
	if bin == "" {
		return MTProtoStatus{}, httpErr(503, "Компонент прокси не установлен. Установите его в панели и повторите.")
	}
	if port <= 0 || port > 65535 {
		return MTProtoStatus{}, httpErr(400, "Порт: целое число от 1 до 65535")
	}

	secret := s.getCfg("mtprotoSecret", "")
	host, err := s.mtprotoHostChoice(host, secret)
	if err != nil {
		return MTProtoStatus{}, httpErr(400, err.Error())
	}
	// Секрет старого формата (без FakeTLS-домена) или с другим доменом перегенерируем.
	// Старые ссылки уже мертвы (mtg их не принимает), так что это ремонт, а не поломка;
	// пишем в лог, чтобы это не выглядело случайной сменой секрета.
	var regenerated bool
	if secret, regenerated, err = mtprotoRepairSecret(secret, host); err != nil {
		return MTProtoStatus{}, err
	}
	if regenerated {
		s.log.Info("mtproto: секрет перегенерирован под FakeTLS", "host", host)
	}

	conf := mtprotoConfPath(s)
	if err := os.WriteFile(conf, []byte(mtprotoConf(secret, port)), 0o600); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не записан: %w", err)
	}
	if err := os.WriteFile(mtprotoServicePath, []byte(mtprotoUnit(bin, conf)), 0o644); err != nil {
		return MTProtoStatus{}, fmt.Errorf("systemd-юнит не записан: %w", err)
	}
	if r := mtprotoRun("systemctl", "daemon-reload"); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl daemon-reload: %s", r.Stderr)
	}

	// enable --now активный юнит не трогает — смену порта он бы просто проигнорировал.
	// Поэтому свой юнит останавливаем сами (это же освобождает порт: иначе повторный POST
	// с тем же портом упрётся в portTaken, который увидит наш же mtg) и проверяем порт
	// только после остановки. 409 остаётся для чужих процессов на порту.
	enabled := strings.TrimSpace(mtprotoRun("systemctl", "is-enabled", mtprotoService).Stdout) == "enabled"
	if mtprotoActive() {
		if r := mtprotoRun("systemctl", "stop", mtprotoService); !r.ok() {
			return MTProtoStatus{}, fmt.Errorf("systemctl stop %s: %s", mtprotoService, r.Stderr)
		}
	}
	if s.portTaken(port) {
		return MTProtoStatus{}, httpErr(409, fmt.Sprintf("Порт %d уже занят другой программой", port))
	}
	fw := s.mtprotoFirewallOpen(port)
	if fw.Error != "" {
		s.log.Warn("mtproto: порт не открыт в фаерволе", "backend", fw.Backend, "error", fw.Error)
	}
	if r := mtprotoRun("systemctl", "enable", mtprotoService); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl enable %s: %s", mtprotoService, r.Stderr)
	}
	verb := "restart"
	if !enabled {
		verb = "start" // новый юнит: restart тоже поднял бы, но start честнее
	}
	if r := mtprotoRun("systemctl", verb, mtprotoService); !r.ok() {
		return MTProtoStatus{}, httpErr(502, "Прокси не запустился: "+s.mtprotoJournalTail())
	}
	time.Sleep(mtprotoSettleDelay)
	if !mtprotoActive() {
		// Type=simple: systemd рапортует успех сразу после exec, а mtg может умереть
		// следующим же тиком. Единственное доказательство — is-active после паузы.
		return MTProtoStatus{}, httpErr(502, "Прокси не запустился: "+s.mtprotoJournalTail())
	}

	// Порт, секрет и домен сохраняем только после того, как сервис реально поднялся:
	// иначе панель показала бы рабочую ссылку на мёртвый прокси.
	s.setCfg("mtprotoPort", strconv.Itoa(port))
	s.setCfg("mtprotoSecret", secret)
	s.setCfg("mtprotoHost", host)
	s.log.Info("mtproto proxy up", "port", port, "host", host)
	st := s.mtprotoStatus()
	st.Firewall = fw // отчёт об открытии порта важнее повторного опроса
	return st, nil
}

// mtprotoRotate — новый секрет (ссылка утекла): конфиг перезаписывается, сервис
// перезапускается. Адрес/порт прежние, домен — сохранённый или из запроса.
func (s *Server) mtprotoRotate(host string) (MTProtoStatus, error) {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	if port <= 0 {
		return MTProtoStatus{}, httpErr(409, "Прокси не поднят — менять секрет нечего")
	}
	old := s.getCfg("mtprotoSecret", "")
	host, err := s.mtprotoHostChoice(host, old)
	if err != nil {
		return MTProtoStatus{}, httpErr(400, err.Error())
	}
	if _, ok := mtprotoSecretHost(old); !ok && old != "" {
		s.log.Info("mtproto: секрет перегенерирован под FakeTLS", "host", host)
	}
	secret, err := mtprotoNewSecret(host)
	if err != nil {
		return MTProtoStatus{}, err
	}
	if err := os.WriteFile(mtprotoConfPath(s), []byte(mtprotoConf(secret, port)), 0o600); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не записан: %w", err)
	}
	if r := mtprotoRun("systemctl", "restart", mtprotoService); !r.ok() {
		return MTProtoStatus{}, httpErr(502, "Прокси не запустился: "+s.mtprotoJournalTail())
	}
	time.Sleep(mtprotoSettleDelay)
	if !mtprotoActive() {
		return MTProtoStatus{}, httpErr(502, "Прокси не запустился: "+s.mtprotoJournalTail())
	}
	s.setCfg("mtprotoSecret", secret)
	s.setCfg("mtprotoHost", host)
	s.log.Info("mtproto secret rotated", "port", port, "host", host)
	return s.mtprotoStatus(), nil
}

// mtprotoStop — остановить и снять с автозапуска. Секрет и порт остаются в config:
// подъём заново вернёт ту же ссылку.
func (s *Server) mtprotoStop() (MTProtoStatus, error) {
	// Любой отказ systemctl (включая «юнита нет») — уже остановлено: честное состояние
	// покажет status по is-active.
	if r := mtprotoRun("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	return s.mtprotoStatus(), nil
}

// mtprotoRemoveService — полностью убрать прокси: остановить сервис, удалить юнит и
// конфиг, очистить порт, секрет и домен, закрыть порт в фаерволе. Сам mtg остаётся на
// месте — его удаляет отдельная кнопка, чтобы можно было переустановить прокси, не
// скачивая бинарник заново.
func (s *Server) mtprotoRemoveService() (MTProtoStatus, error) {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	if r := mtprotoRun("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	if err := os.Remove(mtprotoServicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("systemd-юнит не удалён: %w", err)
	}
	if r := mtprotoRun("systemctl", "daemon-reload"); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl daemon-reload: %s", r.Stderr)
	}
	mtprotoRun("systemctl", "reset-failed", mtprotoService)
	if err := os.Remove(mtprotoConfPath(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не удалён: %w", err)
	}
	if _, err := s.db.Exec("DELETE FROM config WHERE key IN ('mtprotoPort','mtprotoSecret','mtprotoHost')"); err != nil {
		return MTProtoStatus{}, fmt.Errorf("настройки прокси не удалены: %w", err)
	}
	s.mtprotoFirewallClose(port)
	s.log.Info("mtproto proxy removed")
	return s.mtprotoStatus(), nil
}

// mtprotoMtgURL — архив релиза mtg для архитектуры. Набор архитектур тот же, что
// у install.sh: amd64 и arm64; на прочих кнопка установки честно откажет.
func mtprotoMtgURL(goarch string) (string, error) {
	switch goarch {
	case "amd64", "arm64":
		return fmt.Sprintf("%s/v%s/mtg-%s-linux-%s.tar.gz", mtgBaseURL, mtgVersion, mtgVersion, goarch), nil
	default:
		return "", fmt.Errorf("архитектура %s не поддерживается mtg", goarch)
	}
}

// extractMtg — достать бинарник mtg из tar.gz. В релизном архиве один каталог
// mtg-<версия>-linux-<арх>/ с файлом mtg; имя сверяем по basename, чтобы не зависеть
// от версии в пути.
func extractMtg(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("архив mtg не распакован: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("в архиве mtg нет бинарника mtg")
		}
		if err != nil {
			return nil, fmt.Errorf("архив mtg повреждён: %w", err)
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "mtg" {
			continue
		}
		if h.Size <= 0 || h.Size > mtgMaxDownload {
			return nil, fmt.Errorf("неожиданный размер mtg: %d байт", h.Size)
		}
		b := make([]byte, int(h.Size))
		if _, err := io.ReadFull(tr, b); err != nil {
			return nil, fmt.Errorf("бинарник mtg прочитан не полностью: %w", err)
		}
		return b, nil
	}
}

// mtprotoInstallBinary — скачать и поставить mtg в /usr/local/bin. Панель делает то же,
// что install.sh (MTG=1): фиксированная версия, архив с GitHub, проверка запуска.
func (s *Server) mtprotoInstallBinary() (MTProtoStatus, error) {
	if p := mtprotoFindBinary(); p != "" {
		return s.mtprotoStatus(), httpErr(409, "Компонент прокси уже установлен: "+p)
	}
	url, err := mtprotoMtgURL(runtime.GOARCH)
	if err != nil {
		return MTProtoStatus{}, httpErr(400, err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(mtgInstallPath), 0o755); err != nil {
		return MTProtoStatus{}, fmt.Errorf("каталог для mtg не создан: %w", err)
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return MTProtoStatus{}, httpErr(502, "mtg не скачался: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MTProtoStatus{}, httpErr(502, fmt.Sprintf("mtg не скачался: GitHub ответил %s", resp.Status))
	}
	b, err := extractMtg(io.LimitReader(resp.Body, mtgMaxDownload+1))
	if err != nil {
		return MTProtoStatus{}, httpErr(502, err.Error())
	}

	tmp, err := os.CreateTemp(filepath.Dir(mtgInstallPath), ".mtg-*")
	if err != nil {
		return MTProtoStatus{}, fmt.Errorf("временный файл mtg не создан: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return MTProtoStatus{}, fmt.Errorf("mtg не записан: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return MTProtoStatus{}, fmt.Errorf("права mtg не выставлены: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtg не записан: %w", err)
	}

	// Проверяем до переименования: битый или чужой бинарник не должен заменить рабочий.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmpName, "-v").CombinedOutput()
	if err != nil {
		return MTProtoStatus{}, httpErr(502, fmt.Sprintf("mtg не запускается: %v (%s)", err, strings.TrimSpace(string(out))))
	}
	if !strings.Contains(string(out), mtgVersion) {
		return MTProtoStatus{}, httpErr(502, fmt.Sprintf("mtg версии %s не запускается: %s", mtgVersion, strings.TrimSpace(string(out))))
	}
	if err := os.Rename(tmpName, mtgInstallPath); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtg не установлен: %w", err)
	}
	s.log.Info("mtg installed", "version", mtgVersion, "path", mtgInstallPath)
	return s.mtprotoStatus(), nil
}

// mtprotoRemoveBinary — убрать mtg, поставленный панелью. Чужой бинарник
// (/usr/bin/mtg из пакета, ручная установка рядом с awg-ctrl) не трогаем.
func (s *Server) mtprotoRemoveBinary() (MTProtoStatus, error) {
	bin := mtprotoFindBinary()
	if bin == "" {
		return s.mtprotoStatus(), httpErr(404, "Компонент прокси не установлен")
	}
	if bin != mtgInstallPath {
		return s.mtprotoStatus(), httpErr(409, "Компонент прокси установлен не панелью ("+bin+") — удалите его вручную")
	}
	if r := mtprotoRun("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	if err := os.Remove(bin); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("mtg не удалён: %w", err)
	}
	s.log.Info("mtg removed", "path", bin)
	return s.mtprotoStatus(), nil
}
