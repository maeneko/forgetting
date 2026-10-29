package ui

// Вкладка «Монитор» (порт awg-ui/monitor.ts): встроенный Prometheus и основные графики.
//   GET  /ui/monitor            — готов ли Prometheus, можно ли поставить его кнопкой, ход установки
//   POST /ui/monitor/install    — попросить awg-ctrl выполнить `awg-ctrl monitor install` (root)
//   POST /ui/monitor/remove     — то же для `awg-ctrl monitor remove`
//   GET  /ui/monitor/overview   — серии для графиков за 1 ч / 6 ч / 24 ч / 7 дн / 30 дн
// Панель сама пакетов не ставит и в Prometheus ходит только фиксированными запросами.
// Prometheus — на этой машине или на ноде (панель без VPN, например после переноса): тогда и
// запросы к нему, и его скрейп метрик панели идут через агента ноды по её WSS.

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type promRange struct{ seconds, step int64 }

// Шаг — чтобы на графике было ~250–360 точек; окно rate — минимум два скрейпа (15 с).
// Дальше 30 дней не заглядываем: столько Prometheus хранит (promRetention в internal/cli/monitor.go).
var promRanges = map[string]promRange{
	"1h": {3600, 15}, "6h": {21600, 60}, "24h": {86400, 300},
	"7d": {604800, 1800}, "30d": {2592000, 7200},
}

// Окно rate() — не короче шага: иначе на неделе/месяце точка была бы случайной двухминутной выборкой
// из двух часов, а не средним за них. Значения «сейчас» при этом считаются отдельно, по окну 2m.
const promRateWindow = "2m"

func withWindow(q, w string) string { return strings.ReplaceAll(q, "["+promRateWindow+"]", "["+w+"]") }

// rated — ряды, чьё «сейчас» показывает вкладка; при расширенном окне их берём мгновенным запросом.
var rated = []string{"down", "up", "cpu"}

// Запросы по серверам. Трафик — со счётчиков интерфейса (монотонны), а не с сумм по пирам.
var promQueries = []struct{ key, q string }{
	{"down", `sum by (server_id, server) (rate(awg_link_transmit_bytes_total[2m])) * 8`},
	{"up", `sum by (server_id, server) (rate(awg_link_receive_bytes_total[2m])) * 8`},
	{"online", `sum by (server_id, server) (awg_peers_online)`},
	{"heavy", `sum by (server_id, server) (awg_shaping_heavy_devices)`},
	{"drops", `sum by (server_id, server) (rate(awg_shaping_qdisc_drops_total[2m]))`},
	{"congested", `max by (server_id, server) (awg_shaping_congested)`},
	{"scrape_ok", `max by (server_id, server) (awg_scrape_ok)`},
	{"cpu", `100 * (1 - sum by (server_id, server) (rate(awg_host_cpu_seconds_total{mode=~"idle|iowait"}[2m])) / sum by (server_id, server) (rate(awg_host_cpu_seconds_total[2m])))`},
	{"mem", `100 * (1 - sum by (server_id, server) (awg_host_memory_available_bytes) / sum by (server_id, server) (awg_host_memory_total_bytes))`},
	{"cores", `max by (server_id, server) (awg_host_cpu_cores)`},
	{"mem_total", `max by (server_id, server) (awg_host_memory_total_bytes)`},
}

type point [2]float64

type seriesOne struct {
	name   any
	points []point
}

// series — server_id → ряд, в порядке ответа Prometheus (как Map в JS).
type series struct {
	ids []int
	m   map[int]*seriesOne
}

func (s *series) get(id int) *seriesOne {
	if s == nil {
		return nil
	}
	return s.m[id]
}

type Monitor struct {
	a            *App
	promURL      string
	localEnabled bool
	onNode       bool // панель на ноде другой панели: Prometheus этой машины собирает метрики core, не наши
	http         *http.Client

	srcMu sync.Mutex
	src   *promSource // найденный Prometheus; nil — ещё не искали или не нашли
	srcAt time.Time
}

