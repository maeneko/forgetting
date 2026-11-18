package ui

// Таблица бакетов ОС для devices.platform — та же, что в решении (§7), плюс строки,
// которые встречаются у реальных клиентов. Сырое значение наружу не уходит никогда:
// это текст, который пишет клиент (в нём бывает и имя устройства).

import (
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func TestPlatformOS(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		// android — по префиксу: клиенты шлют «Android 14», а не «android».
		{"android", "android"},
		{"androidTv", "android"},
		{"Android 14", "android"},
		{"Samsung SM-S918", "android"},
		{"Pixel 8", "android"},
		// Xiaomi остаётся android: это правило таблицы решения (§7), хотя бриф и
		// называет его «мусором»; вендор — валидный признак Android.
		{"Xiaomi 14", "android"},

		{"windows", "windows"},
		{"windows 10", "windows"},
		{"Windows 11", "windows"},
		{"Win32", "windows"},

		{"macos", "macos"},
		{"macOS", "macos"},
		{"Mac OS X", "macos"},
		{"Mac OS X 10.15", "macos"},
		{"mac os x", "macos"},
		{"Darwin 23", "macos"},

		{"ios", "ios"},
		{"iOS 17", "ios"},
		{"iPhone OS", "ios"},
		{"iPadOS 18", "ios"},

		{"linux", "linux"},
		{"GNU/Linux", "linux"},
		{"Ubuntu 24.04", "linux"},
		{"Debian 13", "linux"},

		{"", "unknown"},
		{"   ", "unknown"},
		{"\t\n", "unknown"},

		{"FreeBSD", "other"},
		{"tv", "other"},
		{"chromeos", "other"},
		{"? ", "other"},

		// Регистр и пробелы по краям.
		{"  IOS  ", "ios"},
		{"  Android 14\t", "android"},
	}
	for _, c := range cases {
		if got := platformOS(c.raw); got != c.want {
			t.Errorf("platformOS(%q) = %q, ожидали %q", c.raw, got, c.want)
		}
	}

	// ios проверяется до macos: строка с «os x» не должна стать ios, а «mac os» не
	// должна уехать в linux или other.
	for _, raw := range []string{"Mac OS X", "mac os x", "macOS 15", "Mac OS X 10.15"} {
		if got := platformOS(raw); got != "macos" {
			t.Errorf("%q: %q, ожидали macos (порядок ios → macos)", raw, got)
		}
	}
	if got := platformOS("iOS 17"); got != "ios" {
		t.Errorf("ios обязан остаться ios, а не macos: %q", got)
	}
}

func TestPlatformBlock(t *testing.T) {
	rows := []osRow{
		{"Android 14", 2},
		{"androidTv", 1},
		{"iOS", 3},
		{"FreeBSD", 2},
		{"", 1},
	}
	got := platformBlock(rows)
	want := `{"total":9,"known":6,"by_os":[` +
		`{"os":"android","devices":3},` +
		`{"os":"windows","devices":0},` +
		`{"os":"macos","devices":0},` +
		`{"os":"linux","devices":0},` +
		`{"os":"ios","devices":3},` +
		`{"os":"other","devices":2},` +
		`{"os":"unknown","devices":1}]}`
	if s := string(jsjson.MustMarshal(got)); s != want {
		t.Errorf("platformBlock:\n получили %s\n ожидали %s", s, want)
	}

	// Порядок всегда osOrder, даже когда все счётчики нулевые.
	empty := platformBlock(nil)
	byOS := arr(field(empty, "by_os"))
	if len(byOS) != len(osOrder) {
		t.Fatalf("by_os: %d записей, ожидали %d", len(byOS), len(osOrder))
	}
	for i, os := range osOrder {
		if x, _ := field(byOS[i], "os").(string); x != os {
			t.Errorf("by_os[%d] = %q, ожидали %q", i, x, os)
		}
	}
	if v := field(empty, "total"); v != 0 {
		t.Errorf("пустой total = %v", v)
	}
	if v := field(empty, "known"); v != 0 {
		t.Errorf("пустой known = %v", v)
	}
}
