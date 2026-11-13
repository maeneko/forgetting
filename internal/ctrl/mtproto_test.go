package ctrl

import (
	"strings"
	"testing"
)

// mtprotoLink — собранная ссылка для клиента Telegram; без адреса/порта/секрета — пусто.
func TestMTProtoLink(t *testing.T) {
	if got := mtprotoLink("203.0.113.7", 443, "ee0102"); got != "https://t.me/proxy?server=203.0.113.7&port=443&secret=ee0102" {
		t.Fatalf("link: %s", got)
	}
	for _, c := range []struct{ ip string; port int; secret string }{
		{"", 443, "ee01"}, {"203.0.113.7", 0, "ee01"}, {"203.0.113.7", 443, ""},
	} {
		if got := mtprotoLink(c.ip, c.port, c.secret); got != "" {
			t.Fatalf("link(%q,%d,%q): %q, ждём пусто", c.ip, c.port, c.secret, got)
		}
	}
}

// mtprotoNewSecret — «ee» + 16 случайных байт в hex (34 символа), секреты не повторяются.
func TestMTProtoSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		s, err := mtprotoNewSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, "ee") || len(s) != 34 {
			t.Fatalf("secret %q: не ee + 32 hex", s)
		}
		if seen[s] {
			t.Fatalf("secret повторился: %s", s)
		}
		seen[s] = true
	}
}

// portTaken — определяет занятый порт по выводу ss (IPv4, IPv6, localhost) и не путает
// с похожими портами (4439 не «занят», когда слушают 443).
func TestPortTaken(t *testing.T) {
	fake := "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\n" +
		"LISTEN 0      128          0.0.0.0:22        0.0.0.0:*    users:(\"sshd\")\n" +
		"LISTEN 0      128             [::]:443           [::]:*    users:(\"nginx\")\n" +
		"LISTEN 0      128          127.0.0.1:5432     0.0.0.0:*\n"
	taken := func(out string, port int) bool { return portTakenFrom(out, port) }
	if !taken(fake, 443) || !taken(fake, 22) || !taken(fake, 5432) {
		t.Fatal("занятые порты не найдены")
	}
	if taken(fake, 4439) || taken(fake, 80) {
		t.Fatal("свободный порт посчитан занятым")
	}
}
