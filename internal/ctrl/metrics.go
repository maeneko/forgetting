package ctrl

import (
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// Метрики для Prometheus. awg-ctrl отдаёт JSON, а текстовый формат собирает awg-ui: на ноде
// агент пропускает только /api/*, и один формат на ядре и нодах проще сверять. Кэш на 5 с —
// несколько скрейперов не должны гонять `awg show` чаще.
const metricsTTL = 5 * time.Second

type metricsCache struct {
	mu   sync.Mutex
	at   time.Time
	data any
}

type metricsPeer struct {
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	IP       string  `json:"ip"`
	Online   bool    `json:"online"`
	Rx       float64 `json:"rx"`
	Tx       float64 `json:"tx"`
	Priority string  `json:"priority"`
	Torrent  bool    `json:"torrent"`
}

type metricsHost struct {
	CPU   jsjson.Object `json:"cpu"` // порядок режимов как в /proc/stat (и в Node-версии)
	Cores int           `json:"cores"`
	Load  []float64     `json:"load"`
	Mem   struct {
		Total     float64 `json:"total"`
		Available float64 `json:"available"`
	} `json:"mem"`
}

type linkStats struct {
	RxBytes   float64 `json:"rx_bytes"`
	TxBytes   float64 `json:"tx_bytes"`
	RxPackets float64 `json:"rx_packets"`
	TxPackets float64 `json:"tx_packets"`
	RxDropped float64 `json:"rx_dropped"`
	TxDropped float64 `json:"tx_dropped"`
	RxErrors  float64 `json:"rx_errors"`
	TxErrors  float64 `json:"tx_errors"`
}

type metricsBody struct {
	Up    bool   `json:"up"`
	Gen   string `json:"gen"`
	Peers struct {
		Total  int `json:"total"`
		Online int `json:"online"`
	} `json:"peers"`
	RxBytes float64       `json:"rx_bytes"`
	TxBytes float64       `json:"tx_bytes"`
	Link    linkStats     `json:"link"`
	Shaping ShapingStatus `json:"shaping"`
	Torrent TorrentStatus `json:"torrent"`
	Host    *metricsHost  `json:"host"`
	List    []metricsPeer `json:"list"`
}

func (s *Server) linkCounter(name string) float64 {
	b, err := os.ReadFile("/sys/class/net/" + s.iface + "/statistics/" + name)
	if err != nil {
		return 0
	}
	return orZero(jsNumber(string(b)))
}

var cpuModes = []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

// readHost: нагрузка хоста из /proc. Счётчики CPU кумулятивные (секунды по режимам, как у
// node_exporter): долю считает Prometheus через rate(). Память — MemAvailable, а не MemFree:
// кэш ядра свободен.
func readHost() *metricsHost {
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	first := strings.Fields(strings.SplitN(string(stat), "\n", 2)[0])
	if len(first) < 2 {
		return nil
	}
	h := &metricsHost{Cores: runtime.NumCPU()}
	for i, m := range cpuModes {
		v := 0.0
		if i+1 < len(first) {
			v = orZero(jsNumber(first[i+1]))
		}
		h.CPU = append(h.CPU, jsjson.KV{Key: m, Val: v / 100}) // USER_HZ = 100 на всех обычных ядрах
	}
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	kb := func(k string) float64 {
		m := regexp.MustCompile(`(?m)^` + k + `:\s+(\d+) kB`).FindStringSubmatch(string(mem))
		if m == nil {
			return 0
		}
		f, _ := strconv.ParseFloat(m[1], 64)
		return f * 1024
	}
	h.Mem.Total, h.Mem.Available = kb("MemTotal"), kb("MemAvailable")
	if la, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(la))
		for i := 0; i < 3 && i < len(f); i++ {
			h.Load = append(h.Load, orZero(jsNumber(f[i])))
		}
	}
	return h
}

func (s *Server) collectMetrics() (*metricsBody, error) {
	st := s.interfaceStatus()
	stats := map[string]peerStat{}
	if st.Up {
		stats = s.getPeersData()
	}
	rows, err := s.db.Query("SELECT name, ip, pub_key FROM users WHERE vpn_key != ''")
	if err != nil {
		return nil, err
	}
	b := &metricsBody{Up: st.Up, Gen: s.cfg().Gen, List: []metricsPeer{}}
	for rows.Next() {
		var n, ip, pub string
		if err := rows.Scan(&n, &ip, &pub); err != nil {
			rows.Close()
			return nil, err
		}
		ps := stats[pub]
		b.List = append(b.List, metricsPeer{"user", n, ip, ps.Online, orZero(ps.Rx), orZero(ps.Tx), s.shaping.PriorityOf(ip), s.torrent.Seen(ip)})
	}
	rows.Close()
	devs, err := s.peersAll()
	if err != nil {
		return nil, err
	}
	for _, p := range devs {
		ps := stats[p.PubKey]
		b.List = append(b.List, metricsPeer{"device", "", p.IP, ps.Online, orZero(ps.Rx), orZero(ps.Tx), s.shaping.PriorityOf(p.IP), s.torrent.Seen(p.IP)})
	}
	b.Peers.Total = st.Peers
	for _, ps := range stats {
		if ps.Online {
			b.Peers.Online++
		}
		b.RxBytes += orZero(ps.Rx)
		b.TxBytes += orZero(ps.Tx)
	}
	b.Link = linkStats{
		s.linkCounter("rx_bytes"), s.linkCounter("tx_bytes"), s.linkCounter("rx_packets"), s.linkCounter("tx_packets"),
		s.linkCounter("rx_dropped"), s.linkCounter("tx_dropped"), s.linkCounter("rx_errors"), s.linkCounter("tx_errors"),
	}
	b.Shaping = s.shaping.Status()
	b.Torrent = s.torrent.Status()
	b.Host = readHost()
	return b, nil
}

// metricsCached отдаёт кэш на metricsTTL; ошибку не кэшируем.
func (s *Server) metricsCached() (any, error) {
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data != nil && time.Since(m.at) <= metricsTTL {
		return m.data, nil
	}
	d, err := s.collectMetrics()
	if err != nil {
		m.data = nil
		return nil, err
	}
	m.at, m.data = time.Now(), d
	return d, nil
}
