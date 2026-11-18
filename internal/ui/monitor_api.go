package ui

// Публичный монитор: GET /api/v1/monitor (ключ awgk_). Один снимок на запрос — окно,
// серверы, реестр и серии — из тех же примитивов, что вкладка «Монитор» (monitor.go):
// второго слоя Prometheus и второго списка запросов нет. В отличие от вкладки запрос,
// упавший наполовину, не 502-ит: он отдаёт остальное с degraded:true.
//
// Приватность: наружу уходят только счётчики, байты, проценты, id и имена серверов
// (их выбрал оператор), флаги и названия бакетов ОС. Меток ключей, uuid, device_*,
// пиров, health.ip, PROM_URL и сырого platform здесь нет и быть не может.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	// monitorCallTimeout — бюджет каждого запроса к Prometheus у публичного API. Все
	// запросы идут параллельно, поэтому сборка укладывается в него же. У вкладки — 8 с/15 с.
	monitorCallTimeout = 6 * time.Second
	// Кэш снимка: страница монитора обновляется раз в минуту, а сборка стоит 18 запросов.
	monitorCacheTTL = 60 * time.Second
	monitorCacheMax = 32
)

// monCall — один снимок в кэше (single-flight, как peersOf в sub_admin.go).
type monCall struct {
	done chan struct{} // закрывается, когда снимок собран
	at   time.Time     // нулевое — ошибка, в кэш не годится
	doc  jsjson.Object
	err  error
}

// apiMonitor — GET /api/v1/monitor?range=1h|6h|24h|7d|30d.
func (m *Monitor) apiMonitor(c *Ctx) bool {
	return aw(func(c *Ctx) (int, any, error) {
		key := ""
		if c.apiKey != nil {
			key = strconv.FormatInt(c.apiKey.ID, 10)
		}
		if !m.apiLimit.allow(key) {
			c.W.Header().Set("Retry-After", "60")
			return 0, nil, fail(429, "Too many requests")
		}
		qs := c.R.URL.Query()
		rangeKey := "1h"
		if _, ok := qs["range"]; ok {
			rangeKey = strings.Join(qs["range"], ",")
		}
		if _, ok := promRanges[rangeKey]; !ok {
			return 0, nil, fail(400, "range: 1h, 6h, 24h, 7d или 30d")
		}
		// nil у visible() значит «панель, видно всё»: у ключа без серверов — пустая область.
		keyScope := c.servers
		if keyScope == nil {
			keyScope = []int{}
		}
		scope, msg := parseServerFilter(qs["server"], keyScope)
		if msg != "" {
			return 0, nil, fail(400, msg)
		}
		doc, err := m.monitorDoc(scope, rangeKey)
		if err != nil {
			return 0, nil, err
		}
		return 200, doc, nil
	})(c)
}

// parseServerFilter — ?server= для монитора: один id, повтор, запятые или all.
// Итог — область ключа ∩ запрошенные id, по возрастанию и без повторов. Второе
// значение — текст ошибки 400 (пусто — всё в порядке).
func parseServerFilter(values []string, keyScope []int) ([]int, string) {
	if values == nil { // параметра нет — область ключа как есть
		return keyScope, ""
	}
	inScope := make(map[int]bool, len(keyScope))
	for _, id := range keyScope {
		inScope[id] = true
	}
	seen := map[int]bool{}
	all := false
	var ids []int
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				return nil, "server: укажите id или all"
			}
			if p == "all" {
				all = true
				continue
			}
			id, err := strconv.Atoi(p)
			if err != nil || id <= 0 {
				return nil, "server: укажите id или all"
			}
			if !inScope[id] {
				return nil, "server: нет доступа"
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if all {
		out := append([]int{}, keyScope...)
		sort.Ints(out)
		return out, ""
	}
	sort.Ints(ids)
	if ids == nil {
		ids = []int{}
	}
	return ids, ""
}

