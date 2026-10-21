package migrate

import (
	"os/exec"
	"strconv"
	"strings"
)

// ufw — вызов ufw (в тестах подменяется). Нет ufw — ошибка, и правила не трогаются.
var ufw = func(args ...string) ([]byte, error) {
	if _, err := exec.LookPath("ufw"); err != nil {
		return nil, err
	}
	return exec.Command("ufw", args...).CombinedOutput()
}

// OpenFirewall — открыть в UFW порты, на которые панель переезжает при приёме: подписка и хаб
// берут порты старого сервера, а install.sh открыл только те, что были при установке. Без этого
// нода #0 не дозвонится до хаба, а ретранслятор — до подписки. UFW не установлен или не активен —
// ничего не делаем (как install.sh). opened — что открыто, failed — что не удалось.
func OpenFirewall(ports ...int) (opened, failed []string) {
	out, err := ufw("status")
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "status: active") {
		return nil, nil
	}
	for _, p := range ports {
		if p <= 0 {
			continue
		}
		rule := strconv.Itoa(p) + "/tcp"
		if _, err := ufw("allow", rule); err != nil {
			failed = append(failed, rule)
		} else {
			opened = append(opened, rule)
		}
	}
	if len(opened) > 0 {
		ufw("reload")
	}
	return opened, failed
}
