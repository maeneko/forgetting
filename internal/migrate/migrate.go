// Package migrate — перенос панели с сервера «панель + VPN» на отдельную панель без VPN: архив
// (ui.db, ключи подписки и хаба, порты), его раскладка на новой панели и нода #0. Общий для
// CLI (`awg-control migrate`) и вкладки «Перенос» в панели. Порядок и смысл —
// docs/node-protocol.md, «Перенос панели».
//
// Старый сервер остаётся нодой #0: id сервера в ui.db не меняется, поэтому конфиг клиента
// (servers[] и ключи) тот же байт в байт, а пиры на его awg-ctrl уже принадлежат области
// перенесённой панели (sub_meta.panel_id едет вместе с ui.db).
package migrate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/maeneko/forgetting/internal/jsjson"
)

var b64url = base64.RawURLEncoding

const (
	Version = 1
	maxFile = 1 << 30 // ui.db крупнее гигабайта — явно не то
	JoinTTL = 3600    // как у панели: одноразовый секрет живёт час
)

// File — что едет в архиве: имя внутри архива и переменная пути в окружении awg-ui.
type File struct {
	Name, Env string
	Required  bool
}

var Files = []File{
	{"ui.db", "UI_DB_FILE", true},
	// 🛑 Ключ подписи и TLS подписки зашиты во все выданные sen://, сертификат хаба — во все
	// join-строки и подключённые ноды: без них перенос ломает и ссылки, и ноды.
	{"sub_sign.key", "SUB_SIGN_KEY_FILE", false},
	{"sub_tls.key", "SUB_TLS_KEY_FILE", false},
	{"sub_tls.crt", "SUB_TLS_CERT_FILE", false},
	{"node_tls.key", "NODE_TLS_KEY_FILE", false},
	{"node_tls.crt", "NODE_TLS_CERT_FILE", false},
}

// Paths — имя файла в архиве → путь на этой машине.
type Paths map[string]string

// PathsFrom — пути из переменных окружения awg-ui (get(env) → путь).
func PathsFrom(get func(env string) string) Paths {
	p := Paths{}
	for _, f := range Files {
		p[f.Name] = get(f.Env)
	}
	return p
}

// OpenDB — ui.db тем же драйвером, что у awg-ui.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ── Архив ───────────────────────────────────────────────────────────────────

// Write — архив в w: снимок db (VACUUM INTO — целостный, вместе с WAL), файлы из p и meta
// (+ panel_id из базы). Отсутствующие необязательные файлы пропускаются.
func Write(w io.Writer, db *sql.DB, p Paths, meta jsjson.Object) error {
	tmp, err := os.MkdirTemp("", "forgetting-migrate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, "ui.db")
	if _, err := db.Exec("VACUUM INTO ?", snap); err != nil {
		return fmt.Errorf("снимок ui.db: %w", err)
	}
	var panelID string
	db.QueryRow("SELECT value FROM sub_meta WHERE key = 'panel_id'").Scan(&panelID)
	meta = append(jsjson.Object{{Key: "v", Val: Version}, {Key: "panel_id", Val: panelID}}, meta...)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	err = add("meta.json", jsjson.MustMarshal(meta))
	for _, f := range Files {
		if err != nil {
			break
		}
		src := p[f.Name]
		if f.Name == "ui.db" {
			src = snap
		}
		raw, rerr := os.ReadFile(src)
		if errors.Is(rerr, os.ErrNotExist) && !f.Required {
			continue
		}
		if rerr != nil {
			err = rerr
			break
		}
		err = add(f.Name, raw)
	}
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	return err
}

type Bundle struct {
	Dir  string
	Meta jsjson.Object
	Has  map[string]bool
}

func (b *Bundle) Close() { os.RemoveAll(b.Dir) }

func (b *Bundle) Num(k string) int {
	f, _ := b.Meta.Get2(k).(float64)
	return int(f)
}

func (b *Bundle) Str(k string) string {
	s, _ := b.Meta.Get2(k).(string)
	return s
}

// Read — распаковка во временный каталог; в архиве допустимы только известные имена.
func Read(r io.Reader) (*Bundle, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("не архив переноса: %w", err)
	}
	dir, err := os.MkdirTemp("", "forgetting-import-")
	if err != nil {
		return nil, err
	}
	b := &Bundle{Dir: dir, Has: map[string]bool{}}
	fail := func(err error) (*Bundle, error) { b.Close(); return nil, err }
	allowed := map[string]bool{"meta.json": true}
	for _, f := range Files {
		allowed[f.Name] = true
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("архив повреждён: %w", err))
		}
		if h.Typeflag != tar.TypeReg || !allowed[h.Name] || b.Has[h.Name] || h.Size > maxFile {
			return fail(fmt.Errorf("в архиве лишнее: %q", h.Name))
		}
		dst, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.Copy(dst, io.LimitReader(tr, maxFile))
			if cerr := dst.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return fail(err)
		}
		b.Has[h.Name] = true
	}
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	var v any
	if err == nil {
		v, err = jsjson.Decode(raw)
	}
	meta, ok := v.(jsjson.Object)
	if err != nil || !ok || meta.Get2("v") != float64(Version) || !b.Has["ui.db"] {
		return fail(errors.New("не архив переноса или от другой версии (нужны meta.json v1 и ui.db)"))
	}
	b.Meta = meta
	return b, nil
}