// promSource — где Prometheus: local — на этой машине, иначе на ноде node (через её агента).
type promSource struct {
	local bool
	node  int
}

// Найденный Prometheus помним недолго; ненайденный ищем при каждом запросе (вкладка
// запрашивает статус редко, а во время установки должна увидеть его сразу).
const promSourceTTL = 30 * time.Second

func newMonitor(a *App) *Monitor {
	return &Monitor{
		a: a, promURL: envOr("PROM_URL", "http://127.0.0.1:9090"),
		localEnabled: strings.ToLower(envOr("LOCAL_NODE", "on")) != "off",
		onNode:       os.Getenv("CORE_HOST") != "",
		http:         &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

var errProm = errors.New("Prometheus не отвечает")

// source — свой Prometheus, если он есть; иначе первый по id сервер, чей агент отдаёт свой.
func (m *Monitor) source() *promSource {
	m.srcMu.Lock()
	if m.src != nil && time.Since(m.srcAt) < promSourceTTL {
		s := m.src
		m.srcMu.Unlock()
		return s
	}
	m.srcMu.Unlock()
	s := m.findSource()
	m.srcMu.Lock()
	m.src, m.srcAt = s, time.Now()
	m.srcMu.Unlock()
	return s
}

// forgetSource — Prometheus поставили, удалили или он перестал отвечать: искать заново.
func (m *Monitor) forgetSource() {
	m.srcMu.Lock()
	m.src = nil
	m.srcMu.Unlock()
}

func (m *Monitor) findSource() *promSource {
	if !m.onNode && m.promReady() {
		return &promSource{local: true}
	}
	ids := m.a.nodes.IDs()
	ready := make([]bool, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		// Сервер 0 со своим awg-ctrl — эта же машина: её Prometheus уже проверен выше.
		if id == 0 && m.localEnabled || !m.a.nodes.IsOnline(id) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.a.nodes.CtrlFor(id)("GET", "/prom/-/ready", nil)
			ready[i] = err == nil && r.Status == 200
		}()
	}
	wg.Wait()
	for i, id := range ids {
		if ready[i] {
			return &promSource{node: id}
		}
	}
	return nil
}

// promGet — JSON-ответ Prometheus (path с запросом): напрямую или через агента ноды.
func (m *Monitor) promGet(src *promSource, path string, q url.Values) (any, error) {
	if src == nil {
		return nil, errProm
	}
	path += "?" + q.Encode()
	if !src.local {
		r, err := m.a.nodes.CtrlFor(src.node)("GET", "/prom"+path, nil)
		if err != nil || r.Status < 200 || r.Status >= 300 {
			return nil, errProm
		}
		return r.Data, nil
	}
	c := &http.Client{Transport: m.http.Transport, Timeout: 8 * time.Second}
	resp, err := c.Get(m.promURL + path)
	if err != nil {
		return nil, errProm
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errProm
	}
	return decodeLikeAxios(raw), nil
}

func (m *Monitor) rangeQuery(src *promSource, query string, start, end, step int64) (*series, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start, 10))
	q.Set("end", strconv.FormatInt(end, 10))
	q.Set("step", strconv.FormatInt(step, 10))
	data, err := m.promGet(src, "/api/v1/query_range", q)
	if err != nil {
		return nil, err
	}
	out := &series{m: map[int]*seriesOne{}}
	for _, r := range arr(field(data, "data", "result")) {
		metric := field(r, "metric")
		idf := jsToNumberAny(field(metric, "server_id"))
		if field(metric, "server_id") == nil {
			idf = math.NaN()
		}
		if idf != float64(int(idf)) {
			continue
		}
		id := int(idf)
		name := field(metric, "server")
		if name == nil {
			name = strconv.Itoa(id)
		}
		s := &seriesOne{name: name}
		for _, v := range arr(field(r, "values")) {
			pair := arr(v)
			if len(pair) < 2 {
				continue
			}
			t := jsToNumberAny(pair[0])
			val := jsToNumberAny(pair[1])
			// NaN/Inf из деления на ноль отбрасываем: на графике это дыра, а не нулевая скорость
			if math.IsNaN(val) || math.IsInf(val, 0) {
				continue
			}
			s.points = append(s.points, point{t, val})
		}
		if _, dup := out.m[id]; !dup {
			out.ids = append(out.ids, id)
		}
		out.m[id] = s
	}
	return out, nil
}

