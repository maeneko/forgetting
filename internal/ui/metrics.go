package ui

// Prometheus: GET /metrics на ядре (порт awg-ui/metrics.ts). Метрики ядра и всех нод
// собираются здесь же — у нод через уже открытый WSS. Доступ — отдельный токен «только чтение
// метрик»; он не открывает ни /api/v1, ни панель.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	scrapeDeadline = 5 * time.Second
	touchEveryS    = 60 // last_used пишем не чаще раза в минуту, а не на каждый скрейп
)

func hashToken(t string) []byte {
	s := sha256.Sum256([]byte(t))
	return s[:]
}

type Metrics struct {
	a         *App
	perPeer   bool
	localFile string

	mu        sync.Mutex
	localMod  time.Time
	localSeen bool
	localHash []byte
}

func newMetrics(a *App) (*Metrics, error) {
	m := &Metrics{
		a: a,
		// Пер-пировые серии (peer = внутренний IP, name = имя пользователя) выключены: это и
		// приватность, и кардинальность. Включается явно.
		perPeer:   strings.ToLower(envOr("METRICS_PEERS", "off")) == "on",
		localFile: envOr("MONITOR_HASH_FILE", "/etc/amnezia/amneziawg/monitor_token.sha256"),
	}
	_, err := a.db.Exec(`
        CREATE TABLE IF NOT EXISTS metrics_token (
            id         INTEGER PRIMARY KEY CHECK (id = 1),
            token_hash BLOB    NOT NULL,
            prefix     TEXT    NOT NULL,
            created_at INTEGER NOT NULL,
            last_used  INTEGER
        )
    `)
	return m, err
}

var reHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// localTokenHash — токен встроенного Prometheus (`awg-ctrl monitor install`): здесь его хэш.
// Файл перечитывается по mtime: установка не требует перезапуска панели.
func (m *Metrics) localTokenHash() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := os.Stat(m.localFile)
	if err != nil {
		m.localSeen, m.localHash = false, nil
		return nil
	}
	if !m.localSeen || !st.ModTime().Equal(m.localMod) {
		raw, err := os.ReadFile(m.localFile)
		if err != nil {
			m.localSeen, m.localHash = false, nil
			return nil
		}
		m.localSeen, m.localMod, m.localHash = true, st.ModTime(), nil
		if h := strings.TrimSpace(string(raw)); reHex64.MatchString(h) {
			m.localHash, _ = hex.DecodeString(h)
		}
	}
	return m.localHash
}

type tokenRow struct {
	hash      []byte
	prefix    string
	createdAt int64
	lastUsed  sql.NullInt64
}

func (m *Metrics) token() *tokenRow {
	t := &tokenRow{}
	if m.a.db.QueryRow("SELECT token_hash, prefix, created_at, last_used FROM metrics_token WHERE id = 1").Scan(&t.hash, &t.prefix, &t.createdAt, &t.lastUsed) != nil {
		return nil
	}
	return t
}

func (m *Metrics) view() jsjson.Object {
	if t := m.token(); t != nil {
		return jsjson.Object{{Key: "exists", Val: true}, {Key: "prefix", Val: t.prefix}, {Key: "created_at", Val: t.createdAt},
			{Key: "last_used", Val: nullInt(t.lastUsed)}, {Key: "per_peer", Val: m.perPeer}}
	}
	return jsjson.Object{{Key: "exists", Val: false}, {Key: "per_peer", Val: m.perPeer}}
}

// router — управление токеном (роуты панели, за JWT). Выпуск и перевыпуск — одно и то же:
// старый токен перестаёт работать. Значение отдаётся один раз.
func (m *Metrics) router() *Router {
	rt := newRouter()
	rt.Get("/", func(c *Ctx) bool { return c.JSON(200, m.view()) })
	rt.Post("/", func(c *Ctx) bool {
		b := make([]byte, 24)
		rand.Read(b)
		tok := "awgm_" + b64url.EncodeToString(b)
		m.a.db.Exec("INSERT OR REPLACE INTO metrics_token (id, token_hash, prefix, created_at, last_used) VALUES (1, ?, ?, ?, NULL)",
			hashToken(tok), tok[:13]+"…", now())
		return c.JSON(201, append(m.view(), jsjson.KV{Key: "token", Val: tok}))
	})
	rt.Delete("/", func(c *Ctx) bool {
		m.a.db.Exec("DELETE FROM metrics_token WHERE id = 1")
		return c.JSON(200, m.view())
	})
	return rt
}

