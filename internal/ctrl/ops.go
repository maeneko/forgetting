package ctrl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// HTTPError — ошибка с кодом ответа; любая другая ошибка превращается в 500.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

func httpErr(status int, msg string) error { return &HTTPError{status, msg} }

// ───────── пользователи (vpn://) ─────────

func (s *Server) addUser(name string) (*userRow, error) {
	keys, err := s.generateKeys()
	if err != nil {
		return nil, err
	}
	serverPub, err := s.getServerPublicKey()
	if err != nil {
		return nil, err
	}
	cfg := s.cfg()

	var ip string
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	ip, err = s.nextIp(tx, cfg.Subnet)
	if err == nil {
		_, err = tx.Exec("INSERT INTO users (name, ip, pub_key, vpn_key, psk_key, key_gen) VALUES (?, ?, ?, ?, ?, ?)",
			name, ip, keys.Public, "", keys.PSK, cfg.Gen)
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	vpnKey, err := EncodeVpnKey(s.keyEnv(), keys, ip, serverPub)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec("UPDATE users SET vpn_key = ? WHERE name = ?", vpnKey, name); err != nil {
		return nil, err
	}

	if r := s.setPeer(keys.Public, keys.PSK, ip); !r.ok() {
		s.db.Exec("DELETE FROM users WHERE name = ?", name)
		return nil, fmt.Errorf("awg set failed: %s", r.Stderr)
	}
	if err := s.rebuildConf(); err != nil {
		return nil, err
	}
	s.log.Info("user created", "name", name, "ip", ip, "gen", cfg.Gen)
	return &userRow{Name: name, IP: ip, PubKey: keys.Public, VpnKey: vpnKey, PskKey: keys.PSK, KeyGen: cfg.Gen}, nil
}

func (s *Server) removeUser(name string) error {
	u, err := s.userGet(name)
	if err != nil {
		return err
	}
	if u == nil {
		return errors.New("Пользователь не найден") // в Node — обычный Error → 500
	}
	if !s.dropFromKernel(u.PubKey) {
		return errors.New("Не удалось снять пира с интерфейса")
	}
	if _, err := s.db.Exec("DELETE FROM users WHERE name = ?", name); err != nil {
		return err
	}
	if err := s.rebuildConf(); err != nil {
		return err
	}
	for _, ext := range []string{".conf", ".key"} {
		f := filepath.Join(s.confDir, "clients", name+ext)
		if _, err := os.Stat(f); err == nil {
			os.Remove(f)
		}
	}
	s.log.Info("user removed", "name", name)
	return nil
}

type reissueResult struct {
	Total       int      `json:"total"`
	Reissued    int      `json:"reissued"`
	Regenerated []string `json:"regenerated"`
	Backup      string   `json:"backup"`
}

// reissueAll перевыпускает все vpn:// ключи на текущих параметрах интерфейса — после смены
// поколения старые перестают работать. IP, pub_key и psk_key сохраняются: на проводе ничего
// не меняется, клиенту достаточно заново импортировать ключ.
func (s *Server) reissueAll() (*reissueResult, error) {
	users, err := s.usersAll()
	if err != nil {
		return nil, err
	}
	serverPub, err := s.getServerPublicKey()
	if err != nil {
		return nil, err
	}
	backup := s.dbPath + ".bak-" + jsonStamp(time.Now())
	if _, err := s.db.Exec("VACUUM INTO ?", backup); err != nil {
		return nil, err
	}
	s.log.Info("reissue: db backed up", "backup", backup, "users", len(users))

	res := &reissueResult{Total: len(users), Regenerated: []string{}, Backup: backup}
	cfg := s.cfg()
	for _, u := range users {
		var keys Keys
		if priv := clientPrivKeyFrom(u.VpnKey); priv != "" {
			keys = Keys{Private: priv, Public: u.PubKey, PSK: u.PskKey}
		} else {
			// Блоб не разобрался — личность пира восстановить неоткуда, выдаём новую.
			s.log.Warn("reissue: vpn_key не декодируется, генерируем новую пару", "name", u.Name)
			keys, err = s.generateKeys()
			if err != nil {
				return nil, err
			}
			runCmd("awg", "set", s.iface, "peer", u.PubKey, "remove")
			if r := s.setPeer(keys.Public, keys.PSK, u.IP); !r.ok() {
				s.log.Error("reissue: awg set failed", "name", u.Name, "stderr", r.Stderr)
				continue
			}
			res.Regenerated = append(res.Regenerated, u.Name)
		}
		vk, err := EncodeVpnKey(s.keyEnv(), keys, u.IP, serverPub)
		if err != nil {
			return nil, err
		}
		if _, err := s.db.Exec("UPDATE users SET vpn_key_prev = vpn_key, vpn_key = ?, pub_key = ?, psk_key = ?, key_gen = ? WHERE name = ?",
			vk, keys.Public, keys.PSK, cfg.Gen, u.Name); err != nil {
			return nil, err
		}
		res.Reissued++
	}
	if err := s.syncPeers(); err != nil {
		return nil, err
	}
	s.log.Info("reissue: done", "total", len(users), "reissued", res.Reissued, "regenerated", len(res.Regenerated))
	return res, nil
}

// ───────── пиры устройств (sen://) ─────────
// awg-ctrl про мастер-ключи ничего не знает: ему приходит «добавь пира с этим публичным
// ключом». Приватного ключа устройства здесь нет и быть не может.

var rePub = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// checkPubKey: base64 от 32 байт (44 символа, последний «=»).
func checkPubKey(k string) (string, error) {
	if !rePub.MatchString(k) {
		return "", httpErr(400, "Неверный публичный ключ")
	}
	b, err := b64Std(k)
	if err != nil || len(b) != 32 {
		return "", httpErr(400, "Неверный публичный ключ")
	}
	return k, nil
}

// pubKeyFromParam: в пути base64url — «/» и «+» обычного base64 ломают маршрут.
func pubKeyFromParam(p string) (string, error) {
	b, err := lenientB64URL(p)
	if err != nil {
		return "", httpErr(400, "Неверный публичный ключ")
	}
	return checkPubKey(stdB64(b))
}

type ipPsk struct {
	IP  string `json:"ip"`
	PSK string `json:"psk_key"`
}

func (s *Server) assertPubKeyFree(q queryer, pub string) error {
	var n int
	if err := q.QueryRow("SELECT COUNT(*) FROM peers WHERE pub_key = ?", pub).Scan(&n); err != nil {
		return err
	}
	taken, err := s.userPubTaken(q, pub)
	if err != nil {
		return err
	}
	if n > 0 || taken {
		return httpErr(409, "Публичный ключ уже используется")
	}
	if sp, err := s.getServerPublicKey(); err != nil {
		return err
	} else if pub == sp {
		return httpErr(409, "Публичный ключ уже используется")
	}
	return nil
}

func (s *Server) addPeer(pub, owner string) (*ipPsk, error) {
	if _, err := checkPubKey(pub); err != nil {
		return nil, err
	}
	psk, err := s.genPSK()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	var ip string
	err = s.assertPubKeyFree(tx, pub)
	if err == nil {
		ip, err = s.nextIp(tx, s.cfg().Subnet)
	}
	if err == nil {
		_, err = tx.Exec("INSERT INTO peers (pub_key, ip, psk_key, owner, created_at) VALUES (?, ?, ?, ?, ?)",
			pub, ip, psk, owner, time.Now().Unix())
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if r := s.setPeer(pub, psk, ip); !r.ok() {
		s.db.Exec("DELETE FROM peers WHERE pub_key = ?", pub)
		return nil, fmt.Errorf("awg set failed: %s", r.Stderr)
	}
	if err := s.rebuildConf(); err != nil {
		return nil, err
	}
	s.log.Info("peer added", "owner", owner, "ip", ip)
	return &ipPsk{ip, psk}, nil
}

// replacePeer — смена ключа устройства: IP и PSK остаются, меняется только pub_key.
func (s *Server) replacePeer(oldPub, newPub string) (*ipPsk, error) {
	if _, err := checkPubKey(newPub); err != nil {
		return nil, err
	}
	old, err := s.peerGet(oldPub)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, httpErr(404, "Пир не найден")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if err = s.assertPubKeyFree(tx, newPub); err == nil {
		_, err = tx.Exec("UPDATE peers SET pub_key = ? WHERE pub_key = ?", newPub, oldPub)
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if r := s.setPeer(newPub, old.PskKey, old.IP); !r.ok() {
		s.db.Exec("UPDATE peers SET pub_key = ? WHERE pub_key = ?", oldPub, newPub)
		return nil, fmt.Errorf("awg set failed: %s", r.Stderr)
	}
	// Старый ключ не снялся — пир остаётся «призраком» в ядре до аудита панели; смену ключа
	// не откатываем: новый уже стоит, а клиент к этому моменту получил бы ошибку.
	if !s.dropFromKernel(oldPub) {
		s.log.Error("peer rekey: старый ключ остался в ядре", "owner", old.Owner)
	}
	if err := s.rebuildConf(); err != nil {
		return nil, err
	}
	s.log.Info("peer rekeyed", "owner", old.Owner, "ip", old.IP)
	return &ipPsk{old.IP, old.PskKey}, nil
}

func (s *Server) rotatePeerPSK(pub string) (*ipPsk, error) {
	p, err := s.peerGet(pub)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, httpErr(404, "Пир не найден")
	}
	psk, err := s.genPSK()
	if err != nil {
		return nil, err
	}
	if r := s.setPeer(pub, psk, p.IP); !r.ok() {
		return nil, fmt.Errorf("awg set failed: %s", r.Stderr)
	}
	if _, err := s.db.Exec("UPDATE peers SET psk_key = ? WHERE pub_key = ?", psk, pub); err != nil {
		return nil, err
	}
	if err := s.rebuildConf(); err != nil {
		return nil, err
	}
	s.log.Info("peer psk rotated", "owner", p.Owner)
	return &ipPsk{p.IP, psk}, nil
}

func (s *Server) removePeer(pub string) error {
	p, err := s.peerGet(pub)
	if err != nil {
		return err
	}
	if p == nil {
		return httpErr(404, "Пир не найден")
	}
	if !s.dropFromKernel(pub) {
		return errors.New("Не удалось снять пира с интерфейса")
	}
	if _, err := s.db.Exec("DELETE FROM peers WHERE pub_key = ?", pub); err != nil {
		return err
	}
	if err := s.rebuildConf(); err != nil {
		return err
	}
	s.log.Info("peer removed", "owner", p.Owner)
	return nil
}

var (
	rePanelID     = regexp.MustCompile(`^[a-z0-9]{8,16}$`)
	reLegacyOwner = regexp.MustCompile(`^m\d+/d\d+$`)
)

const syncMax = 10_000

type legacyPeer struct {
	PubKey string `json:"pub_key"`
	Owner  string `json:"owner"`
	IP     string `json:"ip"`
}
type syncResult struct {
	Present []string     `json:"present"`
	Removed int          `json:"removed"`
	Legacy  []legacyPeer `json:"legacy"`
}

// syncOwnedPeers: панель присылает полный список ключей, которые ей нужны на этом сервере,
// — всё остальное из её области («<panel>/…») снимается. Удаляем только по такому явному
// списку, никогда по времени или неактивности: панель не на связи — ничего не происходит.
// Пиры без области (owner «m1/d2», выданы до областей) панель забирает себе, только если их
// ключ есть в её списке; остальные не трогаем — чьи они, уже не разобрать.
func (s *Server) syncOwnedPeers(panel string, keep []string) (*syncResult, error) {
	want := map[string]bool{}
	for _, k := range keep {
		want[k] = true
	}
	prefix := panel + "/"
	peers, err := s.peersAll()
	if err != nil {
		return nil, err
	}
	var removed []peerRow
	claimed := 0
	for _, p := range peers {
		switch {
		case strings.HasPrefix(p.Owner, prefix):
			// Ядро первым: не снялся — строка остаётся, следующий sync попробует снова.
			if !want[p.PubKey] && s.dropFromKernel(p.PubKey) {
				if _, err := s.db.Exec("DELETE FROM peers WHERE pub_key = ?", p.PubKey); err != nil {
					return nil, err
				}
				removed = append(removed, p)
			}
		case reLegacyOwner.MatchString(p.Owner) && want[p.PubKey]:
			if _, err := s.db.Exec("UPDATE peers SET owner = ? WHERE pub_key = ?", prefix+p.Owner, p.PubKey); err != nil {
				return nil, err
			}
			claimed++
		}
	}
	if len(removed) > 0 || claimed > 0 {
		if err := s.rebuildConf(); err != nil {
			return nil, err
		}
	}
	if len(removed) > 0 {
		owners := make([]string, len(removed))
		for i, p := range removed {
			owners[i] = p.Owner
		}
		s.log.Info("peers synced out", "panel", panel, "removed", owners)
	}
	all, err := s.peersAll()
	if err != nil {
		return nil, err
	}
	res := &syncResult{Present: []string{}, Removed: len(removed), Legacy: []legacyPeer{}}
	for _, p := range all {
		if strings.HasPrefix(p.Owner, prefix) {
			res.Present = append(res.Present, p.PubKey)
		}
		if reLegacyOwner.MatchString(p.Owner) {
			res.Legacy = append(res.Legacy, legacyPeer{p.PubKey, p.Owner, p.IP})
		}
	}
	return res, nil
}

// ───────── переход 2.0 → 3.1 ─────────

// Header protection использует библиотечный chacha-API, которого нет до ядра 5.5 — там
// модуль 3.x просто не соберётся (upstream issue #210).
func kernelRelease() string {
	r := runCmd("uname", "-r")
	return strings.TrimSpace(r.Stdout)
}

func kernelSupportsHeaderProtection(release string) bool {
	parts := strings.Split(release, ".")
	if len(parts) < 2 {
		return false
	}
	maj, e1 := strconv.Atoi(leadingDigits(parts[0]))
	min, e2 := strconv.Atoi(leadingDigits(parts[1]))
	if e1 != nil || e2 != nil {
		return false
	}
	return maj > 5 || (maj == 5 && min >= 5)
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// checkGen31Support решает ЗАГРУЖЕННЫЙ модуль, а не тот, что лежит на диске после apt.
func (s *Server) checkGen31Support() string {
	module, _ := readVersions()
	if !strings.HasPrefix(module, "3.") {
		m := module
		if m == "" {
			m = "неизвестной версии"
		}
		return fmt.Sprintf("В ядре загружен модуль amneziawg %s — для 3.1 нужна 3.x. ", m) +
			"Обнови пакет (apt) и перезагрузи сервер, затем повтори переход."
	}
	if rel := kernelRelease(); !kernelSupportsHeaderProtection(rel) {
		return fmt.Sprintf("Ядро %s старше 5.5 — header protection в нём не работает (upstream issue #210).", rel)
	}
	return ""
}

var reAwg3KeyLine = regexp.MustCompile(`^\s*(` + strings.Join(AWG3Keys, "|") + `)\s*=`)

// confWithGen31 готовит текст awg1.conf для 3.1: выкидывает старые 3.x-строки (конфиг могли
// править руками), поднимает S1–S4 до допустимых при header protection и дописывает 3.x-блок
// в конец [Interface]. [Peer]-блоки не трогаются — личности пиров переход не меняет.
func confWithGen31(conf, headerKey string) (string, []string) {
	var head, rest string
	if loc := pPeerSplit.FindStringIndex(conf); loc != nil {
		head, rest = conf[:loc[0]], conf[loc[0]:]
	} else {
		head = conf
	}
	var lines []string
	for _, l := range strings.Split(head, "\n") {
		if !reAwg3KeyLine.MatchString(l) {
			lines = append(lines, l)
		}
	}
	// Пустые строки и «# имя» в хвосте [Interface] относятся уже к первому [Peer] (их пишет
	// rebuildConf) — отделяем, чтобы 3.x-строки встали внутрь [Interface].
	var tail []string
	for len(lines) > 0 {
		t := strings.TrimSpace(lines[len(lines)-1])
		if t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		tail = append([]string{lines[len(lines)-1]}, tail...)
		lines = lines[:len(lines)-1]
	}
	if tt := strings.TrimSpace(strings.Join(tail, "\n")); tt != "" {
		rest = tt + "\n" + rest
	}
	iface := strings.Join(lines, "\n")

	// S ниже 12 (или вовсе отсутствующий) ядро с header protection не примет. Правка безопасна
	// ровно потому, что следом идёт перевыпуск ключей: обе стороны получают новые значения
	// одновременно. (Регулярка с \s*, как в Node-версии: захватывает и переводы строк.)
	def := DefaultParams()
	defaults := map[string]int{"S1": def.S1, "S2": def.S2, "S3": def.S3, "S4": def.S4}
	bumped := []string{}
	for _, k := range []string{"S1", "S2", "S3", "S4"} {
		re := regexp.MustCompile(`(?m)^\s*` + k + `\s*=\s*(\d+)\s*$`)
		m := re.FindStringSubmatch(iface)
		cur := -1
		if m != nil {
			cur, _ = strconv.Atoi(m[1])
		}
		if m != nil && cur >= minSForHeaderProtection {
			continue
		}
		val := defaults[k]
		if minSForHeaderProtection > val {
			val = minSForHeaderProtection
		}
		if m != nil {
			// как iface.replace(re, …) без флага g: заменяется только первое совпадение
			loc := re.FindStringIndex(iface)
			iface = iface[:loc[0]] + fmt.Sprintf("%s = %d", k, val) + iface[loc[1]:]
		} else {
			iface = trimEndJS(iface) + fmt.Sprintf("\n%s = %d", k, val)
		}
		was := "нет"
		if m != nil {
			was = strconv.Itoa(cur)
		}
		bumped = append(bumped, fmt.Sprintf("%s: %s → %d", k, was, val))
	}

	a3 := []string{"HeaderProtectionKey = " + headerKey}
	for _, kv := range AWG31Defaults {
		a3 = append(a3, kv[0]+" = "+kv[1])
	}
	out := trimEndJS(iface) + "\n" + strings.Join(a3, "\n") + "\n"
	if rest != "" {
		out += "\n" + rest
	}
	return out, bumped
}

type upgradeResult struct {
	Gen     string         `json:"gen"`
	Backup  string         `json:"backup"`
	BumpedS []string       `json:"bumpedS"`
	Reissue *reissueResult `json:"reissue"`
}

// upgradeToGen31: единственная настоящая проверка, что ядро приняло параметры, — подъём
// интерфейса, поэтому шаги такие: бэкап конфига → запись 3.1 → restartAwg() → перевыпуск
// ключей. Если ядро отказало, возвращаем прежний конфиг и поднимаем интерфейс обратно:
// операция либо применяется целиком, либо не оставляет следов.
func (s *Server) upgradeToGen31() (*upgradeResult, error) {
	before, err := os.ReadFile(s.confPath())
	if err != nil {
		return nil, err
	}
	backup := s.confPath() + ".bak-" + jsonStamp(time.Now())
	if err := os.WriteFile(backup, before, 0o600); err != nil {
		return nil, err
	}
	hk, err := s.genPSK()
	if err != nil {
		return nil, err
	}
	conf, bumped := confWithGen31(string(before), hk)
	if err := os.WriteFile(s.confPath(), []byte(conf), 0o600); err != nil {
		return nil, err
	}
	s.log.Info("upgrade: conf rewritten for 3.1", "backup", backup, "bumpedS", bumped)

	rerr := s.restartAwg()
	if rerr == nil && s.cfg().Gen != "3.1" {
		rerr = errors.New("интерфейс поднялся, но конфиг всё ещё 2.0")
	}
	if rerr != nil {
		s.log.Error("upgrade: kernel rejected 3.1, rolling back", "error", rerr)
		os.WriteFile(s.confPath(), before, 0o600)
		if e2 := s.restartAwg(); e2 != nil {
			s.log.Error("upgrade: rollback restart failed", "error", e2)
		}
		return nil, fmt.Errorf("Ядро не приняло конфиг 3.1 — вернули прежний (копия: %s). "+
			"Причина видна в dmesg после `echo \"module amneziawg +p\" > /sys/kernel/debug/dynamic_debug/control`.", backup)
	}

	// Ключи, выданные на 2.0-параметрах, после смены поколения не подключатся, поэтому
	// перевыпускаем сразу — сервер не должен оставаться в состоянии «3.1 поднят, у всех
	// нерабочие ключи». Личности пиров сохраняются.
	re, err := s.reissueAll()
	if err != nil {
		return nil, err
	}
	s.log.Info("upgrade: done", "gen", s.cfg().Gen, "reissued", re.Reissued, "bumpedS", bumped)
	return &upgradeResult{Gen: s.cfg().Gen, Backup: backup, BumpedS: bumped, Reissue: re}, nil
}

var _ = jsjson.Undefined
