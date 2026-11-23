package cli

import (
	"fmt"
	"os"
)

// Юнит агента ставит install.sh при роли «нода»/«релей». `join` превращает уже установленный
// standalone в ноду без повторного запуска установщика, поэтому юнит нужен и здесь.
// 🛑 Текст должен совпадать с install.sh (setup_service).
const (
	agentUnitFile   = "/etc/systemd/system/awg-agent.service"
	controlUnitFile = "/etc/systemd/system/awg-control.service"
)

func agentUnitText() string {
	return fmt.Sprintf(`[Unit]
Description=%s node agent
After=network-online.target awg-control.service
Wants=network-online.target

[Service]
Type=forking
PIDFile=/tmp/awg-agent.pid
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# PID-файл от прошлой загрузки: живой оставляем (агента мог поднять start all — юнит его
# усыновит по PIDFile), мёртвый убираем, чтобы systemd не взял чужой PID.
ExecStartPre=-/bin/sh -c 'p=$(cat /tmp/awg-agent.pid 2>/dev/null); [ -n "$p" ] && kill -0 "$p" 2>/dev/null || rm -f /tmp/awg-agent.pid'
ExecStart=/usr/local/bin/awg-ctrl start agent
ExecStop=/usr/local/bin/awg-ctrl stop agent
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, env("BRAND", "Forgetting"))
}

// ensureAgentUnit — завести и включить отдельный юнит агента (идемпотентно). Только под
// systemd и только если агрегатный юнит уже стоит: без него это не systemd-установка.
func (c *CLI) ensureAgentUnit() {
	if !exists("/run/systemd/system") || !exists(controlUnitFile) {
		return
	}
	if err := os.WriteFile(agentUnitFile, []byte(agentUnitText()), 0o644); err != nil {
		errorf("  awg-agent.service: %v", err)
		return
	}
	c.systemctl("daemon-reload")
	if !c.systemctl("enable", "awg-agent.service") {
		errorf("  автозапуск awg-agent не настроен")
	}
}

// removeAgentUnit — убрать юнит, когда сервер перестал быть нодой/релеем: иначе после
// перезагрузки агент без CORE_HOST уйдёт в цикл перезапусков.
func (c *CLI) removeAgentUnit() {
	if !exists(agentUnitFile) {
		return
	}
	c.systemctl("disable", "--now", "awg-agent.service")
	os.Remove(agentUnitFile)
	c.systemctl("daemon-reload")
}
