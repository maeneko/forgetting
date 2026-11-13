package ctrl

// Telegram MTProto-прокси на ноде: awg-ctrl находит бинарник mtg (9seconds/mtg v2),
// генерирует секрет, пишет TOML-конфиг и systemd-юнит awg-mtproto.service, открывает
// порт в ufw (если он есть) и запускает сервис. Ссылку t.me/proxy?… собирает из
// публичного адреса сервера (serverIp из config): её показывает панель, дальше админ
// раздаёт её пользователям сам.
//
// Состояние — в таблице config users.db (mtprotoPort, mtprotoSecret, mtprotoSecretNew),
// как у шейпинга: переживает перезапуски, секрет не теряется. Сам mtg наружу ходит на
// Telegram напрямую; vpn-интерфейс и его правила он не трогает.
//
// Секрет — plain ee-серия (ee + 16 случайных байт в hex): без FakeTLS-домена. Бинарник
// ищется рядом с awg-ctrl, в /usr/local/bin, /usr/bin и PATH; нет его — оператору
// говорят поставить (install.sh качает его опционально).

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const mtprotoService = "awg-mtproto"

type MTProtoStatus struct {
	Port    int    `json:"port"`           // настроенный порт; 0 — прокси не поднимали
	Secret  string `json:"secret"`         // «ee…»
	Running bool   `json:"running"`        // systemd: active
	Link    string `json:"link,omitempty"` // https://t.me/proxy?… — собран, когда есть порт+секрет+адрес
	Binary  string `json:"binary"`         // найденный mtg; "" — не найден
	Error   string `json:"error,omitempty"`
}

func mtprotoLink(ip string, port int, secret string) string {
	if ip == "" || port <= 0 || secret == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/proxy?server=%s&port=%d&secret=%s", ip, port, secret)
}

func mtprotoNewSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("нет энтропии для секрета: %w", err)
	}
	return "ee" + hex.EncodeToString(b), nil
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
	return portTakenFrom(runCmd("ss", "-tln").Stdout, port)
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

func (s *Server) mtprotoStatus() MTProtoStatus {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	secret := s.getCfg("mtprotoSecret", "")
	bin := mtprotoFindBinary()
	st := MTProtoStatus{Port: port, Secret: secret, Binary: bin}
	if port > 0 && secret != "" {
		st.Link = mtprotoLink(s.cfg().ServerIP, port, secret)
	}
	if bin == "" && port > 0 {
		st.Error = "бинарник mtg не найден — установите его и повторите"
	}
	if r := runCmd("systemctl", "is-active", mtprotoService); strings.TrimSpace(r.Stdout) == "active" {
		st.Running = true
	}
	return st
}

// mtprotoUp — поднять/обновить прокси на порту. Идемпотентно: повтор с тем же портом
// переписывает конфиг и перезапускает сервис; секрет не меняется (смена — mtprotoRotate).
func (s *Server) mtprotoUp(port int) (MTProtoStatus, error) {
	bin := mtprotoFindBinary()
	if bin == "" {
		return MTProtoStatus{}, httpErr(503, "Бинарник mtg не найден. Установите его на сервер и повторите.")
	}
	if port <= 0 || port > 65535 {
		return MTProtoStatus{}, httpErr(400, "Порт: целое число от 1 до 65535")
	}
	if s.portTaken(port) {
		return MTProtoStatus{}, httpErr(409, fmt.Sprintf("Порт %d уже занят другой программой", port))
	}
	secret := s.getCfg("mtprotoSecret", "")
	if secret == "" {
		var err error
		if secret, err = mtprotoNewSecret(); err != nil {
			return MTProtoStatus{}, err
		}
	}

	conf := fmt.Sprintf("secret = %q\nbind-to = %q\n", secret, fmt.Sprintf("0.0.0.0:%d", port))
	if err := os.WriteFile(mtprotoConfPath(s), []byte(conf), 0o600); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не записан: %w", err)
	}
	unit := fmt.Sprintf(`[Unit]
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
`, bin, mtprotoConfPath(s))
	if err := os.WriteFile("/etc/systemd/system/"+mtprotoService+".service", []byte(unit), 0o644); err != nil {
		return MTProtoStatus{}, fmt.Errorf("systemd-юнит не записан: %w", err)
	}
	if r := runCmd("ufw", "allow", fmt.Sprintf("%d/tcp", port)); r.Code != -1 && !r.ok() {
		// ufw нет — не беда (порт и так открыт); есть, но отказал — только в лог
		s.log.Warn("ufw allow failed", "stderr", r.Stderr)
	}
	if r := runCmd("systemctl", "daemon-reload"); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl daemon-reload: %s", r.Stderr)
	}
	if r := runCmd("systemctl", "enable", "--now", mtprotoService); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl enable --now %s: %s", mtprotoService, r.Stderr)
	}
	s.setCfg("mtprotoPort", strconv.Itoa(port))
	s.setCfg("mtprotoSecret", secret)
	s.log.Info("mtproto proxy up", "port", port)
	return s.mtprotoStatus(), nil
}

// mtprotoRotate — новый секрет (ссылка утекла): конфиг перезаписывается, сервис
// перезапускается. Адрес/порт прежние.
func (s *Server) mtprotoRotate() (MTProtoStatus, error) {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	if port <= 0 {
		return MTProtoStatus{}, httpErr(409, "Прокси не поднят — менять секрет нечего")
	}
	secret, err := mtprotoNewSecret()
	if err != nil {
		return MTProtoStatus{}, err
	}
	if err := os.WriteFile(mtprotoConfPath(s), []byte(fmt.Sprintf("secret = %q\nbind-to = %q\n",
		secret, fmt.Sprintf("0.0.0.0:%d", port))), 0o600); err != nil {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не записан: %w", err)
	}
	if r := runCmd("systemctl", "restart", mtprotoService); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl restart %s: %s", mtprotoService, r.Stderr)
	}
	s.setCfg("mtprotoSecret", secret)
	s.log.Info("mtproto secret rotated", "port", port)
	return s.mtprotoStatus(), nil
}

// mtprotoStop — остановить и снять с автозапуска. Секрет и порт остаются в config:
// подъём заново вернёт ту же ссылку.
func (s *Server) mtprotoStop() (MTProtoStatus, error) {
	// Любой отказ systemctl (включая «юнита нет») — уже остановлено: честное состояние
	// покажет status по is-active.
	if r := runCmd("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	return s.mtprotoStatus(), nil
}
