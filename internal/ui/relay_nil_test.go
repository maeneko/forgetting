package ui

import "testing"

// refreshHealth при старте панели (Nodes.Start, LOCAL_NODE=on — дефолт) зовёт n.row(0), а у
// локального сервера строки в nodes нет по устройству реестра. До фикса isRelay() на
// nil-записи паниковала — awg-ui падал на каждом запуске с этой конфигурацией (репорт:
// segfault addr=0x98 на 0.5.4, откат на 0.5.3 помогал). nil-запись — не релей: refreshHealth
// идёт дальше, как в 0.5.3.
func TestRefreshHealthNilRow(t *testing.T) {
	if (*nodeRow)(nil).isRelay() {
		t.Fatal("nil-запись не должна считаться релеем")
	}
	p := newMovePanel(t, "on", "")
	// 0 — локальный сервер (строки нет), 42 — id, которого в реестре нет вовсе.
	for _, id := range []int64{0, 42} {
		p.a.nodes.refreshHealth(id) // не должно паниковать
	}
	if got := p.a.nodes.health[0]; got != nil {
		t.Fatalf("health без ответа awg-ctrl должен быть вычищен, получил %+v", got)
	}
	// Релей из реестра по-прежнему опознаётся, и health для него не обновляется.
	if _, err := p.a.db.Exec("INSERT INTO nodes (id, name, role, created_at) VALUES (7, 'Relay', 'relay', 1)"); err != nil {
		t.Fatal(err)
	}
	if row := p.a.nodes.row(7); !row.isRelay() {
		t.Fatal("релей не опознан")
	}
	p.a.nodes.refreshHealth(7)
}
