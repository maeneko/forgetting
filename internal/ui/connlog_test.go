package ui

import "testing"

// Владелец пира sen:// в журнале → название мастер-ключа и устройства этой панели.
func TestConnLogOwnerNames(t *testing.T) {
	a := testApp(t, t.TempDir(), "on")
	s := &Sub{a: a}
	if err := s.initSchema(); err != nil {
		t.Fatal(err)
	}
	s.panelID = "abcd1234" // initSchema заводит свой

	a.db.Exec("INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (1, 'u1', 'Семья', x'01', 1)")
	a.db.Exec("INSERT INTO devices (id, master_id, device_id, device_name, pub_key, auth_pub, created_at) VALUES (2, 1, 'd', 'iPhone Маши', 'W', 'A', 1)")
	cache := map[string][3]any{}
	for owner, want := range map[string][3]any{
		"abcd1234/m1/d2": {"Семья", "iPhone Маши", false},
		"m1/d2":          {"Семья", "iPhone Маши", false}, // выдан до областей панели
		"abcd1234/m1/d9": {"Семья", nil, false},           // устройство удалено
		"abcd1234/m7/d9": {nil, nil, false},               // и ключ удалён
		"ffff0000/m1/d2": {nil, nil, true},                // другая панель на том же awg-ctrl
		"что-то/другое":  {nil, nil, true},
	} {
		k, d, f := s.ownerNames(owner, cache)
		if k != want[0] || d != want[1] || f != want[2] {
			t.Errorf("%s: %v %v %v, ждали %v", owner, k, d, f, want)
		}
	}
}
