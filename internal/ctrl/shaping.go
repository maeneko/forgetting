package ctrl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Честное деление канала между устройствами (CAKE) + автоподстройка по RFC 6057.
//
// Схема:
//   загрузка (к клиенту)   — CAKE root на awg1, dual-dsthost, diffserv3
//   отдача (от клиента)    — ingress awg1 → ifb-awg, CAKE dual-srchost, besteffort
//   тяжёлые при перегрузке — DSCP CS1 (bulk-tin CAKE) через отдельную nft-таблицу
//   с меткой «торренты»   — туда же, пока метка висит (см. torrent.go), независимо от перегрузки
//
// Ничего не пишет в awg1.conf (PostUp не трогаем): всё применяется из процесса и
// идемпотентно. Правила tc пропадают вместе с интерфейсом, поэтому Apply зовётся на
// старте, после restartAwg()/startInterface() и из страховочной проверки в цикле.
//
// Сэмплер только КЛАССИФИЦИРУЕТ. Он никого не удаляет и не отключает — ни по времени,
// ни по неактивности (правило проекта: удаление только по явному списку от панели).

type ShapingSettings struct {
	Enabled  bool `json:"enabled"`
	DownMbit int  `json:"downMbit"`
	UpMbit   int  `json:"upMbit"`
	Auto     bool `json:"auto"`
}

type QdiscStats struct {
	Packets float64 `json:"packets"`
	Bytes   float64 `json:"bytes"`
	Drops   float64 `json:"drops"`
	Backlog float64 `json:"backlog"`
}

type ShapingStatus struct {
	Settings  ShapingSettings `json:"settings"`
	Active    bool            `json:"active"`
	Congested bool            `json:"congested"`
	Heavy     []string        `json:"heavy"`
	Devices   int             `json:"devices"`
	RateDown  float64         `json:"rateDown"`
	RateUp    float64         `json:"rateUp"`
	Down      *QdiscStats     `json:"down,omitempty"`
	Up        *QdiscStats     `json:"up,omitempty"`
}

const (
	congestOn    = 0.85 // доля ширины, выше которой канал считается перегруженным
	congestOff   = 0.70 // и ниже которой перегрузка снимается (гистерезис)
	activeMbit   = 0.1  // меньше — устройство не считается активным
	heavyMult    = 1.5  // тяжёлое: ≥ 1.5 честных долей…
	heavyMinFrac = 0.05 // …и не меньше 5% ширины канала
	checkEvery   = 3    // страховочная проверка qdisc — раз в N тиков
	ifbName      = "ifb-awg"
	nftTable     = "awg_shape"
)

