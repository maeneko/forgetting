package ctrl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maeneko/forgetting/internal/logx"
)

// mtprotoTestServer — Server с настоящей (пустой) users.db: config-ключи и setCfg живые.
func mtprotoTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{db: db, log: logx.New(os.Stderr, t.TempDir()), confDir: t.TempDir(), iface: "awg1"}
}

// fakePath — PATH ровно с этими бинарниками: mtprotoFindBinary и mtprotoFirewallBackend
// видят только то, что положили мы, а не /usr/local/bin машины разработчика.
func fakePath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// fakeRunner — подмена mtprotoRun по ключу «команда аргументы»; остальное — успех.
func fakeRunner(t *testing.T, byCall map[string]cmdResult) *[]string {
	t.Helper()
	old := mtprotoRun
	t.Cleanup(func() { mtprotoRun = old })
	calls := &[]string{}
	mtprotoRun = func(name string, args ...string) cmdResult {
		key := name
		if len(args) > 0 {
			key += " " + strings.Join(args, " ")
		}
		*calls = append(*calls, key)
		if r, ok := byCall[key]; ok {
			return r
		}
		return cmdResult{Code: 0}
	}
	return calls
}

// mtgSecretShape — как `mtg generate-secret --hex <host>`: «ee» + 32 hex + hex(домена).
func mtgSecretShape(host string) string {
	return "ee" + strings.Repeat("ab", 16) + hex.EncodeToString([]byte(host))
}

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

// mtprotoNewSecret — mtg v2: «ee» + 16 случайных байт в hex + hex(FakeTLS-домена).
// Без домена бинарник 2.3.0 отвечает «error: cannot parse config: hostname cannot be
// empty», поэтому пустой хост не должен получаться никогда.
func TestMTProtoSecret(t *testing.T) {
	const host = "google.com"
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		s, err := mtprotoNewSecret(host)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, "ee") {
			t.Fatalf("secret %q: нет префикса ee", s)
		}
		if want := 2 + 32 + 2*len(host); len(s) != want {
			t.Fatalf("secret %q: длина %d, ждём %d (ee + 16 байт hex + hex(%q))", s, len(s), want, host)
		}
		if _, err := hex.DecodeString(s[2:34]); err != nil {
			t.Fatalf("secret %q: 32 символа после ee — не hex: %v", s, err)
		}
		got, ok := mtprotoSecretHost(s)
		if !ok || got != host {
			t.Fatalf("secret %q: домен %q (ok=%v), ждём %q", s, got, ok, host)
		}
		if seen[s] {
			t.Fatalf("secret повторился: %s", s)
		}
		seen[s] = true
	}

	// host нигде не задан — берётся домен по умолчанию, а не пустая строка.
	s := mtprotoTestServer(t)
	h, err := s.mtprotoHostChoice("", "")
	if err != nil || h != mtprotoDefaultHost {
		t.Fatalf("host по умолчанию: %q, %v; ждём %q", h, err, mtprotoDefaultHost)
	}
	if _, err := mtprotoHost(""); err == nil {
		t.Fatal("пустой host принят: секрет без FakeTLS-домена снова стал бы возможен")
	}
}