func writeText(c *Ctx, status int, ctype, body string) bool {
	c.W.Header().Set("Content-Type", ctype)
	c.W.WriteHeader(status)
	c.W.Write([]byte(body))
	return false
}

// requireToken — нет токена или он неверный — одинаковый 401: не подсказываем, выключены
// метрики или токен не тот.
func (m *Metrics) requireToken(c *Ctx) bool {
	given := bearer(c.R)
	row := m.token()
	var gh []byte
	if strings.HasPrefix(given, "awgm_") {
		gh = hashToken(given)
	}
	panelOK := row != nil && gh != nil && len(row.hash) == len(gh) && subtle.ConstantTimeCompare(gh, row.hash) == 1
	local := m.localTokenHash()
	localOK := gh != nil && local != nil && subtle.ConstantTimeCompare(gh, local) == 1
	if !panelOK && !localOK {
		c.W.Header().Set("WWW-Authenticate", "Bearer")
		return writeText(c, 401, "text/plain; charset=utf-8", "unauthorized\n")
	}
	// last_used — про токен, выпущенный в панели (внешний Prometheus); встроенный в нём не участвует.
	if t := now(); panelOK && (!row.lastUsed.Valid || row.lastUsed.Int64 == 0 || t-row.lastUsed.Int64 >= touchEveryS) {
		m.a.db.Exec("UPDATE metrics_token SET last_used = ? WHERE id = 1", t)
	}
	return true
}

// ── Экспозиция ──────────────────────────────────────────────────────────────

type label struct {
	k string
	v any
}

type sample struct {
	labels []label
	v      any // число; не число и не конечное — 0
}

type exposition struct {
	b    bytes.Buffer
	seen map[string]bool
}

var (
	escLabel = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	escHelp  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

// labelString — String(v) для значения метки.
func labelString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return jsjson.Number(float64(x))
	case int64:
		return jsjson.Number(float64(x))
	case float64:
		return jsjson.Number(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return "undefined"
	}
	return "[object Object]"
}

