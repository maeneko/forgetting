package ctrl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/logx"
)

func TestConnLog(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("INSERT INTO users (name, ip, pub_key, vpn_key) VALUES ('ivan', '10.9.0.2', 'PK_IVAN', 'vpn://x')")
	db.Exec("INSERT INTO peers (pub_key, ip, psk_key, owner, created_at) VALUES ('PK_PHONE', '10.9.0.3', 'psk', 'p1/m1/d2', 1)")
	s := &Server{db: db, iface: "awg1", log: logx.New(os.Stderr, t.TempDir())}

	stats := map[string]peerStat{}
	up := true
	newLog := func() *ConnLog {
		l := newConnLog(s)
		l.peersFn = func() (map[string]peerStat, bool) {
			cp := map[string]peerStat{}
			for k, v := range stats {
				cp[k] = v
			}
			return cp, up
		}
		return l
	}
	l := newLog()
	peer := func(pub string, online bool, hs, rx, tx float64) {
		stats[pub] = peerStat{Online: online, LastHandshake: hs, Rx: rx, Tx: tx}
	}
	events := func() string {
		v, err := l.list(0, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var b []string
		for _, e := range v.Get2("events").([]any) {
			o := e.(jsjson.Object)
			s := o.Get2("event").(string) + " " + o.Get2("name").(string) + o.Get2("owner").(string) + " @" + string(jsjson.MustMarshal(o.Get2("at")))
			if o.Get2("event") == "disconnect" {
				s += " dur=" + string(jsjson.MustMarshal(o.Get2("duration"))) + " rx=" + string(jsjson.MustMarshal(o.Get2("rx")))
			}
			b = append([]string{s}, b...) // старые сверху — читать проще
		}
		return strings.Join(b, "; ")
	}

	peer("PK_IVAN", false, 0, 0, 0)
	peer("PK_PHONE", false, 0, 0, 0)
	l.tick(1000)
	if got := events(); got != "" {
		t.Fatalf("без онлайна событий быть не должно: %s", got)
	}

	peer("PK_IVAN", true, 1010, 100, 50) // подключился, handshake в 1010
	l.tick(1030)
	peer("PK_IVAN", true, 1010, 900, 300) // качает
	l.tick(1060)
	peer("PK_IVAN", true, 1130, 900, 300) // keepalive-handshake без трафика
	l.tick(1150)
	up = false // интерфейс лёг — это не отключение
	l.tick(1180)
	up = true
	peer("PK_IVAN", false, 1130, 900, 300)
	l.tick(1330)
	if got, want := events(), "connect ivan @1010; disconnect ivan @1130 dur=120 rx=800"; got != want {
		t.Fatalf("сессия:\n got %s\nwant %s", got, want)
	}

	// Перезапуск awg-ctrl посреди сессии: новый ConnLog поднимает открытую сессию из базы.
	peer("PK_PHONE", true, 2000, 10, 10)
	l.tick(2010)
	l = newLog()
	peer("PK_PHONE", true, 2100, 70, 10)
	l.tick(2110)
	// Пира сняли с интерфейса (удалили устройство) — сессия закрывается.
	delete(stats, "PK_PHONE")
	l.tick(2140)
	want := "connect ivan @1010; disconnect ivan @1130 dur=120 rx=800; connect p1/m1/d2 @2000; disconnect p1/m1/d2 @2110 dur=110 rx=60"
	if got := events(); got != want {
		t.Fatalf("после перезапуска:\n got %s\nwant %s", got, want)
	}

	// Счётчики сбросились (перезапуск интерфейса) — трафик сессии считается с нуля, не уходит в минус.
	peer("PK_IVAN", true, 3000, 5000, 5000)
	l.tick(3010)
	peer("PK_IVAN", true, 3100, 40, 40)
	l.tick(3110)
	peer("PK_IVAN", false, 3100, 40, 40)
	l.tick(3400)
	if got := events(); !strings.HasSuffix(got, "connect ivan @3000; disconnect ivan @3110 dur=110 rx=40") {
		t.Fatalf("сброс счётчиков: %s", got)
	}

	v, _ := l.list(0, 0, "")
	seen := map[string]any{}
	for _, p := range v.Get2("peers").([]any) {
		o := p.(jsjson.Object)
		seen[o.Get2("pub_key").(string)] = o.Get2("last_seen")
	}
	if seen["PK_IVAN"] != float64(3110) && seen["PK_IVAN"] != int64(3110) {
		t.Errorf("last_seen ivan: %v", seen["PK_IVAN"])
	}
	if f, _ := l.list(0, 0, "ivan"); len(f.Get2("events").([]any)) != 4 {
		t.Errorf("поиск по имени: %d событий", len(f.Get2("events").([]any)))
	}
}
