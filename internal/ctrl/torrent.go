package ctrl

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// Блокировка BitTorrent — то же, что sniffing + blackhole в Xray (3x-ui), только в ядре.
//
// Решение принимается по первым байтам и один раз на соединение: совпала сигнатура — соединение
// получает бит в ct mark и дальше режется целиком, в обе стороны. Смотрим только то, что
// клиенты шлют в интернет (iifname awg1), до NAT, поэтому источник — IP клиента в туннеле.
//
// Ловится: DHT (запросы и ответы), UDP-трекеры, начало uTP-соединения, а при поддержке @ih в
// nft/ядре — ещё открытое TCP-рукопожатие и HTTP-анонс. Не ловится шифрованное рукопожатие
// (MSE/PE) и HTTPS-трекеры — так же, как у Xray. Но без DHT и трекеров клиенту негде брать пиров.
//
// Как и шейпинг, ничего не пишет в awg1.conf: таблица ставится из процесса и идемпотентно.
// nft-таблица не привязана к интерфейсу (iifname — по имени), поэтому переживает его рестарт;
// после перезагрузки сервера её возвращает Apply на старте.

type TorrentSettings struct {
	Enabled bool `json:"enabled"`
}

type TorrentPeer struct {
	IP   string `json:"ip"`
	Name string `json:"name"` // имя пользователя; у sen://-устройства — пусто
}

type TorrentStatus struct {
	Settings TorrentSettings `json:"settings"`
	Active   bool            `json:"active"` // таблица реально стоит
	TCP      bool            `json:"tcp"`    // проверяется и TCP (нужен @ih: nftables ≥ 1.0, свежее ядро)
	Hits     float64         `json:"hits"`   // заблокированных попыток с последнего применения
	Seen     []TorrentPeer   `json:"seen"`   // кто пытался за последние torrentSeenTTL
	Error    string          `json:"error,omitempty"`
}

const (
	torrentTable   = "awg_torrent"
	torrentMark    = "0x00100000" // свой бит ct mark: чужие биты не трогаем
	torrentSeenTTL = "10m"
	torrentCacheS  = 5 * time.Second // /api/users/stats опрашивается часто — nft не дёргаем на каждый
)

// Сигнатуры. @th,64 — начало данных UDP (заголовок 8 байт); @ih,0 — начало данных TCP с учётом опций.
const torrentUDPRules = `
        udp dport 53 return
        @th,64,96 0x64313a6164323a696432303a jump block
        @th,64,96 0x64313a7264323a696432303a jump block
        @th,64,96 0x000004172710198000000000 jump block
        ct state new udp length >= 28 @th,64,8 0x41 @th,72,8 < 3 @th,128,32 0 jump block`

// DHT-запрос «d1:ad2:id20:» и ответ «d1:rd2:id20:» (bencode, ключи отсортированы — «a»/«r» первые);
// UDP-трекер: protocol_id 0x41727101980 + action connect (0); uTP SYN: тип 4, версия 1 (0x41),
// расширение 0–2, timestamp_difference 0 — только в новом потоке, где QUIC начинается с длинного
// заголовка (0xC0+), и не на 53-м порту, чтобы случайный ID DNS-запроса не сошёлся.

const torrentTCPRules = `
        @ih,0,128 0x13426974546f7272656e742070726f74 jump block
        @ih,0,112 0x474554202f616e6e6f756e63653f jump block`

// «\x13BitTorrent prot» — открытое рукопожатие пиров; «GET /announce?» — HTTP-трекер.

type Torrent struct {
	s *Server

	opMu sync.Mutex // сериализует nft

	mu      sync.Mutex
	enabled bool // копия настройки в памяти: cached() зовут внутри циклов по строкам users, а
	//                   соединение с SQLite одно — запрос в базу оттуда ждал бы его вечно
	tcp       bool
	lastError string
	cacheAt   time.Time
	cache     map[string]bool
	cacheHits float64
}

