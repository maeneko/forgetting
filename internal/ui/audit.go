package ui

// Аудит серверов (порт части awg-ui/sub.ts). Периодически core обходит все серверы и ищет
// «мёртвых» пиров: живы на сервере, хотя панель про них не знает. Сообщает и — если включена
// очистка или оператор нажал кнопку — снимает подтверждённых: мёртвых два прохода подряд.
//   ghosts  — есть в ядре (awg show), но неизвестны awg-ctrl;
//   orphans — есть у awg-ctrl в области этой панели, но панели не нужны (sync их не снял);
//             key — мастер-ключ пира, если он отозван или удалён.
// Пиры чужой области в отчёт не попадают — считаются в foreign.

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type auditPeer struct {
	PubKey        string
	Owner         any // только у orphans
	Key           any // только у orphans
	Online        any
	LastHandshake any
	SeenBefore    *bool
	orphan        bool
}

func (p *auditPeer) json() jsjson.Object {
	o := jsjson.Object{{Key: "pub_key", Val: p.PubKey}}
	if p.orphan {
		o = append(o, jsjson.KV{Key: "owner", Val: p.Owner})
	}
	o = append(o, jsjson.KV{Key: "online", Val: p.Online}, jsjson.KV{Key: "lastHandshake", Val: p.LastHandshake})
	if p.orphan {
		o = append(o, jsjson.KV{Key: "key", Val: p.Key})
	}
	if p.SeenBefore != nil {
		o = append(o, jsjson.KV{Key: "seen_before", Val: *p.SeenBefore})
	}
	return o
}

type auditEntry struct {
	ServerID      int
	Status        string // ok | offline | error
	GhostsChecked bool   // false — awg-ctrl старше /api/audit, «призраков» не видно
	Ghosts        []*auditPeer
	Orphans       []*auditPeer
	Foreign       int
	Blocked       *int // очистка отказалась снимать столько пиров разом (предохранитель)
}

func peersJSON(ps []*auditPeer) []any {
	out := []any{}
	for _, p := range ps {
		out = append(out, p.json())
	}
	return out
}

func (e *auditEntry) json() jsjson.Object {
	o := jsjson.Object{{Key: "server_id", Val: e.ServerID}, {Key: "status", Val: e.Status}, {Key: "ghosts_checked", Val: e.GhostsChecked},
		{Key: "ghosts", Val: peersJSON(e.Ghosts)}, {Key: "orphans", Val: peersJSON(e.Orphans)}, {Key: "foreign", Val: e.Foreign}}
	if e.Blocked != nil {
		o = append(o, jsjson.KV{Key: "blocked", Val: *e.Blocked})
	}
	return o
}

type auditReport struct {
	At      int64
	Servers []*auditEntry
}

func (r *auditReport) json() any {
	if r == nil {
		return nil
	}
	list := []any{}
	for _, e := range r.Servers {
		list = append(list, e.json())
	}
	return jsjson.Object{{Key: "at", Val: r.At}, {Key: "servers", Val: list}}
}

type cleanedPeer struct {
	At       int64
	ServerID int
	PubKey   string
	Kind     string // ghost | orphan
	Key      any    // у ghost нет (undefined)
	HasKey   bool
	Auto     bool
}

func (c cleanedPeer) json() jsjson.Object {
	o := jsjson.Object{{Key: "at", Val: c.At}, {Key: "server_id", Val: c.ServerID}, {Key: "pub_key", Val: c.PubKey}, {Key: "kind", Val: c.Kind}}
	if c.HasKey {
		o = append(o, jsjson.KV{Key: "key", Val: c.Key})
	}
	return append(o, jsjson.KV{Key: "auto", Val: c.Auto})
}

// auditSettings — clean: снимать «мёртвых» сами; every_h — период; max — предохранитель.
type auditSettings struct {
	Clean  bool
	EveryH int
	Max    int
}

func (s auditSettings) json() jsjson.Object {
	return jsjson.Object{{Key: "clean", Val: s.Clean}, {Key: "every_h", Val: s.EveryH}, {Key: "max", Val: s.Max}}
}

// ── sub_meta: JSON-значения ─────────────────────────────────────────────────

func (s *Sub) metaGet(key string) any {
	var raw string
	if s.a.db.QueryRow("SELECT value FROM sub_meta WHERE key = ?", key).Scan(&raw) != nil {
		return nil
	}
	v, err := jsjson.Decode([]byte(raw))
	if err != nil {
		return nil
	}
	return v
}

