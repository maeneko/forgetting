package ctrl

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Установка и удаление встроенного мониторинга (Prometheus) по кнопке в панели. Действие
// одно из двух и без параметров: awg-ctrl запускает ту же команду, что оператор набрал бы
// руками (`awg-ctrl monitor install|remove`, root), так что код один. Панель сама пакетов
// не ставит и не удаляет — она только просит об этом здесь.

type monitorJob struct {
	mu     sync.Mutex
	active bool
	action string
	exit   *int
}

var reANSI = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func (s *Server) logTail() string {
	b, err := os.ReadFile(s.cliLog)
	if err != nil {
		return ""
	}
	t := string(b)
	if len(t) > 4000 { // последние 4000 символов (в JS — UTF-16 единиц; для лога разницы нет)
		t = t[len(t)-4000:]
	}
	return strings.ReplaceAll(reANSI.ReplaceAllString(t, ""), "\r", "\n")
}

// startMonitorJob запускает CLI в фоне; возвращает 409, если уже идёт.
func (s *Server) startMonitorJob(action string) error {
	j := &s.monitor
	j.mu.Lock()
	if j.active {
		j.mu.Unlock()
		return httpErr(409, "Операция уже идёт")
	}
	if _, err := os.Stat(s.cliPath); err != nil {
		j.mu.Unlock()
		return httpErr(501, "CLI не найден ("+s.cliPath+"): выполните на сервере awg-ctrl monitor "+action)
	}
	os.MkdirAll(filepath.Dir(s.cliLog), 0o755)
	f, err := os.OpenFile(s.cliLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		j.mu.Unlock()
		return err
	}
	cmd := exec.Command(s.cliPath, "monitor", action)
	cmd.Stdout, cmd.Stderr = f, f
	j.active, j.action, j.exit = true, action, nil
	if err := cmd.Start(); err != nil {
		s.log.Error("monitor: не запустился", "action", action, "error", err.Error())
		code := 127
		j.active, j.exit = false, &code
		f.Close()
		j.mu.Unlock()
		return nil
	}
	j.mu.Unlock()
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		f.Close()
		s.log.Info("monitor: завершён", "action", action, "code", code)
		j.mu.Lock()
		j.active, j.exit = false, &code
		j.mu.Unlock()
	}()
	return nil
}

type jobView struct {
	Action  *string `json:"action"`
	Running bool    `json:"running"`
	Exit    *int    `json:"exit"`
	Log     string  `json:"log"`
}

func (s *Server) monitorView() jobView {
	j := &s.monitor
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{Running: j.active, Exit: j.exit}
	if j.action != "" {
		a := j.action
		v.Action = &a
		v.Log = s.logTail()
	}
	return v
}
