package ui

import (
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// Самозалечивание сирот: ссылки на серверы, которых нет в реестре, чистятся; ключ,
// оставшийся без единого сервера, получает сервер по умолчанию и заявки пиров устройств
// (сведёт reconcile при выходе ноды в онлайн); живые серверы и отозванные ключи не трогаются.
func TestHealDeadServers(t *testing.T) {
	p := newMovePanel(t, "off", "40500")
	s, db := p.s, p.a.db
	for _, q := range []string{
		// нода 1 «удалена из реестра», нода 2 живая (DefaultID при off = первая не-релей = 2)
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (2, 'B', 'x', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'k1', x'01', 1), (2, 'u2', 'k2', x'02', 1), (3, 'u3', 'k3', x'03', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at, revoked_at) VALUES (4, 'u4', 'k4', x'04', 1, 1)",
		// ключ 1: живой сервер 2 + мёртвый 1; ключ 2: только мёртвый; ключ 4 — отозванный, только мёртвый
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1), (1, 2), (2, 1), (4, 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 2, 'd', 'W', 'A', 1)",
		"INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (5, 1, 'P1', '10.0.0.2', 'K')",
		"INSERT INTO peer_tombstones (server_id, pub_key) VALUES (1, 'dead')",
		"INSERT INTO server_profiles (server_id, profile, updated_at) VALUES (1, '{}', 1)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	s.healDeadServers()

	servers := func(id int) string {
		return string(jsjson.MustMarshal(intsJSON(s.serverIDsOf(db, int64(id)))))
	}
	// ключ 1: мёртвый сервер снят, живой остался
	if got := servers(1); got != "[2]" {
		t.Fatalf("ключ 1: серверы %s, ждём [2]", got)
	}
	// ключ 2 остался без серверов → получил дефолт (нода 2) + заявка пира устройству
	if got := servers(2); got != "[2]" {
		t.Fatalf("ключ 2: серверы %s, ждём [2]", got)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM device_peers WHERE device_id = 5 AND server_id = 2 AND peer_pub IS NULL").Scan(&n); err != nil || n != 1 {
		t.Fatalf("заявок пиров ключа 2 на ноде 2: %d (%v), ждём 1", n, err)
	}
	// отозванный ключ не получал ничего
	if got := servers(4); got != "[]" {
		t.Fatalf("ключ 4: серверы %s, ждём []", got)
	}
	// мусор мёртвого сервера вычищен
	for _, q := range []string{
		"SELECT COUNT(*) FROM device_peers WHERE server_id = 1",
		"SELECT COUNT(*) FROM peer_tombstones WHERE server_id = 1",
		"SELECT COUNT(*) FROM server_profiles WHERE server_id = 1",
	} {
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d (%v), ждём 0", q, n, err)
		}
	}
	// повторный проход — ничего не меняет и не падает
	s.healDeadServers()
	if got := servers(1) + " " + servers(2); got != "[2] [2]" {
		t.Fatalf("после второго прохода: %s", got)
	}
}
