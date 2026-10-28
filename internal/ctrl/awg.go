package ctrl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ───────── запуск команд ─────────

type cmdResult struct {
	Stdout, Stderr string
	Code           int // -1: не запустилась
}

func (r cmdResult) ok() bool { return r.Code == 0 }

func runCmdIn(stdin string, name string, args ...string) cmdResult {
	cmd := exec.Command(name, args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	res := cmdResult{Stdout: so.String(), Stderr: se.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
		} else {
			res.Code = -1
		}
	}
	return res
}

func runCmd(name string, args ...string) cmdResult { return runCmdIn("", name, args...) }

// runShell — аналог execSync(cmd).trim(): через sh, ошибка при ненулевом коде.
func runShell(cmd string) (string, error) {
	r := runCmdIn("", "/bin/sh", "-c", cmd)
	if !r.ok() {
		return "", fmt.Errorf("Command failed: %s\n%s", cmd, r.Stderr)
	}
	return strings.TrimSpace(r.Stdout), nil
}

func runCtx(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var so, se bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(se.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return so.String(), nil
}

// jsNumber — Number(s): пустая строка = 0, нечисло = NaN.
func jsNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// jsonStamp — new Date().toISOString().replace(/[:.]/g, "-").
func jsonStamp(t time.Time) string {
	t = t.UTC()
	return t.Format("2006-01-02T15-04-05") + fmt.Sprintf("-%03dZ", t.Nanosecond()/1e6)
}

// ───────── ключи ─────────

func (s *Server) generateKeys() (Keys, error) {
	priv, err := runShell("umask 077 && awg genkey")
	if err != nil {
		return Keys{}, err
	}
	r := runCmdIn(priv, "awg", "pubkey")
	if !r.ok() {
		return Keys{}, errors.New("awg pubkey завершился с ошибкой")
	}
	psk, err := runShell("awg genpsk")
	if err != nil {
		return Keys{}, err
	}
	return Keys{Private: priv, Public: strings.TrimSpace(r.Stdout), PSK: psk}, nil
}

func (s *Server) genPSK() (string, error) { return runShell("awg genpsk") }

func (s *Server) getServerPublicKey() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.confDir, "server_public.key"))
	if err != nil {
		return "", errors.New("server_public.key не найден")
	}
	return strings.TrimSpace(string(b)), nil
}

// ───────── интерфейс ─────────

type peerStat struct {
	Online        bool
	LastHandshake float64
	Rx, Tx        float64
}

// getPeersData: `awg show awg1 dump`, колонки [0] pub, [4] handshake, [5] rx, [6] tx.
func (s *Server) getPeersData() map[string]peerStat {
	out, err := runShell("awg show " + s.iface + " dump")
	if err != nil {
		s.log.Warn("getPeersData failed", "error", err)
		return map[string]peerStat{}
	}
	res := map[string]peerStat{}
	now := float64(time.Now().Unix())
	lines := strings.Split(out, "\n")
	for i := 1; i < len(lines); i++ {
		parts := strings.Split(lines[i], "\t")
		pub := parts[0]
		if pub == "" {
			continue
		}
		get := func(n int) float64 {
			if n < len(parts) {
				return jsNumber(parts[n])
			}
			return math.NaN() // Number(undefined)
		}
		hs := get(4)
		res[pub] = peerStat{
			Online:        hs > 0 && (now-hs) < 180,
			LastHandshake: hs, Rx: get(5), Tx: get(6),
		}
	}
	return res
}

type ifStatus struct {
	Up        bool    `json:"up"`
	Peers     int     `json:"peers"`
	PublicKey *string `json:"publicKey"`
}

var (
	rePeerLine = regexp.MustCompile(`(?m)^peer:`)
	rePubKey   = regexp.MustCompile(`public key:\s*(.+)`)
)

func (s *Server) interfaceStatus() ifStatus {
	r := runCmd("awg", "show", s.iface)
	if !r.ok() {
		return ifStatus{}
	}
	st := ifStatus{Up: true, Peers: len(rePeerLine.FindAllString(r.Stdout, -1))}
	if m := rePubKey.FindStringSubmatch(r.Stdout); m != nil {
		v := strings.TrimSpace(m[1])
		st.PublicKey = &v
	}
	return st
}

func (s *Server) ensureInterfaceUp() error {
	if !s.interfaceStatus().Up {
		return fmt.Errorf("Interface %s is not up. Run: awg-quick up %s", s.iface, s.iface)
	}
	return nil
}