// mtprotoSecretHost — секрет старого формата («ee» + 32 hex, 34 символа) невалиден:
// mtg его не примет, значит его надо перегенерировать, а не показывать ссылку.
func TestMTProtoLegacySecret(t *testing.T) {
	legacy := "ee" + strings.Repeat("ab", 16) // ровно то, что писал awg-ctrl до FakeTLS
	if len(legacy) != 34 {
		t.Fatalf("legacy: %d символов, ждём 34", len(legacy))
	}
	if _, ok := mtprotoSecretHost(legacy); ok {
		t.Fatal("секрет без домена принят за валидный")
	}
	for _, bad := range []string{"", "ee", "ff" + strings.Repeat("ab", 16), "ee" + strings.Repeat("zz", 17)} {
		if _, ok := mtprotoSecretHost(bad); ok {
			t.Fatalf("%q принят за валидный секрет", bad)
		}
	}

	// Валидный секрет из mtg: hex("google.com") = 676f6f676c652e636f6d.
	if h, ok := mtprotoSecretHost(mtgSecretShape("google.com")); !ok || h != "google.com" {
		t.Fatalf("домен из секрета: %q (ok=%v)", h, ok)
	}

	repaired, regenerated, err := mtprotoRepairSecret(legacy, "google.com")
	if err != nil {
		t.Fatal(err)
	}
	if !regenerated {
		t.Fatal("старый секрет не перегенерирован")
	}
	if h, ok := mtprotoSecretHost(repaired); !ok || h != "google.com" {
		t.Fatalf("после ремонта домен %q (ok=%v)", h, ok)
	}
	if len(repaired) <= len(legacy) {
		t.Fatalf("ремонт вернул ту же длину %d — домена в секрете нет", len(repaired))
	}

	// Валидный секрет с тем же доменом не трогаем: иначе смена порта ломала бы ссылки.
	if again, regen, err := mtprotoRepairSecret(repaired, "google.com"); err != nil || regen || again != repaired {
		t.Fatalf("валидный секрет перегенерирован (regen=%v, err=%v)", regen, err)
	}
	// Смена домена — перегенерация: секрет и mtprotoHost не должны расходиться.
	if _, regen, _ := mtprotoRepairSecret(repaired, "example.org"); !regen {
		t.Fatal("секрет с другим доменом не перегенерирован")
	}
}

// mtprotoHost — FakeTLS-домен: ASCII [a-z0-9.-], минимум одна точка, метки без дефиса
// по краям; на выходе lowercase.
func TestMTProtoHost(t *testing.T) {
	good := map[string]string{
		"google.com":           "google.com",
		"Storage.Google.COM":   "storage.google.com",
		"  example.org  ":      "example.org",
		"a-b.c0.example.co.uk": "a-b.c0.example.co.uk",
		"1.2.3.4":              "1.2.3.4",
		"xn--d1acufc.xn--p1ai": "xn--d1acufc.xn--p1ai",
	}
	for in, want := range good {
		got, err := mtprotoHost(in)
		if err != nil || got != want {
			t.Fatalf("host(%q) = %q, %v; ждём %q", in, got, err, want)
		}
	}
	bad := []string{
		"",
		"   ",
		"localhost",                       // нет точки
		".google.com",                     // ведущая точка
		"google.com.",                     // хвостовая точка
		"google..com",                     // пустая метка
		"-google.com",                     // метка с дефиса
		"google-.com",                     // метка с дефисом на конце
		"goo_gle.com",                     // подчёркивание
		"google com",                      // пробел
		"google.com/x",                    // путь
		"пример.рф",                       // не ASCII
		"google.com:443",                  // порт
		strings.Repeat("a", 250) + ".com", // > 253
	}
	for _, in := range bad {
		if got, err := mtprotoHost(in); err == nil {
			t.Fatalf("host(%q) принят: %q", in, got)
		} else if !strings.HasPrefix(err.Error(), "host: ") {
			t.Fatalf("host(%q): ошибка %q без префикса host:", in, err)
		}
	}
}

