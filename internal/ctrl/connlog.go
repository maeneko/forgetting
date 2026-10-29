package ctrl

// Журнал VPN-подключений: кто, когда подключился и отключился, сколько длилась сессия и сколько
// прошло трафика, и когда каждый пир был на связи последний раз (для «кто давно не подключался»).
//
// У WireGuard нет событий подключения — только время последнего handshake и счётчики байт в
// `awg show dump`. Раз в 30 с сверяем их с открытыми сессиями:
//   - пир стал онлайн (handshake моложе 3 мин) — «подключился» со временем этого handshake;
//   - пир ушёл из онлайна или с интерфейса — «отключился» со временем последней активности
//     (последнего тика, когда менялись счётчики): handshake идёт раз в 2 мин, активность точнее.
// Открытые сессии восстанавливаются из базы, поэтому перезапуск awg-ctrl не плодит дублей.
// Внешний IP клиента не пишем: это уже слежка, а не журнал.

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	connlogEvery = 30 * time.Second
	connlogKeepS = 90 * 24 * 3600 // столько дней храним события
	seenEveryS   = 60             // last_seen пишем не чаще раза в минуту на пира
	connlogLimit = 500
)

type connSession struct {
	since          int64
	rx0, tx0       float64 // счётчики на начало сессии
	lastRx, lastTx float64
	active         int64 // последний тик, когда менялись счётчики
	seenSaved      int64
}

type peerIdent struct{ name, owner, ip string }

type ConnLog struct {
	s       *Server
	mu      sync.Mutex
	open    map[string]*connSession
	loaded  bool
	pruned  int64
	peersFn func() (map[string]peerStat, bool) // подменяется в тестах
}

func newConnLog(s *Server) *ConnLog {
	l := &ConnLog{s: s, open: map[string]*connSession{}}
	l.peersFn = func() (map[string]peerStat, bool) {
		if !s.interfaceStatus().Up {
			return nil, false // интерфейс лежит — это не «все отключились», а пауза
		}
		return s.getPeersData(), true
	}
	return l
}

func (l *ConnLog) Start() {
	go func() {
		for {
			l.tick(time.Now().Unix())
			time.Sleep(connlogEvery)
		}
	}()
}

// idents — имя пользователя или владелец пира sen:// и внутренний IP по публичному ключу. Строки
// читаем до конца до любых других запросов: соединение с SQLite одно (см. metrics_test.go).
func (l *ConnLog) idents() map[string]peerIdent {
	out := map[string]peerIdent{}
	if rows, err := l.s.db.Query("SELECT pub_key, name, ip FROM users"); err == nil {
		for rows.Next() {
			var pub, name, ip string
			if rows.Scan(&pub, &name, &ip) == nil {
				out[pub] = peerIdent{name: name, ip: ip}
			}
		}
		rows.Close()
	}
	if rows, err := l.s.db.Query("SELECT pub_key, owner, ip FROM peers"); err == nil {
		for rows.Next() {
			var pub, owner, ip string
			if rows.Scan(&pub, &owner, &ip) == nil {
				out[pub] = peerIdent{owner: owner, ip: ip}
			}
		}
		rows.Close()
	}
	return out
}

// load — открытые сессии (последнее событие пира — «подключился») после перезапуска.
func (l *ConnLog) load(now int64) {
	type row struct {
		pub        string
		at, rx, tx int64
		seen       int64
	}
	var open []row
	rows, err := l.s.db.Query(`SELECT c.pub_key, c.at, COALESCE(c.rx, 0), COALESCE(c.tx, 0), COALESCE(s.at, c.at)
		FROM conn_log c
		JOIN (SELECT pub_key, MAX(id) AS id FROM conn_log GROUP BY pub_key) last ON last.id = c.id
		LEFT JOIN peer_seen s ON s.pub_key = c.pub_key
		WHERE c.event = 'connect'`)
	if err != nil {
		return
	}
	for rows.Next() {
		var r row
		if rows.Scan(&r.pub, &r.at, &r.rx, &r.tx, &r.seen) == nil {
			open = append(open, r)
		}
	}
	rows.Close()
	for _, r := range open {
		l.open[r.pub] = &connSession{since: r.at, rx0: float64(r.rx), tx0: float64(r.tx),
			lastRx: float64(r.rx), lastTx: float64(r.tx), active: r.seen, seenSaved: r.seen}
	}
	l.loaded = true
}

