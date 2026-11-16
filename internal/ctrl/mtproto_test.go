package ctrl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// mtgArchive — tar.gz как у релиза 9seconds/mtg: каталог mtg-<версия>-linux-<арх>/mtg.
func mtgArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// extractMtg берёт именно бинарник mtg из вложенного каталога и не зависит от версии в пути.
func TestExtractMtg(t *testing.T) {
	want := []byte("#!/bin/sh\necho 2.3.0\n")
	got, err := extractMtg(bytes.NewReader(mtgArchive(t, "mtg-2.3.0-linux-amd64/mtg", want)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("бинарник: %q, ждём %q", got, want)
	}

	for _, name := range []string{"mtg-2.3.0-linux-amd64/README.md", "mtg-2.3.0-linux-amd64/nested/mtg2"} {
		if _, err := extractMtg(bytes.NewReader(mtgArchive(t, name, want))); err == nil {
			t.Fatalf("%s: ждали ошибку «нет mtg»", name)
		}
	}
}

// mtprotoMtgURL — только amd64/arm64 и версия из константы (синхронна install.sh).
func TestMTProtoMtgURL(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		got, err := mtprotoMtgURL(arch)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://github.com/9seconds/mtg/releases/download/v2.3.0/mtg-2.3.0-linux-" + arch + ".tar.gz"
		if got != want {
			t.Fatalf("%s: %q, ждём %q", arch, got, want)
		}
	}
	if _, err := mtprotoMtgURL("386"); err == nil {
		t.Fatal("386: ждали ошибку")
	}
}

// mtprotoLink — собранная ссылка для клиента Telegram; без адреса/порта/секрета — пусто.
func TestMTProtoLink(t *testing.T) {
	if got := mtprotoLink("203.0.113.7", 443, "ee0102"); got != "https://t.me/proxy?server=203.0.113.7&port=443&secret=ee0102" {
		t.Fatalf("link: %s", got)
	}
	for _, c := range []struct {
		ip     string
		port   int
		secret string
	}{
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