// PanelData — сколько в базе того, что перенос затёр бы (мастер-ключи, ноды, ключи API).
func PanelData(file string) (int, error) {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	db, err := OpenDB(file)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return Count(db), nil
}

func Count(db *sql.DB) int {
	total := 0
	for _, t := range []string{"master_keys", "nodes", "api_keys"} {
		var n int
		// Таблицы может не быть — awg-ui ещё не запускался.
		if db.QueryRow("SELECT COUNT(*) FROM "+t).Scan(&n) == nil {
			total += n
		}
	}
	return total
}

// ── Раскладка на новой панели ──────────────────────────────────────────────

// Plan — порты и переменные cli.env новой панели. Подписка и хаб — на прежних портах: они в
// ссылках, у нод и в DNS, если адрес — домен.
type Plan struct {
	SubPort, NodePort, UIPort int
	Host                      string
	Env                       [][2]string
}

// NewPlan — env(k) → текущий порт этой панели (0 — не задан), host — её публичный адрес.
func NewPlan(b *Bundle, env func(k string) int, uiPort int, host string) (*Plan, error) {
	p := &Plan{SubPort: b.Num("sub_port"), NodePort: b.Num("node_port"), UIPort: uiPort, Host: host}
	if p.SubPort == 0 {
		p.SubPort = env("SUB_PORT")
	}
	if p.NodePort == 0 {
		p.NodePort = env("NODE_PORT")
	}
	if p.NodePort == 0 {
		return nil, errors.New("приём нод не настроен (NODE_PORT): старый сервер подключится к панели нодой")
	}
	if p.UIPort != 0 && (p.UIPort == p.SubPort || p.UIPort == p.NodePort) || p.SubPort != 0 && p.SubPort == p.NodePort {
		return nil, fmt.Errorf("порты пересекаются: панель %d, подписка %d, хаб %d — смените UI_PORT", p.UIPort, p.SubPort, p.NodePort)
	}
	for _, kv := range [][2]string{{"SUB_PORT", strconv.Itoa(p.SubPort)}, {"NODE_PORT", strconv.Itoa(p.NodePort)}, {"SUB_HOST", host}} {
		if kv[1] != "0" && kv[1] != "" {
			p.Env = append(p.Env, kv)
		}
	}
	tlsOn := "off"
	if b.Has["sub_tls.crt"] && b.Meta.Get2("sub_tls") == true {
		tlsOn = "on"
	}
	p.Env = append(p.Env, [2]string{"SUB_TLS", tlsOn})
	// С этого момента панель считает, кто из клиентов и нод узнал её новый адрес.
	p.Env = append(p.Env, [2]string{"MOVED_IN_AT", strconv.FormatInt(time.Now().Unix(), 10)})
	// Заголовку PROXY от ретранслятора верим только с адреса старого сервера.
	if ip := b.Str("server_ip"); ip != "" {
		p.Env = append(p.Env, [2]string{"RELAY_FROM", ip})
	}
	return p, nil
}