// Окна и пороги; переопределение через env — только для тестов на стенде.
func envNum(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

type tick struct{ dt, bytes float64 }

type peerCounters struct {
	IP     string
	Rx, Tx float64
}

// orderedSet — множество с порядком вставки (JS Set): порядок попадает в nft-скрипт.
type orderedSet struct {
	keys []string
	m    map[string]bool
}

func newOrderedSet() *orderedSet        { return &orderedSet{m: map[string]bool{}} }
func (o *orderedSet) has(k string) bool { return o.m[k] }
func (o *orderedSet) add(k string) {
	if !o.m[k] {
		o.m[k] = true
		o.keys = append(o.keys, k)
	}
}
func (o *orderedSet) del(k string) {
	if !o.m[k] {
		return
	}
	delete(o.m, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}
func (o *orderedSet) clear()         { o.keys, o.m = nil, map[string]bool{} }
func (o *orderedSet) list() []string { return append([]string(nil), o.keys...) }
func (o *orderedSet) size() int      { return len(o.keys) }

type Shaping struct {
	s *Server

	opMu sync.Mutex // сериализует tc/nft: два параллельных Apply дали бы полусобранные qdisc

	mu          sync.Mutex // состояние сэмплера
	prev        map[string][2]float64
	history     map[string][]tick
	histOrder   []string
	totals      []tick
	heavy       *orderedSet
	applied     *orderedSet // что сейчас стоит в nft-наборе: тяжёлые ∪ с меткой торрента
	congested   bool
	lastRateDn  float64
	lastRateUp  float64
	lastTickAt  time.Time
	tickNo      int
	lastError   string
	windowTicks int
	sampleS     float64
	started     bool
}

func newShaping(s *Server) *Shaping {
	sampleS := envNum("SHAPE_SAMPLE_S", 10)
	windowS := envNum("SHAPE_WINDOW_S", 600)
	return &Shaping{
		s: s, prev: map[string][2]float64{}, history: map[string][]tick{}, heavy: newOrderedSet(), applied: newOrderedSet(),
		sampleS: sampleS, windowTicks: int(math.Max(3, math.Round(windowS/sampleS))),
	}
}

func (h *Shaping) Settings() ShapingSettings {
	s := h.s
	down, _ := strconv.Atoi(s.getCfg("shapeDownMbit", "0"))
	up, _ := strconv.Atoi(s.getCfg("shapeUpMbit", "0"))
	return ShapingSettings{
		Enabled:  s.getCfg("shapeEnabled", "0") == "1",
		DownMbit: down, UpMbit: up,
		Auto: s.getCfg("shapeAuto", "1") == "1",
	}
}

func (h *Shaping) save(st ShapingSettings) {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	h.s.setCfg("shapeEnabled", b(st.Enabled))
	h.s.setCfg("shapeDownMbit", strconv.Itoa(st.DownMbit))
	h.s.setCfg("shapeUpMbit", strconv.Itoa(st.UpMbit))
	h.s.setCfg("shapeAuto", b(st.Auto))
}

func (h *Shaping) noteError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := err.Error(); m != h.lastError { // одинаковую ошибку в лог не спамим
		h.s.log.Warn("shaping: " + m)
		h.lastError = m
	}
}

func (h *Shaping) overhead() int {
	if v := os.Getenv("SHAPE_OVERHEAD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return int(f)
		}
	}
	return 60 + h.s.cfg().Params.S4
}

func shExec(name string, args ...string) (string, error) {
	return runCtx(15*time.Second, name, args...)
}

