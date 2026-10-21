package ui

// Вкладка «Перенос»: панель переезжает с сервера «панель + VPN» на отдельную панель без VPN
// прямо из интерфейса, без архива в руках. Те же шаги, что `awg-control migrate` (общий код —
// internal/migrate), только архив идёт из панели в панель:
//
//	новая: POST /ui/migrate/code     → одноразовый код awgmove:// (адрес, порт панели, ключ)
//	старая: POST /ui/migrate/export  → замораживает /sub/v1, шлёт зашифрованный архив на
//	                                    POST /migrate/v1/import новой панели
//	новая: раскладывает архив, заводит ноду #0, отвечает join-строкой и перезапускается
//	старая: пишет PANEL_MOVED и запускает `awg-control join` — становится нодой #0 с ретранслятором
//
// Перезапуск и join делает CLI в отдельном процессе: панель останавливает сама себя.

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maeneko/forgetting/internal/envfile"
	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/migrate"
)

const (
	moveCodeTTL   = time.Hour
	moveMaxFails  = 5
	moveMaxBundle = 1 << 30
	moveTimeout   = 10 * time.Minute
)

// Состояние переноса на этой панели (в памяти: перезапуск его сбрасывает, и так и надо).
const (
	moveIdle      = ""
	moveSending   = "sending"   // старая: архив в пути, /sub/v1 заморожен
	moveMoved     = "moved"     // старая: новая приняла, сейчас будет join
	moveUncertain = "uncertain" // старая: архив ушёл, а ответа нет — новая могла его принять
	moveImported  = "imported"  // новая: архив принят, сейчас перезапуск
)

type Migration struct {
	a       *App
	envFile string               // cli.env: сюда пишутся порты, PANEL_MOVED, RELAY_FROM
	run     func(args ...string) // `awg-control …` отдельным процессом; в тестах — заглушка

	mu      sync.Mutex
	key     []byte // новая: ключ действующего кода
	host    string
	expires time.Time
	fails   int
	state   string
	errText string
	target  string // старая: адрес новой панели
}

func newMigration(a *App) *Migration {
	m := &Migration{a: a, envFile: os.Getenv("CLI_ENV_FILE")}
	m.run = m.spawn
	return m
}

func (a *App) filePath(env, name string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return filepath.Join(a.baseDir, name)
}

// paths — где awg-ui держит файлы из архива (те же правила, что при их загрузке).
func (m *Migration) paths() migrate.Paths {
	return migrate.PathsFrom(func(env string) string {
		for _, f := range migrate.Files {
			if f.Env == env {
				return m.a.filePath(env, f.Name)
			}
		}
		return ""
	})
}

// canExport / canAccept — "" или причина, почему нельзя.
func (m *Migration) canExport() string {
	switch {
	case m.envFile == "":
		return "Панель запущена не через awg-control — перенос недоступен"
	case os.Getenv("CORE_HOST") != "":
		return "Этот сервер уже нода другой панели"
	case !m.a.nodes.local:
		return "У этой панели нет своего VPN: переносить её как сервер-ноду нечего"
	}
	return ""
}

func (m *Migration) canAccept() string {
	switch {
	case m.envFile == "":
		return "Панель запущена не через awg-control — перенос недоступен"
	case m.a.nodes.local:
		return "У этой панели свой VPN. Принять перенос может только панель без VPN: install.sh, роль «только панель»"
	case !m.a.nodes.hubReady():
		return "Не настроен приём нод (NODE_PORT): старый сервер подключится сюда нодой"
	case migrate.Count(m.a.db) > 0:
		return "На панели уже есть серверы, мастер-ключи или ключи API — принять перенос можно только на чистую"
	}
	return ""
}

func (m *Migration) router() *Router {
	rt := newRouter()
	rt.Get("/", m.status)
	rt.Post("/code", m.newCode)
	rt.Delete("/code", func(c *Ctx) bool {
		m.mu.Lock()
		m.key = nil
		m.mu.Unlock()
		return m.status(c)
	})
	rt.Post("/export", m.export)
	rt.Post("/cancel", m.cancel)
	rt.Delete("/retire", func(c *Ctx) bool {
		if err := envfile.Set(m.envFile, nil, "MOVED_IN_AT"); err != nil {
			return c.Error(500, err.Error())
		}
		os.Unsetenv("MOVED_IN_AT")
		return m.status(c)
	})
	return rt
}