// TrustRelay — ещё один адрес ретранслятора: старый сервер может выходить в сеть не с того IP,
// что у него в SERVER_IP (NAT, второй адрес). Панель видит, откуда пришёл архив, — его и берём.
func (p *Plan) TrustRelay(ip string) {
	if ip == "" {
		return
	}
	for i, kv := range p.Env {
		if kv[0] == "RELAY_FROM" {
			for _, have := range strings.Split(kv[1], ",") {
				if have == ip {
					return
				}
			}
			p.Env[i][1] += "," + ip
			return
		}
	}
	p.Env = append(p.Env, [2]string{"RELAY_FROM", ip})
}

// Install — файлы архива на свои места; прежние уходят в *.bak-<время>.
func Install(b *Bundle, p Paths) error {
	stamp := time.Now().Format("20060102-150405")
	for _, f := range Files {
		if !b.Has[f.Name] {
			continue
		}
		dst := p[f.Name]
		olds := []string{dst}
		if f.Name == "ui.db" {
			olds = append(olds, dst+"-wal", dst+"-shm")
		}
		for _, o := range olds {
			if _, err := os.Stat(o); err == nil {
				if err := os.Rename(o, o+".bak-"+stamp); err != nil {
					return fmt.Errorf("не удалось отложить %s: %w", o, err)
				}
			}
		}
		raw, err := os.ReadFile(filepath.Join(b.Dir, f.Name))
		if err == nil {
			err = os.MkdirAll(filepath.Dir(dst), 0o700)
		}
		if err == nil {
			err = os.WriteFile(dst, raw, 0o600)
		}
		if err != nil {
			return fmt.Errorf("не удалось записать %s: %w", dst, err)
		}
	}
	return nil
}

// AddNodeZero — старый сервер в реестре нод под прежним id 0, ждёт первого подключения.
func AddNodeZero(dbFile, name string, secret []byte) error {
	if name == "" || len([]rune(name)) > 40 || strings.ContainsAny(name, "\r\n<>") {
		name = "VPN"
	}
	db, err := OpenDB(dbFile)
	if err != nil {
		return err
	}
	defer db.Close()
	h := sha256.Sum256(secret)
	now := time.Now().Unix()
	if _, err := db.Exec("INSERT INTO nodes (id, name, join_hash, join_expires, created_at) VALUES (0, ?, ?, ?, ?)",
		name, h[:], now+JoinTTL, now); err != nil {
		return fmt.Errorf("нода #0 не заведена (база со старой версии? обновите старый сервер): %w", err)
	}
	return nil
}

// CertPin — SHA-256 SPKI сертификата, base64url: то же, что pin в join-строке.
func CertPin(file string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return "", errors.New("не PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return b64url.EncodeToString(sum[:]), nil
}

// JoinString — как у панели (nodes.go), плюс ui/sub: куда ретранслятору старого сервера слать
// прежние порты панели и подписки. Остальные читатели строки лишних полей не видят.
func JoinString(host string, port int, pin string, id int64, secret []byte, uiPort, subPort int) string {
	o := jsjson.Object{{Key: "v", Val: 1}, {Key: "h", Val: host}, {Key: "p", Val: port},
		{Key: "pin", Val: pin}, {Key: "n", Val: id}, {Key: "s", Val: b64url.EncodeToString(secret)}}
	if uiPort > 0 {
		o = append(o, jsjson.KV{Key: "ui", Val: uiPort})
	}
	if subPort > 0 {
		o = append(o, jsjson.KV{Key: "sub", Val: subPort})
	}
	return "awgjoin://" + b64url.EncodeToString(jsjson.MustMarshal(o))
}