// monitorDoc — кэш на 60 с и single-flight по (range, область). Хит отдаёт сохранённый
// документ вместе с его generated_at: потребитель видит, насколько снимок старый.
func (m *Monitor) monitorDoc(scope []int, rangeKey string) (jsjson.Object, error) {
	key := rangeKey + "|" + scopeKey(scope)
	m.statsMu.Lock()
	if c := m.stats[key]; c != nil {
		select {
		case <-c.done:
			if !c.at.IsZero() && time.Since(c.at) < monitorCacheTTL {
				m.statsMu.Unlock()
				return c.doc, c.err
			}
			// истёк — пересоберём ниже (блокировка ещё наша)
		default:
			m.statsMu.Unlock()
			<-c.done
			return c.doc, c.err
		}
	}
	if len(m.stats) > monitorCacheMax {
		m.stats = map[string]*monCall{}
	}
	c := &monCall{done: make(chan struct{})}
	m.stats[key] = c
	m.statsMu.Unlock()

	// Паника внутри сборки не должна оставить ждущих на закрытом канале навсегда:
	// превращаем её в ошибку (aw ответит 502), как и любую другую.
	doc, err := func() (doc jsjson.Object, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("monitor: %v", p)
			}
		}()
		return m.buildMonitor(scope, rangeKey)
	}()
	if err == nil {
		c.at = time.Now()
	}
	c.doc, c.err = doc, err
	close(c.done)
	return doc, err
}