func (m *Migration) status(c *Ctx) bool {
	m.mu.Lock()
	state, errText, target := m.state, m.errText, m.target
	var code any
	if m.key != nil && time.Now().Before(m.expires) {
		code = jsjson.Object{{Key: "code", Val: migrate.Code{Host: m.host, Port: m.a.port, Key: m.key}.String()},
			{Key: "expires_in", Val: int(time.Until(m.expires).Seconds())}}
	}
	m.mu.Unlock()
	o := jsjson.Object{{Key: "state", Val: state}, {Key: "error", Val: orNil(errText)}, {Key: "target", Val: orNil(target)}, {Key: "code", Val: code}}
	// После приёма база закрыта до перезапуска — что можно, уже не считаем.
	if state != moveImported {
		o = append(o, jsjson.KV{Key: "export", Val: orNil(m.canExport())}, jsjson.KV{Key: "accept", Val: orNil(m.canAccept())},
			jsjson.KV{Key: "retire", Val: m.retire()})
	}
	return c.JSON(200, o)
}

// ── Новая панель ────────────────────────────────────────────────────────────

// retire — можно ли выключить старый сервер (ноду #0 с ретранслятором): кто из клиентов и нод
// уже знает новый адрес панели. Клиент узнаёт его из endpoints любого конфига после переноса,
// нода — из ready (core_ok). Ссылки sen:// без устройств ещё никем не открыты: в них старый
// адрес, их выдают заново. nil — панель ничего не принимала (или отметку убрали).
func (m *Migration) retire() any {
	since, _ := strconv.ParseInt(os.Getenv("MOVED_IN_AT"), 10, 64)
	if since == 0 || m.a.nodes.local {
		return nil
	}
	home := m.a.nodes.home()
	var total, fresh, unused int
	m.a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(d.last_seen >= ?), 0) FROM devices d
		JOIN master_keys k ON k.id = d.master_id WHERE k.revoked_at IS NULL`, since).Scan(&total, &fresh)
	m.a.db.QueryRow(`SELECT COUNT(*) FROM master_keys k WHERE k.revoked_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM devices d WHERE d.master_id = k.id)`).Scan(&unused)
	nodes := []any{}
	if rows, err := m.a.db.Query("SELECT id, name, core_ack FROM nodes WHERE id != 0 AND pub IS NOT NULL ORDER BY id"); err == nil {
		for rows.Next() {
			var id int64
			var name string
			var ack sql.NullString
			if rows.Scan(&id, &name, &ack) == nil {
				nodes = append(nodes, jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: name},
					{Key: "online", Val: m.a.nodes.IsOnline(int(id))}, {Key: "ok", Val: home != "" && ack.String == home}})
			}
		}
		rows.Close()
	}
	return jsjson.Object{{Key: "since", Val: since}, {Key: "home", Val: orNil(home)},
		{Key: "old", Val: jsjson.Object{{Key: "exists", Val: m.a.nodes.Exists(0)}, {Key: "online", Val: m.a.nodes.IsOnline(0)}}},
		{Key: "devices", Val: jsjson.Object{{Key: "total", Val: total}, {Key: "fresh", Val: fresh}}},
		{Key: "unused_links", Val: unused}, {Key: "nodes", Val: nodes}}
}

func (m *Migration) newCode(c *Ctx) bool {
	if why := m.canAccept(); why != "" {
		return c.Error(409, why)
	}
	host, _ := c.Obj().Get2("host").(string)
	if h := strings.TrimSpace(os.Getenv("SUB_HOST")); h != "" {
		host = h
	}
	if !reHost.MatchString(host) {
		return c.Error(400, "Не удалось определить адрес панели — задайте SUB_HOST")
	}
	m.mu.Lock()
	m.key, m.host, m.expires, m.fails = migrate.NewKey(), host, time.Now().Add(moveCodeTTL), 0
	m.mu.Unlock()
	logf("migrate: выдан код переноса (действует час)")
	return m.status(c)
}

// importHandler — POST /migrate/v1/import, без JWT: доступ даёт только ключ кода (тег GCM).
// Ответ с ошибкой — открытым текстом и только пока ничего не тронуто.
func (m *Migration) importHandler(c *Ctx) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil || time.Now().After(m.expires) {
		return c.Error(404, "Новая панель не ждёт переноса: выдайте код заново")
	}
	box, err := io.ReadAll(http.MaxBytesReader(c.W, c.R.Body, moveMaxBundle))
	if err != nil {
		return c.Error(413, "Архив слишком большой")
	}
	plain, err := migrate.OpenRequest(m.key, box)
	if err != nil {
		if m.fails++; m.fails >= moveMaxFails {
			m.key = nil
		}
		warnf("migrate: архив не расшифрован ip=%s", c.remoteIP())
		return c.Error(403, "Код не подходит")
	}
	key := m.key
	m.key = nil // код одноразовый
	if why := m.canAccept(); why != "" {
		return c.Error(409, why)
	}
	b, err := migrate.Read(bytes.NewReader(plain))
	if err != nil {
		return c.Error(400, err.Error())
	}
	defer b.Close()
	host := m.host
	plan, err := migrate.NewPlan(b, func(k string) int {
		switch k {
		case "SUB_PORT":
			return m.a.sub.port
		case "NODE_PORT":
			return m.a.nodes.port
		}
		return 0
	}, m.a.port, host)
	if err != nil {
		return c.Error(409, err.Error())
	}
	if ip := net.ParseIP(c.remoteIP()); ip != nil {
		plan.TrustRelay(ip.String())
	}

	// Дальше файлы меняются: база закрывается (WAL сливается в файл до того, как его отложат).
	paths := m.paths()
	fail := func(err error) bool {
		m.state, m.errText = moveImported, "Перенос прерван: "+err.Error()+". Прежние файлы — в *.bak-<время>."
		warnf("migrate: %v", err)
		return c.Error(500, m.errText)
	}
	m.a.db.Close()
	secret := migrate.NewKey()[:16]
	if err := migrate.Install(b, paths); err != nil {
		return fail(err)
	}
	if err := migrate.AddNodeZero(paths["ui.db"], b.Str("server_name"), secret); err != nil {
		return fail(err)
	}
	pin, err := migrate.CertPin(paths["node_tls.crt"])
	if err != nil {
		return fail(fmt.Errorf("сертификат хаба: %w", err))
	}
	if err := envfile.Set(m.envFile, plan.Env); err != nil {
		return fail(err)
	}
	join := migrate.JoinString(host, plan.NodePort, pin, 0, secret, plan.UIPort, plan.SubPort)
	m.state = moveImported
	logf("migrate: панель «%s» принята с %s — перезапуск", b.Str("server_name"), c.remoteIP())
	m.run("restart")
	resp := jsjson.MustMarshal(jsjson.Object{{Key: "join", Val: join}})
	c.W.Header().Set("Content-Type", "application/octet-stream")
	c.W.WriteHeader(200)
	c.W.Write(migrate.SealResponse(key, resp))
	return false
}

// ── Старая панель ───────────────────────────────────────────────────────────

func (m *Migration) export(c *Ctx) bool {
	if why := m.canExport(); why != "" {
		return c.Error(409, why)
	}
	codeStr, _ := c.Obj().Get2("code").(string)
	code, err := migrate.ParseCode(codeStr)
	if err != nil {
		return c.Error(400, err.Error())
	}
	m.mu.Lock()
	if m.state != moveIdle {
		m.mu.Unlock()
		return c.Error(409, "Перенос уже идёт")
	}
	m.state, m.errText = moveSending, ""
	m.mu.Unlock()
	end := func(state, errText string) {
		m.mu.Lock()
		m.state, m.errText = state, errText
		m.mu.Unlock()
		m.a.sub.frozen.Store(state != moveIdle)
	}
	// Пока архив в пути, подписка отвечает «недоступна» (клиенты держат туннель и повторят):
	// регистрация или смена ключа сейчас остались бы только в старой базе.
	m.a.sub.frozen.Store(true)

	var buf bytes.Buffer
	if err := migrate.Write(&buf, m.a.db, m.paths(), m.exportMeta()); err != nil {
		end(moveIdle, "")
		return c.Error(500, "Архив не собран: "+err.Error())
	}
	logf("migrate: архив %d КБ → %s", buf.Len()/1024, code.URL())
	cl := &http.Client{Timeout: moveTimeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Post(code.URL(), "application/octet-stream", bytes.NewReader(migrate.SealRequest(code.Key, buf.Bytes())))
	if err != nil {
		// Не дозвонились — новая точно ничего не получила. Иначе архив мог дойти.
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			end(moveIdle, "")
			return c.Error(502, "Новая панель не отвечает: "+err.Error())
		}
		return m.uncertain(c, end, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		end(moveIdle, "")
		msg := "Новая панель отказала (" + strconv.Itoa(resp.StatusCode) + ")"
		if v, derr := jsjson.Decode(raw); derr == nil {
			if e, ok := field(v, "error").(string); ok {
				msg = "Новая панель: " + e
			}
		}
		return c.Error(502, msg)
	}
	var plain []byte
	if err == nil {
		plain, err = migrate.OpenResponse(code.Key, raw)
	}
	var join string
	if err == nil {
		v, derr := jsjson.Decode(plain)
		join, _ = field(v, "join").(string)
		if derr != nil || !strings.HasPrefix(join, "awgjoin://") {
			err = errors.New("ответ без join-строки")
		}
	}
	if err != nil {
		return m.uncertain(c, end, err)
	}

	if err := envfile.Set(m.envFile, [][2]string{{"PANEL_MOVED", "on"}}); err != nil {
		return m.uncertain(c, end, err)
	}
	target := strings.TrimSuffix(code.URL(), "/migrate/v1/import")
	m.mu.Lock()
	m.target = target
	m.mu.Unlock()
	end(moveMoved, "")
	logf("migrate: новая панель приняла перенос — join, этот сервер станет нодой #0")
	m.run("join", join)
	return c.JSON(200, jsjson.Object{{Key: "ok", Val: true}, {Key: "target", Val: target}})
}

// uncertain — архив ушёл, а подтверждения нет. Подписка остаётся замороженной: если новая
// панель его приняла, вторая живая копия базы разошлась бы с ней.
func (m *Migration) uncertain(c *Ctx, end func(string, string), err error) bool {
	msg := "Неизвестно, принят ли перенос (" + err.Error() + "). Откройте новую панель: если там есть сервер #0 — " +
		"выполните здесь `awg-ctrl join` со строкой из его карточки; если нет — «Отменить перенос»."
	end(moveUncertain, msg)
	warnf("migrate: %s", msg)
	return c.Error(502, msg)
}

func (m *Migration) cancel(c *Ctx) bool {
	m.mu.Lock()
	ok := m.state == moveUncertain
	if ok {
		m.state, m.errText = moveIdle, ""
	}
	m.mu.Unlock()
	if !ok {
		return c.Error(409, "Отменять нечего")
	}
	m.a.sub.frozen.Store(false)
	logf("migrate: перенос отменён — подписка снова работает")
	return m.status(c)
}

func (m *Migration) exportMeta() jsjson.Object {
	name, ip := "VPN", os.Getenv("SERVER_IP")
	if r, err := m.a.ctrlLocal("GET", "/health", nil); err == nil {
		if s, ok := field(r.Data, "server").(string); ok && s != "" {
			name = s
		}
		if s, ok := field(r.Data, "ip").(string); ok && ip == "" {
			ip = s
		}
	}
	return jsjson.Object{
		{Key: "version", Val: m.a.version}, {Key: "created_at", Val: now()},
		{Key: "server_name", Val: name}, {Key: "server_ip", Val: ip},
		{Key: "ui_port", Val: m.a.port}, {Key: "sub_port", Val: m.a.sub.port}, {Key: "sub_tls", Val: m.a.sub.useTLS},
		{Key: "node_port", Val: m.a.nodes.port},
	}
}

// spawn — `awg-control <args>` отдельным процессом через секунду (ответ успеет уйти). Окружение —
// чистое: CLI читает cli.env заново, а унаследованные значения перекрыли бы новые.
func (m *Migration) spawn(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		warnf("migrate: %v", err)
		return
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || k == "LANG" || k == "TZ" || k == "AWG_ROOT" || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	logFile := filepath.Join(filepath.Dir(m.a.baseDir), "logs", "awg-migrate.log")
	go func() {
		time.Sleep(time.Second)
		cmd := exec.Command(exe, args...)
		cmd.Env, cmd.Dir = env, "/"
		if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			defer f.Close()
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // переживёт эту панель
		if err := cmd.Start(); err != nil {
			warnf("migrate: awg-control %s: %v", args[0], err)
			return
		}
		cmd.Wait()
	}()
}