func newTorrent(s *Server) *Torrent {
	t := &Torrent{s: s}
	t.enabled = t.Settings().Enabled
	return t
}

func (t *Torrent) Settings() TorrentSettings {
	return TorrentSettings{Enabled: t.s.getCfg("torrentBlock", "0") == "1"}
}

func (t *Torrent) script(tcp bool) string {
	iface := t.s.iface
	var b strings.Builder
	// add+delete в одной транзакции: таблица пересоздаётся атомарно, без окна без правил.
	b.WriteString("\nadd table inet " + torrentTable + "\ndelete table inet " + torrentTable + "\ntable inet " + torrentTable + " {\n")
	b.WriteString("    counter hits {\n    }\n")
	b.WriteString("    set seen {\n        type ipv4_addr\n        flags dynamic, timeout\n        timeout " + torrentSeenTTL + "\n    }\n")
	b.WriteString("    chain block {\n        ct mark set ct mark or " + torrentMark + "\n        update @seen { ip saddr }\n        counter name \"hits\"\n        drop\n    }\n")
	b.WriteString("    chain udp_sig {" + torrentUDPRules + "\n    }\n")
	if tcp {
		b.WriteString("    chain tcp_sig {" + torrentTCPRules + "\n    }\n")
	}
	b.WriteString("    chain forward {\n        type filter hook forward priority -10; policy accept;\n")
	b.WriteString("        ct mark and " + torrentMark + " == " + torrentMark + " drop\n")
	b.WriteString("        iifname \"" + iface + "\" meta nfproto ipv4 meta l4proto udp jump udp_sig\n")
	if tcp {
		b.WriteString("        iifname \"" + iface + "\" meta nfproto ipv4 meta l4proto tcp jump tcp_sig\n")
	}
	b.WriteString("    }\n}\n")
	return b.String()
}

func (t *Torrent) applyNow() error {
	if !t.Settings().Enabled {
		t.removeNow()
		return nil
	}
	// Сначала полный набор; старый nft/ядро не знают @ih — тогда без TCP, но UDP (DHT, трекеры,
	// uTP) всё равно режем: это и есть основная часть.
	tcp := true
	err := shExecStdin("nft", []string{"-f", "-"}, t.script(true))
	if err != nil {
		tcp = false
		if err2 := shExecStdin("nft", []string{"-f", "-"}, t.script(false)); err2 != nil {
			return err2
		}
		t.s.log.Warn("torrent: TCP-сигнатуры не поддерживаются, блокируем только UDP", "nft", err.Error())
	}
	t.mu.Lock()
	t.tcp, t.lastError, t.cache = tcp, "", nil
	t.mu.Unlock()
	t.s.log.Info("torrent block applied", "tcp", tcp)
	return nil
}

func (t *Torrent) removeNow() {
	shExec("nft", "delete", "table", "inet", torrentTable)
	t.mu.Lock()
	t.tcp, t.cache = false, nil
	t.mu.Unlock()
}

func (t *Torrent) noteError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := err.Error(); m != t.lastError {
		t.s.log.Warn("torrent: " + m)
		t.lastError = m
	}
}

// Apply не бросает: без nft awg-ctrl должен работать дальше (как шейпинг).
func (t *Torrent) Apply() {
	t.opMu.Lock()
	err := t.applyNow()
	t.opMu.Unlock()
	if err != nil {
		t.noteError(err)
	}
}

// Update сохраняет и применяет; ошибку отдаёт вызывающему — оператору нужна причина.
func (t *Torrent) Update(st TorrentSettings) error {
	v := "0"
	if st.Enabled {
		v = "1"
	}
	t.s.setCfg("torrentBlock", v)
	t.mu.Lock()
	t.enabled, t.cache = st.Enabled, nil
	t.mu.Unlock()
	t.opMu.Lock()
	err := t.applyNow()
	t.opMu.Unlock()
	if err != nil {
		t.mu.Lock()
		t.lastError = err.Error()
		t.mu.Unlock()
	}
	return err
}