func scopeKey(scope []int) string {
	ids := append([]int{}, scope...)
	sort.Ints(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

func errAt(errs []error, i int) error {
	if i < 0 || i >= len(errs) {
		return nil
	}
	return errs[i]
}

func anyErr(errs []error) bool {
	for _, err := range errs {
		if err != nil {
			return true
		}
	}
	return false
}

// filterSeries — только разрешённые серверы. Зовётся до любого total(): sum по
// неотфильтрованной карте сложил бы серверы, которых ключ не видит.
func filterSeries(s *series, allowed map[int]bool) *series {
	out := &series{m: map[int]*seriesOne{}}
	if s == nil {
		return out
	}
	for _, id := range s.ids {
		if !allowed[id] {
			continue
		}
		out.ids = append(out.ids, id)
		out.m[id] = s.m[id]
	}
	return out
}

func filterVals(v map[int]float64, allowed map[int]bool) map[int]float64 {
	if v == nil {
		return nil
	}
	out := map[int]float64{}
	for id, x := range v {
		if allowed[id] {
			out[id] = x
		}
	}
	return out
}

// buildMonitor — снимок для одной области и одного окна.
func (m *Monitor) buildMonitor(scope []int, rangeKey string) (jsjson.Object, error) {
	rg := promRanges[rangeKey]
	gen := now()
	nodes := m.a.nodes
	allowed := map[int]bool{}
	for _, id := range scope {
		if nodes == nil || nodes.Exists(id) {
			allowed[id] = true
		}
	}

	reg := m.a.registryTotals(scope)
	masters := reg.Get2("master_keys")
	devices := reg.Get2("devices")
	slots := reg.Get2("device_slots")
	platforms := reg.Get2("platforms")

	src := m.source()
	installed := src != nil
	var d *monData
	inst := map[string]map[int]float64{}
	instErr := map[string]error{}
	if installed {
		// Пять мгновенных запросов, которых нет у вкладки, идут одной волной с её веером:
		// 18 запросов параллельно, поэтому сборка укладывается в один бюджет, а не в два.
		nowAt := now()
		queries := map[string]string{
			"peers": `sum by (server_id, server) (awg_peers)`,
			"rx":    `sum by (server_id, server) (awg_peers_received_bytes)`,
			"tx":    `sum by (server_id, server) (awg_peers_sent_bytes)`,
			"up":    `max by (server_id, server) (awg_up)`,
			"bw":    `sum by (server_id, server) (awg_shaping_bandwidth_bits)`,
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for k, q := range queries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, err := m.instantQueryT(src, q, nowAt, monitorCallTimeout)
				mu.Lock()
				inst[k], instErr[k] = filterVals(v, allowed), err
				mu.Unlock()
			}()
		}
		d, _ = m.queryAllT(src, rg, monitorCallTimeout)
		wg.Wait()
	}

	degraded := false
	if installed {
		degraded = anyErr(d.errs)
		for _, err := range instErr {
			if err != nil {
				degraded = true
			}
		}
	}

	// Каждый ряд и каждая мгновенная карта — через фильтр области.
	seriesMap := map[string]*series{}
	nowVals := map[string]map[int]float64{}
	serErr := map[string]error{}
	var usedDown, usedUp map[int]float64
	if installed {
		for i, pq := range promQueries {
			seriesMap[pq.key] = filterSeries(d.series[pq.key], allowed)
			serErr[pq.key] = errAt(d.errs, i)
		}
		for k, v := range d.nowVals {
			nowVals[k] = filterVals(v, allowed)
		}
		usedDown = filterVals(d.usedDown, allowed)
		usedUp = filterVals(d.usedUp, allowed)
	}

	to := gen
	if d != nil {
		to = d.to
	}
	from := to - rg.seconds

	sum := func(v map[int]float64, err error) any {
		if err != nil {
			return nil
		}
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	}
	instVal := func(k string, id int) any {
		if instErr[k] != nil {
			return nil
		}
		if x, ok := inst[k][id]; ok {
			return x
		}
		return nil
	}
	val := func(v map[int]float64, id int) any {
		if v == nil {
			return nil
		}
		if x, ok := v[id]; ok {
			return x
		}
		return nil
	}

	seriesOut := any(nil)
	if installed {
		o := jsjson.Object{}
		for _, x := range []struct {
			name, key string
			max       bool
		}{
			{"down_bps", "down", false}, {"up_bps", "up", false}, {"online_peers", "online", false},
			{"cpu_percent", "cpu", true}, {"mem_percent", "mem", true},
		} {
			if serErr[x.key] != nil {
				o = append(o, jsjson.KV{Key: x.name, Val: nil})
				continue
			}
			o = append(o, jsjson.KV{Key: x.name, Val: total(seriesMap[x.key], x.max)})
		}
		seriesOut = o
	}

	var traffic any
	if installed {
		traffic = jsjson.Object{
			{Key: "window", Val: jsjson.Object{
				// rx — принято от клиентов (их отдача), tx — отправлено клиентам (их загрузка).
				{Key: "rx_bytes", Val: sum(usedUp, errAt(d.errs, len(promQueries)+1))},
				{Key: "tx_bytes", Val: sum(usedDown, errAt(d.errs, len(promQueries)))},
			}},
			{Key: "cumulative", Val: jsjson.Object{
				{Key: "rx_bytes", Val: sum(inst["rx"], instErr["rx"])},
				{Key: "tx_bytes", Val: sum(inst["tx"], instErr["tx"])},
			}},
		}
	}

	peers := sum(inst["peers"], instErr["peers"])
	if !installed {
		peers = nil
	}
	var peersOnline any
	if installed && errAt(d.errs, 2) == nil {
		s := 0.0
		for _, id := range seriesMap["online"].ids {
			if v := last(seriesMap["online"].m[id]); v != nil {
				s += jsToNumberAny(v)
			}
		}
		peersOnline = s
	}

	servers := []any{}
	serversOnline := 0
	anyNotOK := false
	var list []serverInfo
	if nodes != nil {
		list = nodes.List()
	}
	for _, s := range list {
		if !allowed[s.ID] {
			continue
		}
		if s.Online {
			serversOnline++
		}
		ok := false
		if installed {
			if v := last(seriesMap["scrape_ok"].get(s.ID)); v != nil && jsToNumberAny(v) == 1 {
				ok = true
			}
		}
		if !ok {
			anyNotOK = true
		}
		id := s.ID
		cur := func(k string) any {
			if v, has := nowVals[k]; has {
				if x, ok := v[id]; ok {
					return x
				}
				return nil
			}
			return last(seriesMap[k].get(id))
		}
		var congested any
		if x := seriesMap["congested"].get(id); x != nil && len(x.points) > 0 {
			congested = last(x) == 1.0
		}
		servers = append(servers, jsjson.Object{
			{Key: "id", Val: id}, {Key: "name", Val: s.Name},
			{Key: "online", Val: s.Online}, {Key: "pending", Val: s.Pending}, {Key: "outdated", Val: s.Outdated},
			{Key: "version", Val: s.Version}, {Key: "ok", Val: ok}, {Key: "up", Val: instVal("up", id)},
			{Key: "peers", Val: instVal("peers", id)}, {Key: "peers_online", Val: last(seriesMap["online"].get(id))},
			{Key: "rx_bytes", Val: instVal("rx", id)}, {Key: "tx_bytes", Val: instVal("tx", id)},
			{Key: "down_bps", Val: cur("down")}, {Key: "up_bps", Val: cur("up")},
			{Key: "window_rx_bytes", Val: val(usedUp, id)}, {Key: "window_tx_bytes", Val: val(usedDown, id)},
			{Key: "bandwidth_bits", Val: instVal("bw", id)},
			{Key: "cpu_percent", Val: cur("cpu")}, {Key: "mem_percent", Val: last(seriesMap["mem"].get(id))},
			{Key: "cores", Val: last(seriesMap["cores"].get(id))},
			{Key: "mem_total_bytes", Val: last(seriesMap["mem_total"].get(id))},
			{Key: "heavy", Val: last(seriesMap["heavy"].get(id))}, {Key: "congested", Val: congested},
		})
	}

	source := any(nil)
	if src != nil {
		if src.local {
			source = "local"
		} else {
			source = "node"
		}
	}

	return jsjson.Object{
		{Key: "generated_at", Val: to}, {Key: "range", Val: rangeKey}, {Key: "step", Val: rg.step},
		{Key: "from", Val: from}, {Key: "to", Val: to}, {Key: "scope", Val: intsJSON(scope)},
		{Key: "partial", Val: !installed || degraded || anyNotOK},
		{Key: "prometheus", Val: jsjson.Object{{Key: "installed", Val: installed}, {Key: "degraded", Val: degraded},
			{Key: "source", Val: source}}},
		{Key: "totals", Val: jsjson.Object{
			{Key: "master_keys", Val: masters}, {Key: "devices", Val: devices}, {Key: "device_slots", Val: slots},
			{Key: "peers", Val: peers}, {Key: "peers_online", Val: peersOnline},
			{Key: "servers_total", Val: len(servers)}, {Key: "servers_online", Val: serversOnline},
		}},
		{Key: "platforms", Val: platforms},
		{Key: "traffic", Val: traffic},
		{Key: "servers", Val: servers},
		{Key: "series", Val: seriesOut},
	}, nil
}

// registryTotals — счётчики реестра sen:// в пределах области: master_keys, devices,
// device_slots и platforms{total,known,by_os}. Без Prometheus и без RPC.
func (a *App) registryTotals(scope []int) jsjson.Object {
	agg := a.registryStats(scope)
	return jsjson.Object{
		{Key: "master_keys", Val: agg.masters}, {Key: "devices", Val: agg.devices},
		{Key: "device_slots", Val: agg.slots}, {Key: "platforms", Val: platformBlock(agg.rows)},
	}
}

// registryAgg — то же, что registryTotals, но значениями: /metrics не нужно собирать JSON.
type registryAgg struct {
	masters int
	devices int
	slots   int
	rows    []osRow
}

// registryStats — общий счётчик реестра для /api/v1/monitor и /metrics. Ключ считается,
// только если все его серверы входят в область (как visible() в sub_admin.go) и он не
// удаляется; устройство идёт за своим ключом, поэтому чужое существование не утекает.
// scope == nil — вся панель (панельные метрики), пустой срез — ничего.
func (a *App) registryStats(scope []int) registryAgg {
	var agg registryAgg
	allowed := map[int]bool{}
	for _, id := range scope {
		if a.nodes == nil || a.nodes.Exists(id) {
			allowed[id] = true
		}
	}
	inScope := func(id int) bool { return scope == nil || allowed[id] }

	servers := map[int64][]int{}
	if rows, err := a.db.Query("SELECT master_id, server_id FROM master_servers ORDER BY master_id, server_id"); err == nil {
		for rows.Next() {
			var mid int64
			var sid int
			if rows.Scan(&mid, &sid) == nil {
				servers[mid] = append(servers[mid], sid)
			}
		}
		rows.Close()
	}

	visible := map[int64]bool{}
	// Строки читаем в память и закрываем до вызовов nodes.Exists: у ui.db одно соединение
	// (SetMaxOpenConns(1)), и запрос внутри открытого rows заблокировал бы сам себя.
	type masterRec struct {
		id    int64
		limit int
	}
	var mastersList []masterRec
	if rows, err := a.db.Query("SELECT id, device_limit FROM master_keys WHERE revoked_at IS NULL"); err == nil {
		for rows.Next() {
			var m masterRec
			if rows.Scan(&m.id, &m.limit) == nil {
				mastersList = append(mastersList, m)
			}
		}
		rows.Close()
	}
	for _, mr := range mastersList {
		ok := true
		for _, sid := range servers[mr.id] {
			// Мёртвый сервер (удалён из реестра) не может входить в область ключа —
			// не считаем его нарушением, как visible().
			if a.nodes != nil && !a.nodes.Exists(sid) {
				continue
			}
			if !inScope(sid) {
				ok = false
				break
			}
		}
		if ok {
			visible[mr.id] = true
			agg.masters++
			agg.slots += mr.limit
		}
	}

	if rows, err := a.db.Query("SELECT master_id, platform, COUNT(*) FROM devices GROUP BY master_id, platform"); err == nil {
		for rows.Next() {
			var mid int64
			var platform string
			var n int
			if rows.Scan(&mid, &platform, &n) != nil {
				continue
			}
			if !visible[mid] {
				continue
			}
			agg.devices += n
			agg.rows = append(agg.rows, osRow{platform: platform, n: n})
		}
		rows.Close()
	}
	return agg
}