// mtprotoConf/mtprotoUnit — чистые builders: systemd получает ровно этот текст.
func TestMTProtoBuilders(t *testing.T) {
	if got, want := mtprotoConf("ee0123", 443), "secret = \"ee0123\"\nbind-to = \"0.0.0.0:443\"\n"; got != want {
		t.Fatalf("conf: %q, ждём %q", got, want)
	}
	want := `[Unit]
Description=awg-mtproto — MTProto proxy (Telegram)
Documentation=https://github.com/9seconds/mtg
After=network.target

[Service]
ExecStart=/usr/local/bin/mtg run /etc/amnezia/amneziawg/mtproto.toml
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`
	if got := mtprotoUnit("/usr/local/bin/mtg", "/etc/amnezia/amneziawg/mtproto.toml"); got != want {
		t.Fatalf("unit:\n%s\nждём:\n%s", got, want)
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

// mtprotoStatus — прокси настроен, но юнит не работает: ссылки нет (она мертва), а Error
// — хвост журнала. Type=simple рапортует успех сразу после exec, поэтому «enable --now»
// ничего не доказывает: mtg умирает следующим тиком, и оператору нужен журнал.
func TestMTProtoStatusStopped(t *testing.T) {
	s := mtprotoTestServer(t)
	fakePath(t, "mtg") // бинарник на месте — проверяем именно ветку с журналом
	s.c = Config{ServerIP: "203.0.113.7"}
	s.setCfg("mtprotoPort", "443")
	s.setCfg("mtprotoSecret", mtgSecretShape("google.com"))
	s.setCfg("mtprotoHost", "google.com")
	fakeRunner(t, map[string]cmdResult{
		"systemctl is-active awg-mtproto": {Stdout: "inactive\n", Code: 3},
		"journalctl -u awg-mtproto --no-pager -n 20": {Stdout: "mtg[1]: cannot parse config: hostname cannot be empty\n" +
			"systemd[1]: awg-mtproto.service: Failed with result 'exit-code'.\n"},
	})

	st := s.mtprotoStatus()
	if st.Running {
		t.Fatal("running=true у неработающего юнита")
	}
	if st.Link != "" {
		t.Fatalf("ссылка у остановленного прокси: %s", st.Link)
	}
	if st.LinkReason != "прокси остановлен" {
		t.Fatalf("link_reason: %q", st.LinkReason)
	}
	want := "mtg[1]: cannot parse config: hostname cannot be empty systemd[1]: awg-mtproto.service: Failed with result 'exit-code'."
	if st.Error != want {
		t.Fatalf("error: %q, ждём %q", st.Error, want)
	}
	if st.Host != "google.com" || st.Port != 443 {
		t.Fatalf("host/port: %q/%d", st.Host, st.Port)
	}
	if st.Firewall.Backend != "none" || !st.Firewall.PortOpen {
		t.Fatalf("firewall: %+v; без фаервола порт открыт по умолчанию", st.Firewall)
	}
}

// mtprotoStatus — работающий прокси: ссылка есть, причины и ошибки нет.
func TestMTProtoStatusRunning(t *testing.T) {
	s := mtprotoTestServer(t)
	fakePath(t, "mtg")
	s.c = Config{ServerIP: "203.0.113.7"}
	secret := mtgSecretShape("google.com")
	s.setCfg("mtprotoPort", "443")
	s.setCfg("mtprotoSecret", secret)
	s.setCfg("mtprotoHost", "google.com")
	fakeRunner(t, map[string]cmdResult{"systemctl is-active awg-mtproto": {Stdout: "active\n"}})

	st := s.mtprotoStatus()
	if !st.Running || st.LinkReason != "" || st.Error != "" {
		t.Fatalf("running=%v link_reason=%q error=%q", st.Running, st.LinkReason, st.Error)
	}
	if want := "https://t.me/proxy?server=203.0.113.7&port=443&secret=" + secret; st.Link != want {
		t.Fatalf("link: %q, ждём %q", st.Link, want)
	}
}

// mtprotoJournalTail — всегда одна строка: переводы строк и отступы схлопываются,
// длинный хвост обрезается до ~500 символов.
func TestMTProtoJournalTail(t *testing.T) {
	s := mtprotoTestServer(t)
	fakeRunner(t, map[string]cmdResult{
		"journalctl -u awg-mtproto --no-pager -n 20": {Stdout: "a\n  b\tc\n\n" + strings.Repeat("x", 900)},
	})
	tail := s.mtprotoJournalTail()
	if strings.ContainsAny(tail, "\n\t") {
		t.Fatalf("хвост журнала не в одну строку: %q", tail)
	}
	if !strings.HasPrefix(tail, "a b c ") {
		t.Fatalf("пробелы не схлопнуты: %q", tail)
	}
	if rs := []rune(tail); len(rs) != 501 || !strings.HasSuffix(tail, "…") {
		t.Fatalf("длина %d, хвост: %q", len(rs), tail[len(tail)-8:])
	}

	fakeRunner(t, map[string]cmdResult{})
	if got := s.mtprotoJournalTail(); got == "" {
		t.Fatal("пустой журнал не дал сообщения")
	}
}

// mtprotoFirewall* — ufw, firewalld и iptables: открытие, защита от дублей и отказ,
// который не валит запуск, а уходит в firewall.error.
func TestMTProtoFirewall(t *testing.T) {
	s := mtprotoTestServer(t)

	t.Run("ufw", func(t *testing.T) {
		fakePath(t, "ufw")
		calls := fakeRunner(t, map[string]cmdResult{
			"ufw status": {Stdout: "Status: active\n\nTo Action From\n-- ------ ----\n443/tcp ALLOW Anywhere\n"},
		})
		fw := s.mtprotoFirewallOpen(443)
		if fw.Backend != "ufw" || !fw.PortOpen || fw.Error != "" {
			t.Fatalf("open: %+v", fw)
		}
		if chk := s.mtprotoFirewallCheck(443); !chk.PortOpen || chk.Error != "" {
			t.Fatalf("check 443: %+v", chk)
		}
		if chk := s.mtprotoFirewallCheck(8443); chk.PortOpen {
			t.Fatalf("незакрытый порт 8443: %+v", chk)
		}
		s.mtprotoFirewallClose(443)
		if !containsCall(*calls, "ufw delete allow 443/tcp") {
			t.Fatalf("порт не закрыт: %v", *calls)
		}
	})

	t.Run("ufw failure", func(t *testing.T) {
		fakePath(t, "ufw")
		fakeRunner(t, map[string]cmdResult{
			"ufw allow 443/tcp": {Stderr: "ERROR: problem running iptables\n", Code: 1},
		})
		fw := s.mtprotoFirewallOpen(443)
		if fw.PortOpen || !strings.Contains(fw.Error, "ERROR: problem running iptables") {
			t.Fatalf("отказ ufw не попал в статус: %+v", fw)
		}
	})

	t.Run("firewalld", func(t *testing.T) {
		fakePath(t, "firewall-cmd")
		calls := fakeRunner(t, map[string]cmdResult{
			"firewall-cmd --query-port=443/tcp": {Code: 1},
		})
		fw := s.mtprotoFirewallOpen(443)
		if fw.Backend != "firewalld" || !fw.PortOpen || fw.Error != "" {
			t.Fatalf("open: %+v", fw)
		}
		if !containsCall(*calls, "firewall-cmd --permanent --add-port=443/tcp") {
			t.Fatalf("правило не добавлено: %v", *calls)
		}
		if !containsCall(*calls, "firewall-cmd --reload") {
			t.Fatalf("firewalld не перезагружен: %v", *calls)
		}
	})

	t.Run("iptables", func(t *testing.T) {
		fakePath(t, "iptables")
		present := false
		old := mtprotoRun
		t.Cleanup(func() { mtprotoRun = old })
		var calls []string
		mtprotoRun = func(name string, args ...string) cmdResult {
			call := name + " " + strings.Join(args, " ")
			calls = append(calls, call)
			switch {
			case strings.HasPrefix(call, "iptables -C "):
				if present {
					return cmdResult{Code: 0}
				}
				return cmdResult{Code: 1}
			case strings.HasPrefix(call, "iptables -I "):
				present = true
				return cmdResult{Code: 0}
			}
			return cmdResult{Code: 0}
		}
		if fw := s.mtprotoFirewallOpen(443); !fw.PortOpen || fw.Backend != "iptables" || fw.Error != "" {
			t.Fatalf("open: %+v", fw)
		}
		if fw := s.mtprotoFirewallOpen(443); !fw.PortOpen {
			t.Fatalf("повторный open: %+v", fw)
		}
		if n := countCalls(calls, "iptables -I INPUT -p tcp --dport 443 -j ACCEPT"); n != 1 {
			t.Fatalf("правило продублировано %d раз: %v", n, calls)
		}
		s.mtprotoFirewallClose(443)
		if !containsCall(calls, "iptables -D INPUT -p tcp --dport 443 -j ACCEPT") {
			t.Fatalf("правило не удалено: %v", calls)
		}
	})
}

func containsCall(calls []string, want string) bool { return countCalls(calls, want) > 0 }

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}
