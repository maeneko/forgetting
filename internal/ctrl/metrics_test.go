package ctrl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/logx"
)

// SQLite открыт с одним соединением: любой запрос в базу изнутри цикла по строкам users ждал
// бы его вечно и намертво вешал awg-ctrl (так было в 0.4.2 с меткой торрента). Скрейп метрик
// идёт раз в 15 с, поэтому зависание проявлялось сразу.
func TestCollectMetricsNoDeadlock(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		db.Exec("INSERT INTO users (name, ip, pub_key, vpn_key) VALUES (?, ?, ?, 'vpn://x')", fmt.Sprintf("u%d", i), fmt.Sprintf("10.9.0.%d", i+2), fmt.Sprintf("pk%d", i))
	}
	s := &Server{db: db, iface: "awg1", log: logx.New(os.Stderr, t.TempDir())}
	s.shaping = newShaping(s)
	s.torrent = newTorrent(s)
	done := make(chan error, 1)
	go func() { _, err := s.collectMetrics(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collectMetrics завис: запрос в базу изнутри цикла по строкам")
	}
}
