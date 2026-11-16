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
)

type MTProtoStatus struct {
	Port    int    `json:"port"`           // настроенный порт; 0 — прокси не поднимали
	Secret  string `json:"secret"`         // «ee…»
	Running bool   `json:"running"`        // systemd: active
	Link    string `json:"link,omitempty"` // https://t.me/proxy?… — собран, когда есть порт+секрет+адрес
	Binary  string `json:"binary"`         // найденный mtg; "" — не найден
	Managed bool   `json:"managed"`        // бинарник стоит там, куда его кладёт панель; только его можно удалить
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
	st := MTProtoStatus{Port: port, Secret: secret, Binary: bin, Managed: bin == mtgInstallPath}
	if port > 0 && secret != "" {
		st.Link = mtprotoLink(s.cfg().ServerIP, port, secret)
	}
	if bin == "" && port > 0 {
		st.Error = "компонент прокси не установлен — установите его и повторите"
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
		return MTProtoStatus{}, httpErr(503, "Компонент прокси не установлен. Установите его в панели и повторите.")
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
	if err := os.WriteFile(mtprotoServicePath, []byte(unit), 0o644); err != nil {
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

// mtprotoRemoveService — полностью убрать прокси: остановить сервис, удалить юнит и
// конфиг, очистить порт и секрет. Сам mtg остаётся на месте — его удаляет отдельная
// кнопка, чтобы можно было переустановить прокси, не скачивая бинарник заново.
func (s *Server) mtprotoRemoveService() (MTProtoStatus, error) {
	port, _ := strconv.Atoi(s.getCfg("mtprotoPort", "0"))
	if r := runCmd("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	if err := os.Remove(mtprotoServicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("systemd-юнит не удалён: %w", err)
	}
	if r := runCmd("systemctl", "daemon-reload"); !r.ok() {
		return MTProtoStatus{}, fmt.Errorf("systemctl daemon-reload: %s", r.Stderr)
	}
	runCmd("systemctl", "reset-failed", mtprotoService)
	if err := os.Remove(mtprotoConfPath(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("mtproto.toml не удалён: %w", err)
	}
	if _, err := s.db.Exec("DELETE FROM config WHERE key IN ('mtprotoPort','mtprotoSecret','mtprotoSecretNew')"); err != nil {
		return MTProtoStatus{}, fmt.Errorf("настройки прокси не удалены: %w", err)
	}
	if port > 0 {
		if r := runCmd("ufw", "delete", "allow", fmt.Sprintf("%d/tcp", port)); r.Code != -1 && !r.ok() {
			s.log.Warn("ufw delete failed", "port", port, "stderr", r.Stderr)
		}
	}
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
	if r := runCmd("systemctl", "disable", "--now", mtprotoService); !r.ok() && r.Code != -1 {
		s.log.Warn("systemctl disable --now failed (юнита нет — уже остановлен?)", "stderr", r.Stderr)
	}
	if err := os.Remove(bin); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MTProtoStatus{}, fmt.Errorf("mtg не удалён: %w", err)
	}
	s.log.Info("mtg removed", "path", bin)
	return s.mtprotoStatus(), nil
}
