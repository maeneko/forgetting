package ui

// Публичная проверка здоровья: GET /api/v1/health (ключ awgk_). Отвечает всегда 200, если
// ключ принят: недоступная нода — это факт о сервере, а не ошибка запроса, поэтому она
// попадает в его запись (online:false, source:"none", error), а не в 5xx. Ключ без серверов
// получает пустую область, а не «всю панель».
//
// Источник — живой /api/metrics каждого сервера через CtrlFor (локальный awg-ctrl по HTTP,
// ноды по WSS): up, peers{total,online}, rx/tx_bytes, host{cpu,cores,load,mem}. Ответ не
// кэшируется. cpu_percent считается по дельте счётчиков host.cpu между двумя запросами:
// первый вызов и сброс счётчика дают null.
//
// Приватность: наружу уходят только id, имя сервера (его выбрал оператор), флаги, счётчики,
// байты, проценты и load. Меток ключей, uuid, device_*, списков пиров, IP, путей к секретам,
// PSK и прочего в ответе нет и быть не может.

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// healthCallTimeout — бюджет запроса к /api/metrics одного сервера. Серверы опрашиваются
// параллельно, поэтому 6 с — это и потолок всего запроса (как monitorCallTimeout).
const healthCallTimeout = 6 * time.Second

// cpuSample — счётчики секунд по режимам с прошлого запроса (host.cpu).
type cpuSample map[string]float64

type healthAPI struct {
	a *App
	// ctrl — источник Ctrl для сервера; nil — a.nodes.CtrlFor. Поле есть только для тестов.
	ctrl func(id int) Ctrl

	mu      sync.Mutex
	samples map[int]cpuSample
}

func newHealthAPI(a *App) *healthAPI {
	return &healthAPI{a: a, samples: map[int]cpuSample{}}
}

// ctrlFor — контроллер сервера: локальный awg-ctrl или нода, как у остального API.
func (h *healthAPI) ctrlFor(id int) Ctrl {
	if h.ctrl != nil {
		return h.ctrl(id)
	}
	return h.a.nodes.CtrlFor(id)
}

// handle — GET /api/v1/health.
func (h *healthAPI) handle(c *Ctx) bool {
	// Тот же лимитер, что у /api/v1/monitor: 60 запросов в минуту на ключ. Отдельного
	// лимитера с другими числами нет — ключ делит бюджет между двумя публичными ручками.
	if m := h.a.monitor; m != nil && m.apiLimit != nil {
		key := ""
		if c.apiKey != nil {
			key = strconv.FormatInt(c.apiKey.ID, 10)
		}
		if !m.apiLimit.allow(key) {
			c.W.Header().Set("Retry-After", "60")
			return c.Error(429, "Too many requests")
		}
	}

	scope := []int{}
	if c.servers != nil {
		seen := map[int]bool{}
		for _, id := range c.servers {
			if !seen[id] {
				seen[id] = true
				scope = append(scope, id)
			}
		}
		sort.Ints(scope)
	}

	// Реестр — единственный источник id и имён; область ключа проверяется по нему, поэтому
	// удалённый сервер в ответ не попадает. Список уже упорядочен, но id сортируем явно.
	byID := map[int]serverInfo{}
	if h.a.nodes != nil {
		for _, s := range h.a.nodes.List() {
			byID[s.ID] = s
		}
	}
	var ids []int
	for _, id := range scope {
		if _, ok := byID[id]; ok {
			ids = append(ids, id)
		}
	}

	// Веер параллельный: каждый сервер получает свой бюджет, общий — один healthCallTimeout.
	results := make([]healthResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = h.fetch(id)
		}()
	}
	wg.Wait()

	servers := []any{}
	online := 0
	up := 0
	peers := 0.0
	peersOnline := 0.0
	for i, id := range ids {
		r := results[i]
		obj := jsjson.Object{{Key: "id", Val: id}, {Key: "name", Val: byID[id].Name}}
		if !r.reachable() {
			// Счётчик CPU больше не с чем сравнивать: следующий успешный ответ — первый.
			h.forgetCPU(id)
			servers = append(servers, append(obj,
				jsjson.KV{Key: "online", Val: false},
				jsjson.KV{Key: "up", Val: nil},
				jsjson.KV{Key: "peers", Val: nil},
				jsjson.KV{Key: "peers_online", Val: nil},
				jsjson.KV{Key: "rx_bytes", Val: nil},
				jsjson.KV{Key: "tx_bytes", Val: nil},
				jsjson.KV{Key: "cores", Val: nil},
				jsjson.KV{Key: "load", Val: nil},
				jsjson.KV{Key: "mem_total_bytes", Val: nil},
				jsjson.KV{Key: "mem_available_bytes", Val: nil},
				jsjson.KV{Key: "cpu_percent", Val: nil},
				jsjson.KV{Key: "source", Val: "none"},
				jsjson.KV{Key: "error", Val: "ctrl недоступен"},
			))
			continue
		}

		d := r.resp.Data
		isUp, upOK := field(d, "up").(bool)
		host := field(d, "host")
		hasHost := truthy(host)
		cpu := h.cpuPercent(id, host)
		load := any(nil)
		cores := any(nil)
		memTotal := any(nil)
		memAvail := any(nil)
		if hasHost {
			if l := arr(field(host, "load")); l != nil {
				out := make([]any, 0, len(l))
				for _, v := range l {
					if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
						out = append(out, f)
					} else {
						out = append(out, nil)
					}
				}
				load = out
			}
			cores = healthNum(field(host, "cores"))
			memTotal = healthNum(field(host, "mem", "total"))
			memAvail = healthNum(field(host, "mem", "available"))
		} else {
			h.forgetCPU(id)
		}
		var upVal, peersVal, onlineVal, rx, tx any
		if upOK {
			upVal = isUp
			if isUp {
				up++
			}
		}
		if n := healthNum(field(d, "peers", "total")); n != nil {
			peersVal = n
			peers += n.(float64)
		}
		if n := healthNum(field(d, "peers", "online")); n != nil {
			onlineVal = n
			peersOnline += n.(float64)
		}
		rx = healthNum(field(d, "rx_bytes"))
		tx = healthNum(field(d, "tx_bytes"))
		online++
		servers = append(servers, append(obj,
			jsjson.KV{Key: "online", Val: true},
			jsjson.KV{Key: "up", Val: upVal},
			jsjson.KV{Key: "peers", Val: peersVal},
			jsjson.KV{Key: "peers_online", Val: onlineVal},
			jsjson.KV{Key: "rx_bytes", Val: rx},
			jsjson.KV{Key: "tx_bytes", Val: tx},
			jsjson.KV{Key: "cores", Val: cores},
			jsjson.KV{Key: "load", Val: load},
			jsjson.KV{Key: "mem_total_bytes", Val: memTotal},
			jsjson.KV{Key: "mem_available_bytes", Val: memAvail},
			jsjson.KV{Key: "cpu_percent", Val: cpu},
			jsjson.KV{Key: "source", Val: "ctrl"},
		))
	}

	return c.JSON(200, jsjson.Object{
		{Key: "generated_at", Val: time.Unix(now(), 0).UTC().Format(time.RFC3339)},
		{Key: "scope", Val: scope},
		{Key: "summary", Val: jsjson.Object{
			{Key: "servers_total", Val: len(servers)},
			{Key: "servers_online", Val: online},
			{Key: "servers_up", Val: up},
			{Key: "peers", Val: peers},
			{Key: "peers_online", Val: peersOnline},
		}},
		{Key: "servers", Val: servers},
	})
}