func (l *ConnLog) tick(now int64) {
	stats, ok := l.peersFn()
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		l.load(now)
	}
	who := l.idents()
	write := func(event, pub string, at int64, dur *int64, rx, tx float64) {
		id := who[pub]
		var d any
		if dur != nil {
			d = *dur
		}
		l.s.db.Exec("INSERT INTO conn_log (at, event, pub_key, name, owner, ip, duration, rx, tx) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			at, event, pub, id.name, id.owner, id.ip, d, int64(rx), int64(tx))
	}
	seen := func(pub string, at int64) {
		l.s.db.Exec("INSERT INTO peer_seen (pub_key, at) VALUES (?, ?) ON CONFLICT(pub_key) DO UPDATE SET at = excluded.at", pub, at)
	}
	closeSession := func(pub string, sess *connSession) {
		at := max(sess.active, sess.since)
		dur := at - sess.since
		// Счётчики сбрасываются при перезапуске интерфейса: тогда считаем от нуля.
		rx, tx := sess.lastRx-sess.rx0, sess.lastTx-sess.tx0
		if rx < 0 {
			rx = sess.lastRx
		}
		if tx < 0 {
			tx = sess.lastTx
		}
		write("disconnect", pub, at, &dur, rx, tx)
		seen(pub, at)
		delete(l.open, pub)
	}

	for pub, st := range stats {
		sess := l.open[pub]
		switch {
		case st.Online && sess == nil:
			at := int64(st.LastHandshake)
			if at <= 0 || at > now {
				at = now
			}
			write("connect", pub, at, nil, st.Rx, st.Tx)
			l.open[pub] = &connSession{since: at, rx0: st.Rx, tx0: st.Tx, lastRx: st.Rx, lastTx: st.Tx, active: now, seenSaved: now}
			seen(pub, now)
		case st.Online:
			if st.Rx != sess.lastRx || st.Tx != sess.lastTx {
				sess.lastRx, sess.lastTx, sess.active = st.Rx, st.Tx, now
			}
			if now-sess.seenSaved >= seenEveryS {
				seen(pub, sess.active)
				sess.seenSaved = now
			}
		case sess != nil:
			if hs := int64(st.LastHandshake); hs > sess.active && hs <= now {
				sess.active = hs
			}
			if st.Rx > sess.lastRx || st.Tx > sess.lastTx {
				sess.lastRx, sess.lastTx = st.Rx, st.Tx
			}
			closeSession(pub, sess)
		}
	}
	// Пира сняли с интерфейса (удалили пользователя, перевыпустили ключ) посреди сессии.
	for pub, sess := range l.open {
		if _, ok := stats[pub]; !ok {
			closeSession(pub, sess)
		}
	}
	if now-l.pruned >= 3600 {
		l.s.db.Exec("DELETE FROM conn_log WHERE at < ?", now-connlogKeepS)
		l.pruned = now
	}
}

// list — GET /api/connlog: события (новые сверху) и «когда был на связи» по каждому пиру.
//
//	?limit=N (до 500), ?before=<id> — следующая страница, ?q=<строка> — по имени, владельцу, IP.
func (l *ConnLog) list(limit, before int64, q string) (ob, error) {
	if limit <= 0 || limit > connlogLimit {
		limit = connlogLimit
	}
	where, args := []string{"1 = 1"}, []any{}
	if before > 0 {
		where, args = append(where, "id < ?"), append(args, before)
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		where = append(where, `(name LIKE ? ESCAPE '\' OR owner LIKE ? ESCAPE '\' OR ip LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	args = append(args, limit)
	rows, err := l.s.db.Query("SELECT id, at, event, pub_key, name, owner, ip, duration, rx, tx FROM conn_log WHERE "+
		strings.Join(where, " AND ")+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	events := []any{}
	for rows.Next() {
		var id, at int64
		var event, pub, name, owner, ip string
		var dur, rx, tx *int64
		if err := rows.Scan(&id, &at, &event, &pub, &name, &owner, &ip, &dur, &rx, &tx); err != nil {
			rows.Close()
			return nil, err
		}
		e := ob{{"id", id}, {"at", at}, {"event", event}, {"pub_key", pub}, {"name", name}, {"owner", owner}, {"ip", ip}}
		if event == "disconnect" {
			e = append(e, kv{"duration", deref(dur)}, kv{"rx", deref(rx)}, kv{"tx", deref(tx)})
		}
		events = append(events, e)
	}
	rows.Close()

	// Все пиры сервера с последним временем на связи (null — с тех пор, как ведётся журнал, не было).
	stats := map[string]peerStat{}
	if st, ok := l.peersFn(); ok {
		stats = st
	}
	peers := []any{}
	rows, err = l.s.db.Query(`SELECT u.pub_key, u.name, '' AS owner, u.ip, s.at FROM users u LEFT JOIN peer_seen s ON s.pub_key = u.pub_key
		UNION ALL
		SELECT p.pub_key, '', p.owner, p.ip, s.at FROM peers p LEFT JOIN peer_seen s ON s.pub_key = p.pub_key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pub, name, owner, ip string
		var at *int64
		if err := rows.Scan(&pub, &name, &owner, &ip, &at); err != nil {
			rows.Close()
			return nil, err
		}
		var last any
		if at != nil {
			last = *at
		}
		peers = append(peers, ob{{"pub_key", pub}, {"name", name}, {"owner", owner}, {"ip", ip},
			{"online", stats[pub].Online}, {"last_seen", last}})
	}
	rows.Close()

	var since any
	var first int64
	if l.s.db.QueryRow("SELECT MIN(at) FROM conn_log").Scan(&first) == nil && first > 0 {
		since = first
	}
	return ob{{"events", events}, {"peers", peers}, {"since", since}, {"keep_days", connlogKeepS / 86400}}, nil
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func queryInt(c *rctx, k string) int64 {
	n, _ := strconv.ParseInt(c.r.URL.Query().Get(k), 10, 64)
	return n
}
