package ctrl

import (
	"strings"
	"testing"
)

func TestGenI1Templates(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		v := GenI1()
		b, ok := i1Bytes(v)
		if !ok {
			t.Fatalf("шаблон не разбирается: %q", v)
		}
		kind, label := DescribeI1(v)
		seen[kind] = true
		switch kind {
		case "dns":
			if !strings.HasPrefix(v, "<r 2>") || !strings.HasPrefix(label, "DNS-запрос к ") {
				t.Fatalf("DNS: %q → %q", v, label)
			}
		case "quic":
			if n := len(b); n != 1200 && n != 1250 && n != 1252 {
				t.Fatalf("QUIC длиной %d: %q", n, v)
			}
		case "stun":
			if len(b) != 20 && len(b) != 28 {
				t.Fatalf("STUN длиной %d: %q", len(b), v)
			}
		default:
			t.Fatalf("неожиданный вид %q: %q", kind, v)
		}
		// Только <b>/<r>, без пробелов вне тегов — так I1 пишут и клиенты AmneziaVPN.
		if strings.Contains(strings.NewReplacer("<b ", "", "<r ", "").Replace(v), " ") {
			t.Fatalf("лишние пробелы: %q", v)
		}
	}
	for _, k := range []string{"dns", "quic", "stun"} {
		if !seen[k] {
			t.Fatalf("шаблон %s ни разу не выпал", k)
		}
	}
}

func TestDescribeI1(t *testing.T) {
	cases := map[string]string{
		DefaultParams().I1: "DNS-ответ от icloud.com",
		"":                 "без маскировки",
		"<c><t>":           "свой шаблон",
		"<b 0xdeadbeef>":   "свой шаблон",
	}
	for in, want := range cases {
		if _, got := DescribeI1(in); got != want {
			t.Errorf("DescribeI1(%q) = %q, want %q", in, got, want)
		}
	}
}