// instantQuery — значение на момент time по каждому server_id.
func (m *Monitor) instantQuery(src *promSource, query string, time_ int64) (map[int]float64, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("time", strconv.FormatInt(time_, 10))
	data, err := m.promGet(src, "/api/v1/query", q)
	if err != nil {
		return nil, err
	}
	out := map[int]float64{}
	for _, r := range arr(field(data, "data", "result")) {
		metric := field(r, "metric")
		if field(metric, "server_id") == nil {
			continue
		}
		idf := jsToNumberAny(field(metric, "server_id"))
		pair := arr(field(r, "value"))
		if idf != float64(int(idf)) || len(pair) < 2 {
			continue
		}
		if v := jsToNumberAny(pair[1]); !math.IsNaN(v) && !math.IsInf(v, 0) {
			out[int(idf)] = v
		}
	}
	return out, nil
}

// total — сумма (или максимум) рядов по меткам времени.
func total(s *series, max bool) []any {
	acc := map[float64]float64{}
	var order []float64
	for _, id := range s.ids {
		for _, p := range s.m[id].points {
			cur, seen := acc[p[0]]
			if !seen {
				order = append(order, p[0])
			}
			if max {
				acc[p[0]] = math.Max(cur, p[1])
			} else {
				acc[p[0]] = cur + p[1]
			}
		}
	}
	sort.Float64s(order)
	out := []any{}
	for _, t := range order {
		out = append(out, []any{t, acc[t]})
	}
	return out
}

func last(s *seriesOne) any {
	if s == nil || len(s.points) == 0 {
		return nil
	}
	return s.points[len(s.points)-1][1]
}