func (s *Sub) metaSet(key string, v any) {
	s.a.db.Exec("INSERT INTO sub_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, string(jsjson.MustMarshal(v)))
}

func parsePeer(v any, orphan bool) *auditPeer {
	o, _ := v.(jsjson.Object)
	p := &auditPeer{PubKey: strOr(o.Get2("pub_key"), ""), Online: o.Get2("online"), LastHandshake: o.Get2("lastHandshake"), orphan: orphan}
	if orphan {
		p.Owner, p.Key = o.Get2("owner"), o.Get2("key")
	}
	if b, ok := o.Get2("seen_before").(bool); ok {
		p.SeenBefore = &b
	}
	return p
}

func parseReport(v any) *auditReport {
	o, ok := v.(jsjson.Object)
	if !ok {
		return nil
	}
	r := &auditReport{At: int64(numOr(o.Get2("at"), 0))}
	for _, x := range arr(o.Get2("servers")) {
		e := &auditEntry{ServerID: int(numOr(field(x, "server_id"), 0)), Status: strOr(field(x, "status"), "error"),
			Foreign: int(numOr(field(x, "foreign"), 0))}
		e.GhostsChecked, _ = field(x, "ghosts_checked").(bool)
		for _, p := range arr(field(x, "ghosts")) {
			e.Ghosts = append(e.Ghosts, parsePeer(p, false))
		}
		for _, p := range arr(field(x, "orphans")) {
			e.Orphans = append(e.Orphans, parsePeer(p, true))
		}
		if b, ok := field(x, "blocked").(float64); ok {
			n := int(b)
			e.Blocked = &n
		}
		r.Servers = append(r.Servers, e)
	}
	return r
}

func (s *Sub) loadAudit() {
	s.lastAudit = parseReport(s.metaGet("audit"))
	s.auditSettings = auditSettings{Clean: false, EveryH: 1, Max: 10}
	if o, ok := s.metaGet("audit_settings").(jsjson.Object); ok {
		if b, ok := o.Get2("clean").(bool); ok {
			s.auditSettings.Clean = b
		}
		if f, ok := o.Get2("every_h").(float64); ok {
			s.auditSettings.EveryH = int(f)
		}
		if f, ok := o.Get2("max").(float64); ok {
			s.auditSettings.Max = int(f)
		}
	}
	for _, x := range arr(s.metaGet("audit_cleaned")) {
		o, _ := x.(jsjson.Object)
		k, hasKey := o.Get("key")
		auto, _ := o.Get2("auto").(bool)
		s.cleanedLog = append(s.cleanedLog, cleanedPeer{At: int64(numOr(o.Get2("at"), 0)), ServerID: int(numOr(o.Get2("server_id"), 0)),
			PubKey: strOr(o.Get2("pub_key"), ""), Kind: strOr(o.Get2("kind"), ""), Key: k, HasKey: hasKey, Auto: auto})
	}
}

func (s *Sub) cleanedJSON() []any {
	out := []any{}
	for _, c := range s.cleanedLog {
		out = append(out, c.json())
	}
	return out
}

// ── Проход ──────────────────────────────────────────────────────────────────

var reOwnerMaster = regexp.MustCompile(`^[^/]+/m(\d+)/`)

func (s *Sub) auditNow(id int) (*auditEntry, error) {
	e := &auditEntry{ServerID: id, Status: "ok", GhostsChecked: true}
	if !s.a.nodes.Exists(id) || !s.a.nodes.IsOnline(id) {
		e.Status, e.GhostsChecked = "offline", false
		return e, nil
	}
	ctrl := s.a.nodes.CtrlFor(id)
	// Сначала обычное сведение: отчёт — про то, что не снялось само.
	if err := s.reconcileNow(id); err != nil {
		return nil, err
	}
	a, err := ctrl("GET", "/api/audit", nil)
	if err != nil {
		return nil, err
	}
	p, err := ctrl("GET", "/api/peers", nil)
	if err != nil {
		return nil, err
	}
	fail := func() (*auditEntry, error) {
		e.Status, e.GhostsChecked = "error", false
		e.Ghosts, e.Orphans = nil, nil
		return e, nil
	}
	if !ok2xx(p.Status) {
		return fail()
	}
	if ok2xx(a.Status) {
		for _, g := range arr(field(a.Data, "ghosts")) {
			e.Ghosts = append(e.Ghosts, &auditPeer{PubKey: strOr(field(g, "pub_key"), ""), Online: field(g, "online"), LastHandshake: field(g, "lastHandshake")})
		}
	} else if a.Status == 404 {
		e.GhostsChecked = false
	} else {
		return fail()
	}

	keep := map[string]bool{}
	for _, k := range s.keepOf(id) {
		keep[k] = true
	}
	prefix := s.panelID + "/"
	for _, peer := range arr(field(p.Data, "peers")) {
		owner, ok := field(peer, "owner").(string)
		if !ok {
			return nil, errTypeError // peer.owner.startsWith бросил бы TypeError
		}
		pub := strOr(field(peer, "pub_key"), "")
		if !strings.HasPrefix(owner, prefix) {
			if strings.Contains(owner, "/") {
				e.Foreign++
			}
			continue
		}
		if keep[pub] {
			continue
		}
		key := any("(нет в ui.db)")
		if m := reOwnerMaster.FindStringSubmatch(owner); m != nil {
			if mid, err := strconv.ParseInt(m[1], 10, 64); err == nil && mid <= 9007199254740991 {
				if row := s.masterRowByID(s.a.db, mid); row != nil {
					if row.RevokedAt.Valid {
						key = row.Label + " (удалён)"
					} else {
						key = row.Label
					}
				}
			}
		}
		e.Orphans = append(e.Orphans, &auditPeer{PubKey: pub, Owner: owner, Online: field(peer, "online"),
			LastHandshake: field(peer, "lastHandshake"), Key: key, orphan: true})
	}
	return e, nil
}

type jsTypeError struct{}

func (jsTypeError) Error() string { return "TypeError" }

var errTypeError = jsTypeError{}

// audit — под той же блокировкой, что и сведение: пир, которого прямо сейчас добавляет проход, не «призрак».
func (s *Sub) audit(id int) *auditEntry {
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	e, err := func() (e *auditEntry, err error) {
		defer func() {
			if recover() != nil {
				e, err = nil, errTypeError
			}
		}()
		return s.auditNow(id)
	}()
	if err != nil || e == nil {
		return &auditEntry{ServerID: id, Status: "error"}
	}
	return e
}

// cleanEntry — снять «мёртвых» сервера: только подтверждённых (seen_before), не больше max за проход.
// «Призраки» — через awg-ctrl (он сам перепроверяет ключи), лишние — обычным DELETE.
func (s *Sub) cleanEntry(e *auditEntry, auto bool) error {
	seen := func(ps []*auditPeer) []*auditPeer {
		var out []*auditPeer
		for _, p := range ps {
			if p.SeenBefore != nil && *p.SeenBefore {
				out = append(out, p)
			}
		}
		return out
	}
	ghosts, orphans := seen(e.Ghosts), seen(e.Orphans)
	if len(ghosts)+len(orphans) == 0 || e.Status != "ok" {
		return nil
	}
	s.auditMu.Lock()
	max := s.auditSettings.Max
	s.auditMu.Unlock()
	if auto && len(ghosts)+len(orphans) > max {
		n := len(ghosts) + len(orphans)
		e.Blocked = &n
		warnf("sub: очистка сервера #%d пропущена: %d пиров больше предела %d", e.ServerID, n, max)
		return nil
	}
	ctrl := s.a.nodes.CtrlFor(e.ServerID)
	var done []cleanedPeer
	log := func(p *auditPeer, kind string) {
		done = append(done, cleanedPeer{At: now(), ServerID: e.ServerID, PubKey: p.PubKey, Kind: kind, Key: p.Key, HasKey: p.orphan, Auto: auto})
	}
	if len(ghosts) > 0 {
		keys := []any{}
		for _, g := range ghosts {
			keys = append(keys, g.PubKey)
		}
		r, err := ctrl("POST", "/api/audit/clean", jsjson.Object{{Key: "pub_keys", Val: keys}})
		if err != nil {
			return err
		}
		if ok2xx(r.Status) {
			removed := map[string]bool{}
			for _, x := range arr(field(r.Data, "removed")) {
				if k, ok := x.(string); ok {
					removed[k] = true
				}
			}
			for _, g := range ghosts {
				if removed[g.PubKey] {
					log(g, "ghost")
				}
			}
		}
	}
	for _, p := range orphans {
		r, err := ctrl("DELETE", "/api/peers/"+pubParam(p.PubKey), nil)
		if err != nil {
			return err
		}
		if ok2xx(r.Status) || r.Status == 404 {
			log(p, "orphan")
		} else if gone(r.Status) {
			break
		}
	}
	if len(done) == 0 {
		return nil
	}
	cleaned := map[string]bool{}
	var short []string
	for _, d := range done {
		cleaned[d.PubKey] = true
		k := d.PubKey
		if len(k) > 8 {
			k = k[:8]
		}
		short = append(short, k)
	}
	drop := func(ps []*auditPeer) []*auditPeer {
		var out []*auditPeer
		for _, p := range ps {
			if !cleaned[p.PubKey] {
				out = append(out, p)
			}
		}
		return out
	}
	e.Ghosts, e.Orphans = drop(e.Ghosts), drop(e.Orphans)
	s.auditMu.Lock()
	s.cleanedLog = append(done, s.cleanedLog...)
	if len(s.cleanedLog) > 100 {
		s.cleanedLog = s.cleanedLog[:100]
	}
	s.metaSet("audit_cleaned", s.cleanedJSON())
	s.auditMu.Unlock()
	what := "очистка"
	if auto {
		what = "автоочистка"
	}
	warnf("sub: %s сервера #%d: снято %d (%s)", what, e.ServerID, len(done), strings.Join(short, ", "))
	return nil
}

// cleanLocked — очистка под блокировкой сервера, как и сам аудит.
func (s *Sub) cleanLocked(e *auditEntry, auto bool) {
	l := s.lock(e.ServerID)
	l.Lock()
	defer l.Unlock()
	if err := s.cleanEntry(e, auto); err != nil && auto {
		warnf("sub: очистка сервера #%d не удалась: %v", e.ServerID, err)
	}
}

// auditAll — проход по всем серверам; одновременно идёт не больше одного (повторный вызов ждёт
// текущий). auto — плановый: очищает сам, если включено. Ручной только смотрит.
func (s *Sub) auditAll(auto bool) {
	s.auditMu.Lock()
	if ch := s.auditing; ch != nil {
		s.auditMu.Unlock()
		<-ch
		return
	}
	ch := make(chan struct{})
	s.auditing = ch
	prev := s.lastAudit
	s.auditMu.Unlock()
	defer func() {
		s.auditMu.Lock()
		s.auditing = nil
		s.auditMu.Unlock()
		close(ch)
	}()

	ids := s.a.nodes.IDs()
	report := &auditReport{At: now(), Servers: make([]*auditEntry, len(ids))}
	done := make(chan struct{}, len(ids))
	for i, id := range ids {
		go func() { report.Servers[i] = s.audit(id); done <- struct{}{} }()
	}
	for range ids {
		<-done
	}
	s.auditMu.Lock()
	clean := s.auditSettings.Clean
	s.auditMu.Unlock()
	for _, e := range report.Servers {
		was := map[string]bool{}
		if prev != nil {
			for _, x := range prev.Servers {
				if x.ServerID == e.ServerID {
					for _, p := range append(append([]*auditPeer{}, x.Ghosts...), x.Orphans...) {
						was[p.PubKey] = true
					}
					break
				}
			}
		}
		for _, p := range append(append([]*auditPeer{}, e.Ghosts...), e.Orphans...) {
			b := was[p.PubKey]
			p.SeenBefore = &b
		}
		if auto && clean {
			s.cleanLocked(e, true)
		}
	}
	s.auditMu.Lock()
	s.lastAudit = report
	s.metaSet("audit", report.json())
	s.auditMu.Unlock()
	for _, e := range report.Servers {
		if e.Status == "error" {
			warnf("sub: аудит сервера #%d не удался", e.ServerID)
		}
		if n := len(e.Ghosts) + len(e.Orphans); n > 0 {
			online := 0
			for _, p := range append(append([]*auditPeer{}, e.Ghosts...), e.Orphans...) {
				if truthy(p.Online) {
					online++
				}
			}
			warnf("sub: аудит сервера #%d: мёртвых пиров %d (в ядре без записи: %d, лишних у awg-ctrl: %d, онлайн: %d)",
				e.ServerID, n, len(e.Ghosts), len(e.Orphans), online)
		}
	}
}

// cleanNow — ручная очистка подтверждённых «мёртвых» по последнему отчёту.
func (s *Sub) cleanNow() {
	s.auditMu.Lock()
	rep := s.lastAudit
	s.auditMu.Unlock()
	if rep == nil {
		return
	}
	for _, e := range rep.Servers {
		s.cleanLocked(e, false)
	}
	s.auditMu.Lock()
	s.metaSet("audit", rep.json())
	s.auditMu.Unlock()
}

func (s *Sub) auditState() jsjson.Object {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return jsjson.Object{{Key: "enabled", Val: s.auditOn}, {Key: "settings", Val: s.auditSettings.json()},
		{Key: "report", Val: s.lastAudit.json()}, {Key: "cleaned", Val: s.cleanedJSON()}}
}