// read — набор seen и счётчик из ядра. ok=false — таблицы нет.
func (t *Torrent) read() (seen map[string]bool, hits float64, ok bool) {
	out, err := shExec("nft", "-j", "list", "set", "inet", torrentTable, "seen")
	if err != nil {
		return nil, 0, false
	}
	seen = map[string]bool{}
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
			Counter *struct {
				Packets float64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if json.Unmarshal([]byte(out), &doc) == nil {
		for _, it := range doc.Nftables {
			if it.Set == nil {
				continue
			}
			// Элемент с таймаутом — {"elem": {"val": "10.9.0.2", ...}}, без него — просто строка.
			for _, raw := range it.Set.Elem {
				var ip string
				if json.Unmarshal(raw, &ip) != nil {
					var e struct {
						Elem struct {
							Val string `json:"val"`
						} `json:"elem"`
					}
					if json.Unmarshal(raw, &e) == nil {
						ip = e.Elem.Val
					}
				}
				if ip != "" {
					seen[ip] = true
				}
			}
		}
	}
	if out, err := shExec("nft", "-j", "list", "counter", "inet", torrentTable, "hits"); err == nil {
		doc.Nftables = nil
		if json.Unmarshal([]byte(out), &doc) == nil {
			for _, it := range doc.Nftables {
				if it.Counter != nil {
					hits = it.Counter.Packets
				}
			}
		}
	}
	return seen, hits, true
}

// cached не трогает базу (только память и nft) — его можно звать где угодно.
func (t *Torrent) cached() (map[string]bool, float64, bool) {
	t.mu.Lock()
	if !t.enabled {
		t.mu.Unlock()
		return nil, 0, false
	}
	if t.cache != nil && time.Since(t.cacheAt) < torrentCacheS {
		c, h := t.cache, t.cacheHits
		t.mu.Unlock()
		return c, h, true
	}
	t.mu.Unlock()
	seen, hits, ok := t.read()
	if !ok {
		return nil, 0, false
	}
	t.mu.Lock()
	t.cache, t.cacheHits, t.cacheAt = seen, hits, time.Now()
	t.mu.Unlock()
	return seen, hits, true
}

// SeenSet — кто пытался открыть торрент-соединение за последние torrentSeenTTL. Берите его до
// запроса к базе, а не внутри цикла по строкам: так на открытом курсоре не висит вызов nft.
func (t *Torrent) SeenSet() map[string]bool {
	seen, _, _ := t.cached()
	out := make(map[string]bool, len(seen))
	for ip := range seen {
		out[ip] = true
	}
	return out
}

// SeenList — IP с меткой «торренты», по порядку; шейпинг держит их в фоновом классе.
func (t *Torrent) SeenList() []string {
	seen, _, _ := t.cached()
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func (t *Torrent) Status() TorrentStatus {
	st := TorrentStatus{Settings: t.Settings(), Seen: []TorrentPeer{}}
	seen, hits, active := t.cached()
	t.mu.Lock()
	st.TCP, st.Error = t.tcp && active, t.lastError
	t.mu.Unlock()
	st.Active, st.Hits = active, hits
	if len(seen) == 0 {
		return st
	}
	names := map[string]string{}
	if rows, err := t.s.db.Query("SELECT name, ip FROM users"); err == nil {
		for rows.Next() {
			var n, ip string
			if rows.Scan(&n, &ip) == nil {
				names[ip] = n
			}
		}
		rows.Close()
	}
	for ip := range seen {
		st.Seen = append(st.Seen, TorrentPeer{IP: ip, Name: names[ip]})
	}
	sort.Slice(st.Seen, func(i, j int) bool { return st.Seen[i].IP < st.Seen[j].IP })
	return st
}