func (e *exposition) add(name, typ, help string, samples []sample) {
	if len(samples) == 0 {
		return
	}
	if !e.seen[name] {
		e.seen[name] = true
		e.b.WriteString("# HELP " + name + " " + escHelp.Replace(help) + "\n# TYPE " + name + " " + typ + "\n")
	}
	for _, s := range samples {
		e.b.WriteString(name)
		if len(s.labels) > 0 {
			parts := make([]string, len(s.labels))
			for i, l := range s.labels {
				parts[i] = l.k + `="` + escLabel.Replace(labelString(l.v)) + `"`
			}
			e.b.WriteString("{" + strings.Join(parts, ",") + "}")
		}
		v := 0.0
		switch x := s.v.(type) {
		case float64:
			v = x
		case int:
			v = float64(x)
		case int64:
			v = float64(x)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			v = 0
		}
		e.b.WriteString(" " + jsjson.Number(v) + "\n")
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// scrapeServer — один сервер за скрейп: сам сервер не отвечает — остальные не страдают.
func (m *Metrics) scrapeServer(s serverInfo) any {
	if s.Pending || !m.a.nodes.IsOnline(s.ID) {
		return nil
	}
	ch := make(chan any, 1)
	go func() {
		r, err := m.a.nodes.CtrlFor(s.ID)("GET", "/api/metrics", nil)
		if err != nil || r.Status != 200 {
			ch <- nil
			return
		}
		ch <- r.Data
	}()
	select {
	case d := <-ch:
		return d
	case <-time.After(scrapeDeadline):
		return nil
	}
}

func (m *Metrics) render() string {
	t0 := time.Now()
	list := m.a.nodes.List()
	data := make([]any, len(list))
	var wg sync.WaitGroup
	for i, s := range list {
		wg.Add(1)
		go func() { defer wg.Done(); data[i] = m.scrapeServer(s) }()
	}
	wg.Wait()
	out := &exposition{seen: map[string]bool{}}
	L := func(s serverInfo, extra ...label) []label {
		return append([]label{{"server", s.Name}, {"server_id", s.ID}}, extra...)
	}
	// Серии только тех серверов, чьи метрики получены.
	per := func(fn func(s serverInfo, d any) []sample) []sample {
		var res []sample
		for i, s := range list {
			if truthy(data[i]) {
				res = append(res, fn(s, data[i])...)
			}
		}
		return res
	}
	one := func(fn func(d any) any) func(serverInfo, any) []sample {
		return func(s serverInfo, d any) []sample { return []sample{{L(s), fn(d)}} }
	}
	or0 := func(v any) any {
		if v == nil {
			return 0.0
		}
		return v
	}

	out.add("awg_panel_info", "gauge", "Версия панели Forgetting.", []sample{{[]label{{"version", m.a.version}}, 1.0}})
	out.add("awg_servers", "gauge", "Серверов в панели (ядро и ноды).", []sample{{nil, float64(len(list))}})
	var online, scrapeOK, info []sample
	for i, s := range list {
		online = append(online, sample{L(s), b2f(s.Online)})
		scrapeOK = append(scrapeOK, sample{L(s), b2f(truthy(data[i]))})
		if !s.Pending {
			gen, module, tools := any(""), any(""), any("")
			if s.Health != nil {
				gen, module, tools = s.Health.Gen, s.Health.Module, s.Health.Tools
			}
			ver := s.Version
			if ver == nil {
				ver = ""
			}
			info = append(info, sample{L(s, label{"version", ver}, label{"gen", gen}, label{"module", module}, label{"tools", tools}), 1.0})
		}
	}
	out.add("awg_server_online", "gauge", "Сервер на связи с панелью (1/0).", online)
	out.add("awg_scrape_ok", "gauge", "Метрики сервера получены в этом скрейпе (1/0).", scrapeOK)
	out.add("awg_node_info", "gauge", "Версия Forgetting и поколение AWG на сервере.", info)

	out.add("awg_up", "gauge", "Интерфейс AWG поднят (1/0).", per(one(func(d any) any { return b2f(truthy(field(d, "up"))) })))
	out.add("awg_peers", "gauge", "Пиров в интерфейсе.", per(one(func(d any) any { return or0(field(d, "peers", "total")) })))
	out.add("awg_peers_online", "gauge", "Пиров с рукопожатием за последние 3 минуты.", per(one(func(d any) any { return or0(field(d, "peers", "online")) })))
	// Сумма по пирам — gauge, а не counter: при удалении пира сумма уменьшается.
	out.add("awg_peers_received_bytes", "gauge", "Принято от клиентов (их отдача), байт, сумма по пирам. Уменьшается при удалении пира.", per(one(func(d any) any { return or0(field(d, "rx_bytes")) })))
	out.add("awg_peers_sent_bytes", "gauge", "Отправлено клиентам (их загрузка), байт, сумма по пирам. Уменьшается при удалении пира.", per(one(func(d any) any { return or0(field(d, "tx_bytes")) })))
	for _, x := range [][3]string{
		{"rx_bytes", "receive_bytes", "байт"}, {"tx_bytes", "transmit_bytes", "байт"},
		{"rx_packets", "receive_packets", "пакетов"}, {"tx_packets", "transmit_packets", "пакетов"},
		{"rx_dropped", "receive_dropped", "потерянных пакетов"}, {"tx_dropped", "transmit_dropped", "потерянных пакетов"},
		{"rx_errors", "receive_errors", "ошибок"}, {"tx_errors", "transmit_errors", "ошибок"},
	} {
		out.add("awg_link_"+x[1]+"_total", "counter", "Интерфейс awg1: "+x[2]+" ("+x[0]+").", per(one(func(d any) any { return or0(field(d, "link", x[0])) })))
	}

	// Нагрузка хоста. CPU — счётчики секунд по режимам: доля = rate() в Prometheus.
	out.add("awg_host_cpu_seconds_total", "counter", "Время процессора по режимам, секунд (как node_exporter).",
		per(func(s serverInfo, d any) []sample {
			cpu, _ := field(d, "host", "cpu").(jsjson.Object)
			var res []sample
			for _, kv := range cpu {
				res = append(res, sample{L(s, label{"mode", kv.Key}), jsToNumberAny(kv.Val)})
			}
			return res
		}))
	hostOne := func(fn func(h any) any) func(serverInfo, any) []sample {
		return func(s serverInfo, d any) []sample {
			h := field(d, "host")
			if !truthy(h) {
				return nil
			}
			return []sample{{L(s), fn(h)}}
		}
	}
	out.add("awg_host_cpu_cores", "gauge", "Ядер процессора.", per(hostOne(func(h any) any { return or0(field(h, "cores")) })))
	out.add("awg_host_memory_total_bytes", "gauge", "Оперативной памяти всего, байт.", per(hostOne(func(h any) any { return or0(field(h, "mem", "total")) })))
	out.add("awg_host_memory_available_bytes", "gauge", "Доступной памяти (MemAvailable), байт.", per(hostOne(func(h any) any { return or0(field(h, "mem", "available")) })))
	out.add("awg_host_load", "gauge", "Средняя загрузка системы (load average).", per(func(s serverInfo, d any) []sample {
		h := field(d, "host")
		if !truthy(h) {
			return nil
		}
		load := arr(field(h, "load"))
		var res []sample
		for i, p := range []string{"1m", "5m", "15m"} {
			var v any = 0.0
			if i < len(load) && load[i] != nil {
				v = load[i]
			}
			res = append(res, sample{L(s, label{"period", p}), v})
		}
		return res
	}))

	// Деление канала (CAKE)
	sh := func(fn func(x any) any) []sample {
		return per(func(s serverInfo, d any) []sample {
			x := field(d, "shaping")
			if !truthy(x) {
				return nil
			}
			return []sample{{L(s), fn(x)}}
		})
	}
	out.add("awg_shaping_enabled", "gauge", "Деление канала включено в настройках (1/0).", sh(func(x any) any { return b2f(truthy(field(x, "settings", "enabled"))) }))
	out.add("awg_shaping_active", "gauge", "CAKE реально стоит на интерфейсе (1/0).", sh(func(x any) any { return b2f(truthy(field(x, "active"))) }))
	out.add("awg_shaping_congested", "gauge", "Канал считается перегруженным (1/0).", sh(func(x any) any { return b2f(truthy(field(x, "congested"))) }))
	out.add("awg_shaping_heavy_devices", "gauge", "Устройств в фоновом классе.", sh(func(x any) any {
		if h, ok := field(x, "heavy").([]any); ok {
			return float64(len(h))
		}
		return 0.0
	}))
	out.add("awg_shaping_active_devices", "gauge", "Активных устройств в окне наблюдения.", sh(func(x any) any { return or0(field(x, "devices")) }))
	// dirs — значение nil (undefined) серию не даёт.
	dirs := func(fn func(x any, dir string) any) []sample {
		return per(func(s serverInfo, d any) []sample {
			x := field(d, "shaping")
			if !truthy(x) {
				return nil
			}
			var res []sample
			for _, dir := range []string{"down", "up"} {
				if v := fn(x, dir); v != nil {
					res = append(res, sample{L(s, label{"direction", dir}), v})
				}
			}
			return res
		})
	}
	mbit := func(v any) any {
		if v == nil {
			return math.NaN() // undefined * 1e6
		}
		return jsToNumberAny(v) * 1e6
	}
	out.add("awg_shaping_bandwidth_bits", "gauge", "Заданная ширина канала, бит/с.", dirs(func(x any, dir string) any {
		if dir == "down" {
			return mbit(field(x, "settings", "downMbit"))
		}
		return mbit(field(x, "settings", "upMbit"))
	}))
	out.add("awg_shaping_rate_bits", "gauge", "Скорость за последний тик сэмплера, бит/с.", dirs(func(x any, dir string) any {
		if dir == "down" {
			return mbit(field(x, "rateDown"))
		}
		return mbit(field(x, "rateUp"))
	}))
	out.add("awg_shaping_qdisc_drops_total", "counter", "Пакетов, отброшенных CAKE.", dirs(func(x any, dir string) any { return field(x, dir, "drops") }))
	out.add("awg_shaping_qdisc_backlog_bytes", "gauge", "Байт в очереди CAKE.", dirs(func(x any, dir string) any { return field(x, dir, "backlog") }))

	// Блокировка BitTorrent
	tb := func(fn func(x any) any) []sample {
		return per(func(s serverInfo, d any) []sample {
			x := field(d, "torrent")
			if !truthy(x) {
				return nil
			}
			return []sample{{L(s), fn(x)}}
		})
	}
	out.add("awg_torrent_block_enabled", "gauge", "Блокировка BitTorrent включена в настройках (1/0).", tb(func(x any) any { return b2f(truthy(field(x, "settings", "enabled"))) }))
	out.add("awg_torrent_block_active", "gauge", "nft-таблица блокировки реально стоит (1/0).", tb(func(x any) any { return b2f(truthy(field(x, "active"))) }))
	out.add("awg_torrent_blocked_total", "counter", "Заблокированных попыток торрент-соединений с последнего применения правил.", tb(func(x any) any { return or0(field(x, "hits")) }))
	out.add("awg_torrent_devices", "gauge", "Устройств, пытавшихся качать торренты за последние 10 минут.", tb(func(x any) any { return float64(len(arr(field(x, "seen")))) }))

	if m.perPeer {
		peers := func(fn func(p any) any) []sample {
			return per(func(s serverInfo, d any) []sample {
				var res []sample
				for _, p := range arr(field(d, "list")) {
					name := field(p, "name")
					if name == nil {
						name = ""
					}
					res = append(res, sample{L(s, label{"peer", field(p, "ip")}, label{"name", name}, label{"kind", field(p, "kind")}), fn(p)})
				}
				return res
			})
		}
		out.add("awg_peer_online", "gauge", "Пир на связи (1/0).", peers(func(p any) any { return b2f(truthy(field(p, "online"))) }))
		out.add("awg_peer_background", "gauge", "Пир в фоновом классе: канал занят, а он давно качает больше своей доли (1/0).", peers(func(p any) any { return b2f(field(p, "priority") == "background") }))
		out.add("awg_peer_torrent", "gauge", "Пир пытался открыть торрент-соединение за последние 10 минут (1/0).", peers(func(p any) any { return b2f(truthy(field(p, "torrent"))) }))
		out.add("awg_peer_received_bytes_total", "counter", "Принято от пира, байт.", peers(func(p any) any { return field(p, "rx") }))
		out.add("awg_peer_sent_bytes_total", "counter", "Отправлено пиру, байт.", peers(func(p any) any { return field(p, "tx") }))
	}

	var masters, devices int64
	m.a.db.QueryRow("SELECT COUNT(*) AS n FROM master_keys WHERE revoked_at IS NULL").Scan(&masters)
	m.a.db.QueryRow("SELECT COUNT(*) AS n FROM devices").Scan(&devices)
	out.add("awg_master_keys", "gauge", "Мастер-ключей sen:// (без удаляемых).", []sample{{nil, masters}})
	out.add("awg_devices", "gauge", "Устройств sen://.", []sample{{nil, devices}})
	out.add("awg_scrape_duration_seconds", "gauge", "Сколько панель собирала метрики.", []sample{{nil, time.Since(t0).Seconds()}})
	return out.b.String()
}

func (m *Metrics) handler(c *Ctx) bool {
	return writeText(c, 200, "text/plain; version=0.0.4; charset=utf-8", m.render())
}
