package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// «Серверы для всех»: add — у каждого живого ключа, remove — ни у кого, остальное не трогается;
// пиры снятых серверов — в надгробия, новые ключи получают набор по умолчанию.
func TestBulkServers(t *testing.T) {
	p := newMovePanel(t, "off", "40500")
	s, db := p.s, p.a.db
	if err := p.a.initAPIKeys(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO nodes (id, name, pub, created_at) VALUES (1, 'A', 'x', 1), (2, 'B', 'x', 1), (3, 'C', 'x', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'k1', x'01', 1), (2, 'u2', 'k2', x'02', 1), (3, 'u3', 'k3', x'03', 1)",
		"INSERT INTO master_keys (id, uuid, label, secret, created_at, revoked_at) VALUES (4, 'u4', 'k4', x'04', 1, 1)",
		"INSERT INTO master_servers (master_id, server_id) VALUES (1, 1), (2, 1), (2, 2), (3, 2), (4, 1)",
		"INSERT INTO devices (id, master_id, device_id, pub_key, auth_pub, created_at) VALUES (5, 2, 'd', 'W', 'A', 1)",
		"INSERT INTO device_peers (device_id, server_id, peer_pub, ip, psk) VALUES (5, 1, 'P1', '10.0.0.2', 'K'), (5, 2, 'P2', '10.0.1.2', 'K')",
		"INSERT INTO api_keys (id, label, key_hash, prefix, created_at) VALUES (1, 'Бот', 'h1', 'p1', 1), (2, 'Всё', 'h2', 'p2', 1)",
		"INSERT INTO api_key_servers (key_id, server_id) VALUES (1, 1), (1, 2), (2, 1), (2, 2), (2, 3)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	servers := func() string {
		var out []string
		for _, id := range []int64{1, 2, 3, 4} {
			out = append(out, string(jsjson.MustMarshal(intsJSON(s.serverIDsOf(db, id)))))
		}
		return strings.Join(out, " ")
	}
	body := func(kv ...any) jsjson.Object {
		o := jsjson.Object{}
		for i := 0; i < len(kv); i += 2 {
			o = append(o, jsjson.KV{Key: kv[i].(string), Val: kv[i+1]})
		}
		return o
	}
	f := func(ids ...float64) []any {
		out := []any{}
		for _, x := range ids {
			out = append(out, x)
		}
		return out
	}
	status := func(err error) int {
		var ae *apiError
		if errors.As(err, &ae) {
			return ae.status
		}
		return 0
	}

	out, err := s.bulkServers(body("add", f(3), "remove", f(2), "dry_run", true))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(jsjson.MustMarshal(out)); got != `{"changed":3,"total":3,"hidden":[{"id":1,"label":"Бот","keys":3}]}` {
		t.Errorf("предпросмотр: %s", got)
	}
	if got := servers(); got != "[1] [1,2] [2] [1]" {
		t.Errorf("предпросмотр изменил ключи: %s", got)
	}
	if _, err := s.bulkServers(body("remove", f(1, 2))); status(err) != 409 {
		t.Errorf("ключи без серверов: %v", err)
	}
	if _, err := s.bulkServers(body("add", f(2), "remove", f(2))); status(err) != 400 {
		t.Errorf("и у всех, и ни у кого: %v", err)
	}
	if _, err := s.bulkServers(body("add", f(9))); status(err) != 400 {
		t.Errorf("несуществующий сервер: %v", err)
	}

	if _, err := s.bulkServers(body("add", f(3), "remove", f(2), "defaults", f(3))); err != nil {
		t.Fatal(err)
	}
	if got := servers(); got != "[1,3] [1,3] [3] [1]" {
		t.Errorf("после применения: %s (удаляемый ключ трогать нельзя)", got)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM peer_tombstones WHERE server_id = 2 AND pub_key = 'P2'").Scan(&n)
	if n != 1 {
		t.Error("пир снятого сервера не в надгробиях")
	}
	db.QueryRow("SELECT COUNT(*) FROM device_peers WHERE device_id = 5 AND server_id = 3").Scan(&n)
	if n != 1 {
		t.Error("устройство не получило заявку на новом сервере")
	}
	m, err := s.createMaster(body("label", "Новый"), nil)
	if err != nil || string(jsjson.MustMarshal(m.Get2("servers"))) != "[3]" {
		t.Errorf("новый ключ: %v %v", m, err)
	}
	if _, err := s.bulkServers(body("defaults", nil)); err != nil || s.defaultServers() != nil {
		t.Errorf("сброс набора по умолчанию: %v %v", s.defaultServers(), err)
	}
}
