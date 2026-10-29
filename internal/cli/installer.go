package cli

// Обновить, переустановить, удалить — из меню awg-ctrl и командами `awg-ctrl update|reinstall|
// uninstall`. Работу делает install.sh (ACTION=… — без его меню): CLI находит скрипт, кладёт во
// временный файл и передаёт ему управление (exec), чтобы диалог шёл в том же терминале.
//
// Скрипт — только из /tmp: install.sh перед заменой файлов убивает процессы, в командной строке
// которых есть путь установки (kill_related). Запущенный из /opt/awg-control/install.sh — или
// текстом через `bash -c` — он убил бы сам себя.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Откуда брать свежий установщик; INSTALLER_URL в cli.env — своё зеркало.
const defaultInstallerURL = "https://raw.githubusercontent.com/maeneko/forgetting/main/install.sh"

var installerActions = map[string]struct{ title, what string }{
	"update":    {"Обновить", "новая версия; настройки, пользователи и роль сохранятся"},
	"reinstall": {"Переустановить", "с нуля; данные можно будет сохранить"},
	"uninstall": {"Удалить", "полностью: сервисы, данные, правила фаервола; копию данных предложит сохранить"},
}

// fetchInstaller — текст install.sh: для обновления и переустановки — свежий из сети (иначе
// поставилась бы та же версия), для удаления — копия из установки (работает и без сети).
// Копия — запасной путь и для переустановки.
func (c *CLI) fetchInstaller(action string) ([]byte, string, error) {
	local := filepath.Join(c.root, "install.sh")
	readLocal := func() ([]byte, string, error) {
		b, err := os.ReadFile(local)
		return b, local, err
	}
	if action == "uninstall" {
		if b, src, err := readLocal(); err == nil {
			return b, src, nil
		}
	}
	url := env("INSTALLER_URL", defaultInstallerURL)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Get(url)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			if err == nil && strings.HasPrefix(string(b), "#!") {
				return b, url, nil
			}
		}
		err = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	if action == "reinstall" {
		if b, src, lerr := readLocal(); lerr == nil {
			fmt.Printf("  %s\n", dim("Установщик не скачался ("+err.Error()+") — берём копию из установки, версия та же"))
			return b, src, nil
		}
	}
	return nil, "", err
}

// runInstaller — подтверждение и exec install.sh с ACTION. Возвращается только при отказе или
// ошибке: после exec процесс CLI заменён установщиком.
func (c *CLI) runInstaller(action string, confirm bool) {
	a, ok := installerActions[action]
	if !ok {
		errorf("  Неизвестное действие: %s", action)
		return
	}
	if os.Geteuid() != 0 {
		errorf("  Нужен root: sudo awg-ctrl %s", action)
		return
	}
	if confirm && !c.confirmYes(fmt.Sprintf("  %s — %s. Продолжить?", a.title, a.what)) {
		fmt.Println("  Отменено")
		return
	}
	script, src, err := c.fetchInstaller(action)
	if err != nil {
		errorf("  Не удалось получить установщик: %v", err)
		errorf("  %s", dim("Запустите его вручную (README, «Установка») или задайте INSTALLER_URL в "+c.envFile))
		return
	}
	f, err := os.CreateTemp("", "awg-install-*.sh")
	if err != nil {
		errorf("  %v", err)
		return
	}
	f.Write(script)
	f.Close()
	fmt.Printf("  %s\n\n", dim("Установщик: "+src))
	bash := "/bin/bash"
	if p, err := exec.LookPath("bash"); err == nil {
		bash = p
	}
	// Исходное окружение, без cli.env: иначе UI_PORT, SERVER_NAME и прочие сохранённые значения
	// установщик принял бы за явно заданные параметры.
	environ := append(withoutKey(c.origEnv, "ACTION"), "ACTION="+action)
	if err := syscall.Exec(bash, []string{"bash", f.Name()}, environ); err != nil {
		errorf("  Не удалось запустить установщик: %v", err)
	}
}

func withoutKey(environ []string, key string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}