func (m *Monitor) promReady() bool {
	c := &http.Client{Transport: m.http.Transport, Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(m.promURL + "/-/ready")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

type monitorJob struct {
	can     bool
	action  any
	running bool
	exit    any
	log     string
}

// target — чей awg-ctrl ставит и удаляет Prometheus: того сервера, где он уже стоит; если его
// нет — свой (панель + VPN) или выбранный в сервер-баре (панель без VPN). -1 — некому: например,
// Prometheus на машине панели без VPN поставлен руками.
func (m *Monitor) target(src *promSource, qs url.Values) int {
	switch {
	case src != nil && !src.local:
		return src.node
	case m.onNode:
		return -1 // ставит его core: кнопка здесь поставила бы Prometheus для core
	case m.localEnabled:
		return 0
	case src != nil:
		return -1
	}
	if f := jsToNumber(strings.Join(qs["server"], ",")); f == float64(int(f)) && f >= 0 && m.a.nodes.Exists(int(f)) {
		return int(f)
	}
	return -1
}

// host — сервер target, если это не машина панели (её вкладка так и называет: «этот сервер»).
func (m *Monitor) host(target int) any {
	if target < 0 || target == 0 && m.localEnabled {
		return nil
	}
	for _, s := range m.a.nodes.List() {
		if s.ID == target {
			return jsjson.Object{{Key: "id", Val: s.ID}, {Key: "name", Val: s.Name}}
		}
	}
	return nil
}

// jobState — установка возможна, только если у target есть awg-ctrl и он знает маршрут.
func (m *Monitor) jobState(target int) monitorJob {
	if target < 0 {
		return monitorJob{}
	}
	r, err := m.a.nodes.CtrlFor(target)("GET", "/api/monitor/job", nil)
	if err != nil || r.Status != 200 {
		return monitorJob{}
	}
	j := monitorJob{can: true, action: field(r.Data, "action"), running: truthy(field(r.Data, "running")), exit: field(r.Data, "exit")}
	switch l := field(r.Data, "log").(type) {
	case nil:
	case string:
		j.log = l
	default:
		j.log, _ = jsString(l)
	}
	return j
}

func (m *Monitor) router() *Router {
	rt := newRouter()
	rt.Get("/", func(c *Ctx) bool {
		src := m.source()
		target := m.target(src, c.R.URL.Query())
		job := m.jobState(target)
		// Установщик выходит, только когда Prometheus ответил, но проверка выше могла пройти
		// до этого: тогда страница получила бы «готово» вместе с ready=false и перестала следить.
		if src == nil && !job.running && job.action == "install" && job.exit != nil && jsToNumberAny(job.exit) == 0 {
			src = m.source()
		}
		ready := src != nil
		var jobOut any
		if job.can && truthy(job.action) {
			jobOut = jsjson.Object{{Key: "action", Val: job.action}, {Key: "running", Val: job.running}, {Key: "exit", Val: job.exit}, {Key: "log", Val: job.log}}
		}
		return c.JSON(200, jsjson.Object{{Key: "ready", Val: ready}, {Key: "can_install", Val: job.can}, {Key: "job", Val: jobOut},
			{Key: "command", Val: "awg-ctrl monitor install"}, {Key: "host", Val: m.host(target)}, {Key: "on_node", Val: m.onNode}})
	})
	// Установка и удаление — одно и то же: просим awg-ctrl сервера, он запускает CLI от root.
	ask := func(action string) handler {
		return func(c *Ctx) bool {
			target := m.target(m.source(), c.R.URL.Query())
			if target < 0 {
				return c.Error(409, "Некому поставить Prometheus: выберите VPN-сервер или выполните команду на машине")
			}
			m.forgetSource()
			r, err := m.a.nodes.CtrlFor(target)("POST", "/api/monitor/"+action, nil)
			if err != nil {
				return c.Error(502, "awg-ctrl недоступен")
			}
			if r.Status == 404 {
				return c.Error(501, "awg-ctrl старой версии не умеет этого: обновите сервер или выполните команду вручную")
			}
			return c.JSON(r.Status, r.Data)
		}
	}
	rt.Post("/install", ask("install"))
	rt.Post("/remove", ask("remove"))

	rt.Get("/overview", func(c *Ctx) bool {
		qs := c.R.URL.Query()
		rangeKey := "1h"
		if _, ok := qs["range"]; ok {
			rangeKey = strings.Join(qs["range"], ",")
		}
		rg, ok := promRanges[rangeKey]
		if !ok {
			return c.Error(400, "range: 1h, 6h, 24h, 7d или 30d")
		}
		src := m.source()
		end := now()
		start := end - rg.seconds
		window := promRateWindow
		if rg.step > 120 {
			window = strconv.FormatInt(rg.step, 10) + "s"
		}
		got := make([]*series, len(promQueries))
		errs := make([]error, len(promQueries)+2+len(rated))
		// Объём за весь период: increase() по тем же монотонным счётчикам (сбросы счётчика он учитывает).
		// Окно расширено — «сейчас» с конца ряда было бы средним за шаг; берём мгновенные значения по 2m.
		var usedDown, usedUp map[int]float64
		nowVals := map[string]map[int]float64{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i, pq := range promQueries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i], errs[i] = m.rangeQuery(src, withWindow(pq.q, window), start, end, rg.step)
			}()
		}
		span := strconv.FormatInt(rg.seconds, 10) + "s"
		n := len(promQueries)
		wg.Add(2)
		go func() {
			defer wg.Done()
			usedDown, errs[n] = m.instantQuery(src, `sum by (server_id, server) (increase(awg_link_transmit_bytes_total[`+span+`]))`, end)
		}()
		go func() {
			defer wg.Done()
			usedUp, errs[n+1] = m.instantQuery(src, `sum by (server_id, server) (increase(awg_link_receive_bytes_total[`+span+`]))`, end)
		}()
		if window != promRateWindow {
			for j, k := range rated {
				for _, pq := range promQueries {
					if pq.key != k {
						continue
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						v, err := m.instantQuery(src, pq.q, end)
						errs[n+2+j] = err
						mu.Lock()
						nowVals[k] = v
						mu.Unlock()
					}()
				}
			}
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				m.forgetSource()
				return c.Error(502, err.Error())
			}
		}
		used := func(v map[int]float64, id int) any {
			if x, ok := v[id]; ok {
				return x
			}
			return nil
		}
		// Вкладка показывает один сервер — выбранный в сервер-баре (?server=<id>).
		if _, has := qs["server"]; has {
			if f := jsToNumber(strings.Join(qs["server"], ",")); f == float64(int(f)) {
				only := int(f)
				for i, s := range got {
					f := &series{m: map[int]*seriesOne{}}
					if x, ok := s.m[only]; ok {
						f.ids, f.m[only] = []int{only}, x
					}
					got[i] = f
				}
			}
		}
		s := map[string]*series{}
		for i, pq := range promQueries {
			s[pq.key] = got[i]
		}
		cur := func(k string, id int) any {
			if v, ok := nowVals[k]; ok {
				if x, ok := v[id]; ok {
					return x
				}
				return nil
			}
			return last(s[k].get(id))
		}
		ids := map[int]bool{}
		for _, x := range got {
			for _, id := range x.ids {
				ids[id] = true
			}
		}
		var sorted []int
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Ints(sorted)
		servers := []any{}
		for _, id := range sorted {
			var name any = strconv.Itoa(id)
			for _, k := range []string{"scrape_ok", "down", "online"} {
				if x := s[k].get(id); x != nil {
					name = x.name
					break
				}
			}
			servers = append(servers, jsjson.Object{
				{Key: "id", Val: id}, {Key: "name", Val: name}, {Key: "ok", Val: last(s["scrape_ok"].get(id)) == 1.0},
				{Key: "down", Val: cur("down", id)}, {Key: "up", Val: cur("up", id)},
				{Key: "online", Val: last(s["online"].get(id))}, {Key: "heavy", Val: last(s["heavy"].get(id))},
				{Key: "congested", Val: last(s["congested"].get(id)) == 1.0}, {Key: "cpu", Val: cur("cpu", id)},
				{Key: "mem", Val: last(s["mem"].get(id))}, {Key: "cores", Val: last(s["cores"].get(id))},
				{Key: "mem_total", Val: last(s["mem_total"].get(id))},
				{Key: "used_down", Val: used(usedDown, id)}, {Key: "used_up", Val: used(usedUp, id)},
			})
		}
		var rangeOut any = "1h"
		if _, ok := qs["range"]; ok {
			rangeOut = rangeKey
		}
		return c.JSON(200, jsjson.Object{
			{Key: "range", Val: rangeOut}, {Key: "step", Val: rg.step}, {Key: "from", Val: start}, {Key: "to", Val: end},
			{Key: "servers", Val: servers},
			{Key: "total", Val: jsjson.Object{
				{Key: "down", Val: total(s["down"], false)}, {Key: "up", Val: total(s["up"], false)}, {Key: "online", Val: total(s["online"], false)},
				{Key: "heavy", Val: total(s["heavy"], false)}, {Key: "drops", Val: total(s["drops"], false)},
				{Key: "cpu", Val: total(s["cpu"], true)}, {Key: "mem", Val: total(s["mem"], true)},
			}},
		})
	})
	return rt
}