// readVersions: версия загруженного модуля (важнее версии на диске: интерфейс обслуживает
// именно загруженный) и awg-tools. Вызывается на старте и при restartAwg — только тогда
// модуль может смениться; из /health не вызывается (он без авторизации).
func readVersions() (module, tools string) {
	if b, err := os.ReadFile("/sys/module/amneziawg/version"); err == nil {
		module = strings.TrimSpace(string(b))
	} else if v, err := runShell("modinfo -F version amneziawg"); err == nil {
		module = v
	}
	if v, err := runShell("awg --version"); err == nil {
		for _, t := range strings.Fields(v) {
			if regexp.MustCompile(`^v?\d`).MatchString(t) {
				tools = t
				break
			}
		}
	}
	return
}

// ───────── awg1.conf ─────────

func (s *Server) confPath() string { return filepath.Join(s.confDir, s.iface+".conf") }

func trimEndJS(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

// rebuildConf пересобирает [Peer]-секции из БД; [Interface] сохраняется как есть.
// Пиры устройств идут после users; без записей в peers вывод байт-в-байт прежний.
func (s *Server) rebuildConf() error {
	users, err := s.db.Query("SELECT name, ip, pub_key, psk_key FROM users")
	if err != nil {
		return err
	}
	type pr struct{ label, ip, pub, psk string }
	var all []pr
	for users.Next() {
		var n, ip, pub, psk string
		if err := users.Scan(&n, &ip, &pub, &psk); err != nil {
			users.Close()
			return err
		}
		all = append(all, pr{n, ip, pub, psk})
	}
	users.Close()
	devices, err := s.peersAll()
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.confPath()); err != nil {
		return nil
	}
	raw, err := os.ReadFile(s.confPath())
	if err != nil {
		return err
	}
	for _, d := range devices {
		label := d.Owner
		if label == "" {
			label = "device"
		}
		all = append(all, pr{label, d.IP, d.PubKey, d.PskKey})
	}
	iface := trimEndJS(pPeerSplit.Split(string(raw), 2)[0])
	ka := s.cfg().Keepalive
	blocks := make([]string, 0, len(all))
	for _, u := range all {
		blocks = append(blocks, "\n# "+u.label+"\n[Peer]\nPublicKey = "+u.pub+"\nPresharedKey = "+u.psk+
			"\nAllowedIPs = "+u.ip+"/32\nPersistentKeepalive = "+ka)
	}
	if err := os.WriteFile(s.confPath(), []byte(iface+"\n"+strings.Join(blocks, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	s.log.Info("conf rebuilt", "peers", len(all))
	return nil
}

var reAwgQuickOnly = regexp.MustCompile(`(?i)^\s*(Address|DNS|MTU|Table|PreUp|PostUp|PreDown|PostDown|SaveConfig)\s*=`)
var rePrivateKeyLine = regexp.MustCompile(`^\s*PrivateKey\s*=`)
var reInterfaceLine = regexp.MustCompile(`^\s*\[Interface\]`)

// buildSyncConf готовит «stripped»-конфиг для `awg syncconf`: без awg-quick-ключей, но
// с [Interface] и PrivateKey.
//
// 🛑 КРИТИЧНО: [Interface] с PrivateKey ОБЯЗАН попасть в этот конфиг. Когда syncconf
// получал только [Peer]-блоки, AmneziaWG обнулял приватный ключ интерфейса и параметры
// обфускации — сервер поднимался с public-key=(none), и ВСЕ клиенты отваливались.
// PrivateKey берём из server_private.key — та же идентичность, что в server_public.key
// (его зашивают в vpn:// ключи) и в PostUp.
func (s *Server) buildSyncConf() (string, error) {
	privRaw, err := os.ReadFile(s.serverPrivKeyFile)
	if err != nil {
		return "", err
	}
	priv := strings.TrimSpace(string(privRaw))
	raw, err := os.ReadFile(s.confPath())
	if err != nil {
		return "", err
	}
	var out []string
	replaced := false
	for _, line := range strings.Split(string(raw), "\n") {
		if reAwgQuickOnly.MatchString(line) {
			continue
		}
		if rePrivateKeyLine.MatchString(line) {
			out = append(out, "PrivateKey = "+priv)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		for i, l := range out {
			if reInterfaceLine.MatchString(l) {
				out = append(out[:i+1], append([]string{"PrivateKey = " + priv}, out[i+1:]...)...)
				break
			}
		}
	}
	return strings.Join(out, "\n"), nil
}

func (s *Server) syncPeers() error {
	if err := s.rebuildConf(); err != nil {
		return err
	}
	cfg, err := s.buildSyncConf()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "awg_sync_*.conf") // 0600, O_EXCL, случайное имя
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(cfg); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if r := runCmd("awg", "syncconf", s.iface, f.Name()); !r.ok() {
		s.log.Warn("syncPeers syncconf failed", "stderr", r.Stderr)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) AS n FROM users").Scan(&n)
	s.log.Info("peers synced", "count", n)
	return nil
}

// reloadAwgParams перечитывает параметры после любой правки конфига: иначе ключи
// vpn:// собирались бы на старых параметрах — например, на 2.0 уже после перехода на 3.1.
func (s *Server) reloadAwgParams() string {
	p := ReadParams(s.confPath(), func(m string) { s.log.Warn(m) })
	mtu := ReadMTU(s.confPath())
	s.cfgMu.Lock()
	s.c.Params = p
	s.c.MTU = mtu
	s.c.Gen = p.Gen()
	s.c.Keepalive = keepaliveByGen[s.c.Gen]
	g := s.c.Gen
	s.cfgMu.Unlock()
	s.log.Info("awg params loaded from conf", "Jc", p.Jc, "H1", p.H1, "gen", g)
	return g
}

func (s *Server) restartAwg() error {
	s.log.Info("AWG restart: down")
	if r := runCmd("awg-quick", "down", s.iface); !r.ok() {
		s.log.Warn("awg-quick down failed", "stderr", r.Stderr)
	}
	s.log.Info("AWG restart: up")
	if r := runCmd("awg-quick", "up", s.iface); !r.ok() {
		return fmt.Errorf("awg-quick up failed: %s", r.Stderr)
	}
	// Конфиг мог измениться, пока интерфейс лежал (переход на 3.1, ручная правка) —
	// перечитываем ДО syncPeers: keepalive из CONFIG уходит в [Peer]-блоки.
	gen := s.reloadAwgParams()
	if err := s.syncPeers(); err != nil {
		return err
	}
	go s.shaping.Apply() // tc-правила жили на интерфейсе, который мы только что пересоздали
	m, t := readVersions()
	s.cfgMu.Lock()
	s.c.AwgModule, s.c.AwgTools = m, t
	s.cfgMu.Unlock()
	s.log.Info("AWG restart: done", "module", m, "tools", t, "gen", gen)
	return nil
}

func (s *Server) startInterface() (ifStatus, error) {
	st := s.interfaceStatus()
	if st.Up {
		s.log.Info("AWG already up", "peers", st.Peers)
		return st, nil
	}
	if r := runCmd("awg-quick", "up", s.iface); !r.ok() {
		return ifStatus{}, fmt.Errorf("awg-quick up failed: %s", r.Stderr)
	}
	if err := s.syncPeers(); err != nil {
		return ifStatus{}, err
	}
	go s.shaping.Apply()
	return s.interfaceStatus(), nil
}

// setPeer: PSK уходит во временный файл, а не в аргументы — иначе он виден в /proc/<pid>/cmdline.
func (s *Server) setPeer(pub, psk, ip string) cmdResult {
	f, err := os.CreateTemp("", "awg_psk_*.tmp")
	if err != nil {
		return cmdResult{Stderr: err.Error(), Code: -1}
	}
	defer os.Remove(f.Name())
	f.WriteString(psk)
	f.Close()
	return runCmd("awg", "set", s.iface, "peer", pub,
		"preshared-key", f.Name(),
		"allowed-ips", ip+"/32",
		"persistent-keepalive", s.cfg().Keepalive)
}

// kernelPeers — ключи пиров, стоящих в ядре (nil — не удалось спросить).
func (s *Server) kernelPeers() map[string]bool {
	r := runCmd("awg", "show", s.iface, "peers")
	if !r.ok() {
		return nil
	}
	set := map[string]bool{}
	for _, l := range strings.Split(r.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			set[l] = true
		}
	}
	return set
}

// dropFromKernel снимает пира с интерфейса и убеждается, что его там нет. Порядок во
// всех вызывающих: сначала ядро, потом база — при отказе строка остаётся, и повторная
// попытка (надгробие панели, следующий sync) найдёт пира.
func (s *Server) dropFromKernel(pub string) bool {
	r := runCmd("awg", "set", s.iface, "peer", pub, "remove")
	if r.ok() {
		return true
	}
	if left := s.kernelPeers(); left != nil && !left[pub] {
		return true // ответ странный, но пира уже нет
	}
	s.log.Error("peer remove from kernel failed", "pub", pub, "stderr", r.Stderr)
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func b64Std(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
func stdB64(b []byte) string          { return base64.StdEncoding.EncodeToString(b) }

// lenientB64URL повторяет Buffer.from(s, "base64url") в Node: терпит паддинг, «+», «/».
func lenientB64URL(s string) ([]byte, error) {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	return base64.RawURLEncoding.DecodeString(s)
}
