package ui

// Вкладка «Монитор» (порт awg-ui/monitor.ts): встроенный Prometheus и основные графики.
//   GET  /ui/monitor            — готов ли Prometheus, можно ли поставить его кнопкой, ход установки
//   POST /ui/monitor/install    — попросить локальный awg-ctrl выполнить `awg-ctrl monitor install` (root)
//   POST /ui/monitor/remove     — то же для `awg-ctrl monitor remove`
//   GET  /ui/monitor/overview   — серии для графиков за 1 ч / 6 ч / 24 ч
// Панель сама пакетов не ставит и в Prometheus ходит только фиксированными запросами.

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type promRange struct{ seconds, step int64 }

// Шаг — чтобы на графике было ~250–360 точек; окно rate — минимум два скрейпа (15 с).
var promRanges = map[string]promRange{"1h": {3600, 15}, "6h": {21600, 60}, "24h": {86400, 300}}

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
	http         *http.Client
}

func newMonitor(a *App) *Monitor {
	return &Monitor{
		a: a, promURL: envOr("PROM_URL", "http://127.0.0.1:9090"),
		localEnabled: strings.ToLower(envOr("LOCAL_NODE", "on")) != "off",
		http:         &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

var errProm = errors.New("Prometheus не отвечает")

func (m *Monitor) rangeQuery(query string, start, end, step int64) (*series, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start, 10))
	q.Set("end", strconv.FormatInt(end, 10))
	q.Set("step", strconv.FormatInt(step, 10))
	c := &http.Client{Transport: m.http.Transport, Timeout: 8 * time.Second}
	resp, err := c.Get(m.promURL + "/api/v1/query_range?" + q.Encode())
	if err != nil {
		return nil, errProm
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errProm
	}
	out := &series{m: map[int]*seriesOne{}}
	for _, r := range arr(field(decodeLikeAxios(raw), "data", "result")) {
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

// jobState — установка возможна, только если рядом свой awg-ctrl и он знает маршрут.
func (m *Monitor) jobState() monitorJob {
	if !m.localEnabled {
		return monitorJob{}
	}
	r, err := m.a.ctrlLocal("GET", "/api/monitor/job", nil)
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
		var ready bool
		var job monitorJob
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); ready = m.promReady() }()
		go func() { defer wg.Done(); job = m.jobState() }()
		wg.Wait()
		var jobOut any
		if job.can && truthy(job.action) {
			jobOut = jsjson.Object{{Key: "action", Val: job.action}, {Key: "running", Val: job.running}, {Key: "exit", Val: job.exit}, {Key: "log", Val: job.log}}
		}
		return c.JSON(200, jsjson.Object{{Key: "ready", Val: ready}, {Key: "can_install", Val: job.can}, {Key: "job", Val: jobOut},
			{Key: "command", Val: "awg-ctrl monitor install"}})
	})
	// Установка и удаление — одно и то же: просим локальный awg-ctrl, он запускает CLI от root.
	ask := func(action string) handler {
		return func(c *Ctx) bool {
			if !m.localEnabled {
				return c.Error(409, "У этой панели нет своего VPN-сервера: выполните команду на машине")
			}
			r, err := m.a.ctrlLocal("POST", "/api/monitor/"+action, nil)
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
			return c.Error(400, "range: 1h, 6h или 24h")
		}
		end := now()
		start := end - rg.seconds
		got := make([]*series, len(promQueries))
		errs := make([]error, len(promQueries))
		var wg sync.WaitGroup
		for i, pq := range promQueries {
			wg.Add(1)
			go func() { defer wg.Done(); got[i], errs[i] = m.rangeQuery(pq.q, start, end, rg.step) }()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return c.Error(502, err.Error())
			}
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
				{Key: "down", Val: last(s["down"].get(id))}, {Key: "up", Val: last(s["up"].get(id))},
				{Key: "online", Val: last(s["online"].get(id))}, {Key: "heavy", Val: last(s["heavy"].get(id))},
				{Key: "congested", Val: last(s["congested"].get(id)) == 1.0}, {Key: "cpu", Val: last(s["cpu"].get(id))},
				{Key: "mem", Val: last(s["mem"].get(id))}, {Key: "cores", Val: last(s["cores"].get(id))},
				{Key: "mem_total", Val: last(s["mem_total"].get(id))},
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