// healthResult — ответ /api/metrics сервера или то, почему его нет.
type healthResult struct {
	resp *CtrlResp
	err  error
}

func (r healthResult) reachable() bool {
	return r.err == nil && r.resp != nil && r.resp.Status == 200
}

// fetch — один сервер с бюджетом healthCallTimeout. Зависший Ctrl не держит запрос дольше:
// горутина пишет в буферизованный канал и завершается сама.
func (h *healthAPI) fetch(id int) healthResult {
	ch := make(chan healthResult, 1)
	go func() {
		resp, err := h.ctrlFor(id)("GET", "/api/metrics", nil)
		ch <- healthResult{resp: resp, err: err}
	}()
	select {
	case r := <-ch:
		return r
	case <-time.After(healthCallTimeout):
		return healthResult{err: errHealthTimeout}
	}
}

var errHealthTimeout = &healthTimeoutError{}

type healthTimeoutError struct{}

func (*healthTimeoutError) Error() string { return "ctrl недоступен: таймаут" }

// healthNum — число из JSON или nil (не 0 вместо «неизвестно»).
func healthNum(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return nil
}

// forgetCPU — сбросить базовую линию сервера: следующий ответ снова даст null.
func (h *healthAPI) forgetCPU(id int) {
	h.mu.Lock()
	delete(h.samples, id)
	h.mu.Unlock()
}

// cpuPercent — занятость CPU по дельте host.cpu между двумя запросами. Все режимы в сумме
// дают прошедшее время, idle — простой; busy/total*100, один знак. Первый запрос, сброс
// счётчика (отрицательная дельта), пропавший idle или нулевой интервал — null.
func (h *healthAPI) cpuPercent(id int, host any) any {
	list := arr(field(host, "cpu"))
	if list == nil {
		h.forgetCPU(id)
		return nil
	}
	cur := cpuSample{}
	for _, item := range list {
		mode, ok := field(item, "mode").(string)
		if !ok || mode == "" {
			continue
		}
		v := field(item, "val")
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			h.forgetCPU(id)
			return nil
		}
		cur[mode] = f
	}
	if len(cur) == 0 {
		h.forgetCPU(id)
		return nil
	}

	h.mu.Lock()
	prev := h.samples[id]
	h.samples[id] = cur
	h.mu.Unlock()
	if prev == nil {
		return nil
	}

	total, idle := 0.0, 0.0
	idleSeen := false
	for mode, v := range cur {
		p, ok := prev[mode]
		if !ok {
			continue // режим появился только сейчас — дельты у него нет
		}
		d := v - p
		if d < 0 { // счётчик перезапущен
			return nil
		}
		total += d
		if mode == "idle" {
			idle, idleSeen = d, true
		}
	}
	if !idleSeen || total <= 0 || idle > total {
		return nil
	}
	pct := (total - idle) / total * 100
	return math.Round(pct*10) / 10
}
