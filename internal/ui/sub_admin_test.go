package ui

import (
	"errors"
	"path/filepath"
	"testing"
)

// Ключ, который есть, но выходит за область API-ключа, — 403, а не 404: клиент не должен
// принимать его за удалённый и выдавать вместо него второй.
func TestMasterForScope(t *testing.T) {
	dir := t.TempDir()
	db, err := openUIDB(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Sub{a: &App{db: db, baseDir: dir}} // baseDir: initSchema кладёт туда panel.id
	if err := s.initSchema(); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for id, secret := range map[int]string{1: "a", 2: "b", 3: "c"} {
		exec("INSERT INTO master_keys (id, uuid, label, secret, created_at) VALUES (?, ?, 'k', ?, 1)", id, "u"+secret, []byte(secret))
	}
	exec("INSERT INTO master_servers (master_id, server_id) VALUES (1, 0), (2, 0), (2, 1), (3, 0)")
	exec("UPDATE master_keys SET revoked_at = 1 WHERE id = 3")

	status := func(id int64, scope []int) int {
		_, err := s.masterFor(id, scope)
		var ae *apiError
		if err == nil {
			return 200
		}
		if !errors.As(err, &ae) {
			t.Fatalf("masterFor(%d, %v): %v", id, scope, err)
		}
		return ae.status
	}
	for _, c := range []struct {
		name  string
		id    int64
		scope []int
		want  int
	}{
		{"панель видит всё", 2, nil, 200},
		{"ключ в области", 1, []int{0}, 200},
		{"сервер ключа вне области", 2, []int{0}, 403},
		{"нет такого ключа", 99, []int{0}, 404},
		{"удалённый ключ", 3, []int{0}, 404},
		{"удалённый ключ в панели", 3, nil, 404},
	} {
		if got := status(c.id, c.scope); got != c.want {
			t.Errorf("%s: %d, ожидали %d", c.name, got, c.want)
		}
	}
}