func shExecStdin(name string, args []string, input string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var se bytes.Buffer
	cmd.Stderr = &se
	cmd.Stdin = strings.NewReader(input)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(se.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}

func (h *Shaping) nftApply(heavy []string) error {
	elements := ""
	if len(heavy) > 0 {
		elements = "\n        elements = { " + strings.Join(heavy, ", ") + " }"
	}
	// add+delete в одной транзакции: таблица пересоздаётся атомарно, без окна без правил.
	script := "\nadd table inet " + nftTable + "\ndelete table inet " + nftTable + "\ntable inet " + nftTable + " {\n" +
		"    set heavy {\n        type ipv4_addr" + elements + "\n    }\n" +
		"    chain postrouting {\n        type filter hook postrouting priority -150; policy accept;\n" +
		"        oifname \"" + h.s.iface + "\" ip daddr @heavy ip dscp set cs1\n    }\n}\n"
	return shExecStdin("nft", []string{"-f", "-"}, script)
}

func (h *Shaping) hasCake(dev string) bool {
	out, err := shExec("tc", "qdisc", "show", "dev", dev, "root")
	return err == nil && strings.Contains(out, "cake")
}

func (h *Shaping) hasIngressRedirect() bool {
	out, err := shExec("tc", "filter", "show", "dev", h.s.iface, "parent", "ffff:")
	return err == nil && strings.Contains(out, "mirred")
}

func (h *Shaping) applyNow() error {
	st := h.Settings()
	if !st.Enabled || st.DownMbit <= 0 || st.UpMbit <= 0 {
		h.removeNow()
		return nil
	}
	iface := h.s.iface
	oh := strconv.Itoa(h.overhead())

	// Загрузка (к клиенту). replace = change, если qdisc уже стоит: скорость меняется на лету.
	if _, err := shExec("tc", "qdisc", "replace", "dev", iface, "root", "cake",
		"bandwidth", fmt.Sprintf("%dmbit", st.DownMbit), "diffserv3", "dual-dsthost", "overhead", oh); err != nil {
		return err
	}
	// Отдача (от клиента): зеркалим входящий трафик awg1 на ifb и делим там.
	if _, err := shExec("ip", "link", "show", ifbName); err != nil {
		if _, err := shExec("ip", "link", "add", ifbName, "type", "ifb"); err != nil {
			return err
		}
	}
	if _, err := shExec("ip", "link", "set", ifbName, "up"); err != nil {
		return err
	}
	if _, err := shExec("tc", "qdisc", "replace", "dev", ifbName, "root", "cake",
		"bandwidth", fmt.Sprintf("%dmbit", st.UpMbit), "besteffort", "dual-srchost", "overhead", oh); err != nil {
		return err
	}
	if !h.hasIngressRedirect() {
		shExec("tc", "qdisc", "del", "dev", iface, "ingress") // ошибки игнорируем
		if _, err := shExec("tc", "qdisc", "add", "dev", iface, "handle", "ffff:", "ingress"); err != nil {
			return err
		}
		if _, err := shExec("tc", "filter", "add", "dev", iface, "parent", "ffff:", "protocol", "all", "prio", "1",
			"matchall", "action", "mirred", "egress", "redirect", "dev", ifbName); err != nil {
			return err
		}
	}
	heavy := h.desired()
	if err := h.nftApply(heavy); err != nil {
		return err
	}
	h.mu.Lock()
	h.applied = setOf(heavy)
	h.lastError = ""
	h.mu.Unlock()
	h.s.log.Info("shaping applied", "down", st.DownMbit, "up", st.UpMbit, "overhead", oh, "heavy", len(heavy))
	return nil
}

func (h *Shaping) removeNow() {
	iface := h.s.iface
	shExec("tc", "qdisc", "del", "dev", iface, "root")
	shExec("tc", "qdisc", "del", "dev", iface, "ingress")
	shExec("ip", "link", "del", ifbName)
	shExec("nft", "delete", "table", "inet", nftTable)
	h.mu.Lock()
	h.heavy.clear()
	h.applied.clear()
	h.history, h.histOrder, h.prev = map[string][]tick{}, nil, map[string][2]float64{}
	h.totals = nil
	h.congested = false
	h.mu.Unlock()
}

// Apply не бросает: отсутствие tc/nft/модуля не должно валить awg-ctrl (в отличие от
// ensureInterfaceUp).
func (h *Shaping) Apply() {
	h.opMu.Lock()
	err := h.applyNow()
	h.opMu.Unlock()
	if err != nil {
		h.noteError(err)
	}
}

// Update сохраняет настройки и применяет их; ошибку применения отдаёт вызывающему —
// пользователю нужна честная причина, а не молчаливый warn.
func (h *Shaping) Update(st ShapingSettings) error {
	h.save(st)
	h.opMu.Lock()
	err := h.applyNow()
	h.opMu.Unlock()
	if err == nil {
		h.mu.Lock()
		h.lastError = ""
		h.mu.Unlock()
	}
	return err
}

// desired — кого держать в фоновом классе: тяжёлые (порядок сэмплера) и, следом, устройства с
// меткой «торренты». Метку ставит и снимает nft (таймаут), поэтому сверяемся каждый тик.
func (h *Shaping) desired() []string {
	h.mu.Lock()
	out := setOf(h.heavy.list())
	h.mu.Unlock()
	for _, ip := range h.s.torrent.SeenList() {
		out.add(ip)
	}
	return out.list()
}

func setOf(list []string) *orderedSet {
	o := newOrderedSet()
	for _, k := range list {
		o.add(k)
	}
	return o
}

// syncNft — привести nft-набор к desired(); без изменений nft не трогаем.
func (h *Shaping) syncNft() {
	want := h.desired()
	h.mu.Lock()
	same := len(want) == h.applied.size()
	for _, ip := range want {
		same = same && h.applied.has(ip)
	}
	h.mu.Unlock()
	if same {
		return
	}
	h.opMu.Lock()
	err := h.nftApply(want)
	h.opMu.Unlock()
	if err != nil {
		h.noteError(err)
		return
	}
	h.mu.Lock()
	h.applied = setOf(want)
	h.mu.Unlock()
}

// ───────── сэмплер (RFC 6057) ─────────

func (h *Shaping) pushTick(arr []tick, t tick) []tick {
	arr = append(arr, t)
	for len(arr) > h.windowTicks {
		arr = arr[1:]
	}
	return arr
}

func mbit(arr []tick) float64 {
	var dt, by float64
	for _, t := range arr {
		dt += t.dt
		by += t.bytes
	}
	if dt <= 0 {
		return 0
	}
	return by * 8 / dt / 1e6
}

func (h *Shaping) readPeers() ([]peerCounters, error) {
	out, err := runCtx(10*time.Second, "awg", "show", h.s.iface, "dump")
	if err != nil {
		return nil, err
	}
	var rows []peerCounters
	lines := strings.Split(out, "\n")
	if len(lines) > 0 {
		lines = lines[1:]
	}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) < 4 || f[0] == "" || f[3] == "" {
			continue
		}
		ip := strings.SplitN(strings.SplitN(f[3], ",", 2)[0], "/", 2)[0]
		rx, tx := 0.0, 0.0
		if len(f) > 5 {
			rx = orZero(jsNumber(f[5]))
		}
		if len(f) > 6 {
			tx = orZero(jsNumber(f[6]))
		}
		rows = append(rows, peerCounters{ip, rx, tx})
	}
	return rows, nil
}

func orZero(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

func (h *Shaping) tick() {
	st := h.Settings()
	if !st.Enabled {
		return
	}
	now := time.Now()
	h.mu.Lock()
	dt := h.sampleS
	if !h.lastTickAt.IsZero() {
		dt = now.Sub(h.lastTickAt).Seconds()
	}
	h.lastTickAt = now
	h.tickNo++
	tickNo := h.tickNo
	h.mu.Unlock()
	// В конце любого тика, в том числе пока окно не набралось: метка торрента не ждёт 10 минут.
	defer h.syncNft()

	// Страховка: интерфейс могли поднять руками (awg-quick down/up) — qdisc тогда исчез.
	if tickNo%checkEvery == 0 && !h.hasCake(h.s.iface) {
		h.s.log.Warn("shaping: qdisc на интерфейсе пропал, применяю заново")
		h.Apply()
	}

	peers, err := h.readPeers()
	if err != nil {
		h.noteError(err)
		return
	}

	h.mu.Lock()
	var totalDown, totalUp float64
	seen := map[string]bool{}
	for _, p := range peers {
		seen[p.IP] = true
		before, had := h.prev[p.IP]
		// Счётчики ядра сбрасываются при down/up интерфейса и замене ключа пира →
		// отрицательная дельта = 0.
		dTx, dRx := 0.0, 0.0
		if had && p.Tx >= before[1] {
			dTx = p.Tx - before[1]
		}
		if had && p.Rx >= before[0] {
			dRx = p.Rx - before[0]
		}
		h.prev[p.IP] = [2]float64{p.Rx, p.Tx}
		totalDown += dTx
		totalUp += dRx
		if _, ok := h.history[p.IP]; !ok {
			h.histOrder = append(h.histOrder, p.IP)
		}
		h.history[p.IP] = h.pushTick(h.history[p.IP], tick{dt, dTx})
	}
	var kept []string
	for _, ip := range h.histOrder {
		if seen[ip] {
			kept = append(kept, ip)
			continue
		}
		delete(h.prev, ip)
		delete(h.history, ip)
		h.heavy.del(ip)
	}
	h.histOrder = kept
	h.totals = h.pushTick(h.totals, tick{dt, totalDown})
	h.lastRateDn = totalDown * 8 / dt / 1e6
	h.lastRateUp = totalUp * 8 / dt / 1e6

	if !st.Auto {
		h.heavy.clear()
		h.congested = false
		h.mu.Unlock()
		return
	}
	if len(h.totals) < h.windowTicks { // пока окно не набралось, решений не принимаем
		h.mu.Unlock()
		return
	}

	avg := mbit(h.totals)
	wasCongested := h.congested
	if !h.congested && avg >= congestOn*float64(st.DownMbit) {
		h.congested = true
	}
	if h.congested && avg < congestOff*float64(st.DownMbit) {
		h.congested = false
	}

	next := newOrderedSet()
	if h.congested {
		type rate struct {
			ip string
			r  float64
		}
		var rates []rate
		for _, ip := range h.histOrder {
			if hh := h.history[ip]; len(hh) >= h.windowTicks {
				rates = append(rates, rate{ip, mbit(hh)})
			}
		}
		active := 0
		for _, r := range rates {
			if r.r >= activeMbit {
				active++
			}
		}
		share := float64(st.DownMbit) / math.Max(1, float64(active))
		for _, r := range rates {
			wasHeavy := h.heavy.has(r.ip)
			// Входим на 1.5 доли, выходим ниже 1.0 доли — без дребезга на границе.
			if r.r >= math.Max(heavyMult*share, heavyMinFrac*float64(st.DownMbit)) || (wasHeavy && r.r >= share) {
				next.add(r.ip)
			}
		}
	}
	changed := next.size() != h.heavy.size()
	if !changed {
		for _, ip := range next.keys {
			if !h.heavy.has(ip) {
				changed = true
				break
			}
		}
	}
	if changed || wasCongested != h.congested {
		h.heavy = next
		h.s.log.Info("shaping: класс тяжёлых обновлён", "congested", h.congested, "avgMbit", math.Round(avg), "heavy", next.list())
	}
	h.mu.Unlock()
}

func (h *Shaping) Start() {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()
	go func() {
		t := time.NewTicker(time.Duration(h.sampleS * float64(time.Second)))
		defer t.Stop()
		var busy sync.Mutex
		for range t.C {
			if !busy.TryLock() { // предыдущий тик ещё идёт — пропускаем, а не копим очередь
				continue
			}
			go func() {
				defer busy.Unlock()
				h.tick()
			}()
		}
	}()
}

// ───────── состояние ─────────

func (h *Shaping) qdiscStats(dev string) *QdiscStats {
	out, err := shExec("tc", "-s", "-j", "qdisc", "show", "dev", dev, "root")
	if err != nil {
		return nil
	}
	var arr []map[string]any
	if json.Unmarshal([]byte(out), &arr) != nil {
		return nil
	}
	for _, q := range arr {
		if q["kind"] == "cake" {
			n := func(k string) float64 { f, _ := q[k].(float64); return f }
			return &QdiscStats{n("packets"), n("bytes"), n("drops"), n("backlog")}
		}
	}
	return nil
}

func (h *Shaping) PriorityOf(ip string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.applied.has(ip) { // то, что реально стоит в nft: тяжёлые и с меткой торрента
		return "background"
	}
	return "normal"
}

func (h *Shaping) Status() ShapingStatus {
	st := h.Settings()
	var active bool
	var down, up *QdiscStats
	if st.Enabled {
		active = h.hasCake(h.s.iface)
		down, up = h.qdiscStats(h.s.iface), h.qdiscStats(ifbName)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	devices := 0
	for _, hh := range h.history {
		if mbit(hh) >= activeMbit {
			devices++
		}
	}
	heavy := h.applied.list()
	if heavy == nil {
		heavy = []string{}
	}
	round1 := func(f float64) float64 { return math.Round(f*10) / 10 }
	return ShapingStatus{
		Settings: st, Active: active, Congested: h.congested, Heavy: heavy, Devices: devices,
		RateDown: round1(h.lastRateDn), RateUp: round1(h.lastRateUp), Down: down, Up: up,
	}
}
