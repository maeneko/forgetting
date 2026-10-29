// Package ctrl — awg-ctrl: привилегированный REST API, который управляет интерфейсом
// AmneziaWG (`awg`, `awg-quick`, `ip`, `tc`, `nft`) и базой пользователей users.db.
// Слушает только 127.0.0.1; снаружи до него добирается лишь awg-ui (и агент ноды, и CLI)
// с подписанным Ed25519-токеном.
package ctrl

import (
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/logx"
)

// Config — то, что в Node-версии лежало в CONFIG. Параметры и поколение меняются при
// restartAwg/переходе на 3.1 — читать через Server.cfg().
type Config struct {
	ServerIP   string
	ServerPort int
	ServerName string
	DNS1, DNS2 string
	MTU        int
	Keepalive  string
	Subnet     string
	Params     Params
	Gen        string
	AwgModule  string
	AwgTools   string
}

type Server struct {
	iface             string
	confDir           string
	dbPath            string
	serverPrivKeyFile string
	port              int

	db  *sql.DB
	log *logx.Logger
	pub ed25519.PublicKey

	// mu делает последовательности «БД + awg + awg1.conf» неделимыми. В Node весь код между
	// await атомарен (один поток); в Go без этого два запроса перемешали бы шаги.
	mu    sync.Mutex
	cfgMu sync.RWMutex
	c     Config

	shaping *Shaping
	torrent *Torrent
	metrics metricsCache
	monitor monitorJob
	cliPath string
	cliLog  string
}

func (s *Server) cfg() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.c
}

func (s *Server) keyEnv() keyEnv {
	c := s.cfg()
	return keyEnv{P: c.Params, ServerIP: c.ServerIP, ServerPort: c.ServerPort, ServerName: c.ServerName,
		DNS1: c.DNS1, DNS2: c.DNS2, MTU: c.MTU, Keepalive: c.Keepalive, Gen: c.Gen, Subnet: c.Subnet}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getLocalIP() string {
	r := runCmd("ip", "route", "show", "default")
	m := regexp.MustCompile(`dev\s+(\S+)`).FindStringSubmatch(r.Stdout)
	if m == nil {
		return ""
	}
	a := runCmd("ip", "addr", "show", m[1])
	if m := regexp.MustCompile(`inet\s+([\d.]+)`).FindStringSubmatch(a.Stdout); m != nil {
		return m[1]
	}
	return ""
}

// New открывает базу и ключи, читает параметры из awg1.conf. Не слушает порт.
func New() (*Server, error) {
	s := &Server{
		iface:             "awg1",
		confDir:           envOr("AWG_CONF_DIR", "/etc/amnezia/amneziawg"),
		serverPrivKeyFile: envOr("AWG_SERVER_PRIV_KEY_FILE", "/etc/amnezia/server_private.key"),
		cliPath:           envOr("AWG_CLI", "/usr/local/bin/awg-ctrl"),
	}
	s.dbPath = envOr("AWG_DB_PATH", filepath.Join(s.confDir, "users.db"))
	port, _ := strconv.Atoi(os.Getenv("PORT"))
	if port == 0 {
		port = 3005
	}
	s.port = port
	cwd, _ := os.Getwd()
	s.log = logx.New(os.Stdout, filepath.Join(cwd, "logs"))
	s.cliLog = envOr("MONITOR_LOG", filepath.Join(filepath.Dir(cwd), "logs", "monitor-job.log"))

	pubFile := envOr("INTERNAL_AUTH_PUB_FILE", "/etc/amnezia/amneziawg/internal_auth_public.key")
	pub, err := iauth.LoadPublic(pubFile)
	if err != nil {
		s.log.Error("FATAL: публичный ключ внутренней авторизации не найден: " + pubFile)
		return nil, fmt.Errorf("FATAL: публичный ключ внутренней авторизации не найден: %s", pubFile)
	}
	s.pub = pub

	db, err := openDB(s.dbPath)
	if err != nil {
		return nil, err
	}
	s.db = db

	// Адрес/порт/имя: БД, затем env, затем (для адреса) маршрут по умолчанию. Как в Node,
	// запасной адрес вычисляется всегда, даже если в БД уже есть значение.
	fallbackIP := ""
	if v, ok := os.LookupEnv("SERVER_IP"); ok {
		fallbackIP = v
	} else {
		fallbackIP = getLocalIP()
	}
	ip := s.getCfg("serverIp", fallbackIP)
	portStr := s.getCfg("serverPort", envOr("SERVER_PORT", "51820"))
	name := s.getCfg("serverName", envOr("SERVER_NAME", "VPN"))
	if ip == "" {
		return nil, fmt.Errorf("SERVER_IP не задан — передай через env при первом запуске")
	}
	s.setCfg("serverIp", ip)
	s.setCfg("serverPort", portStr)
	s.setCfg("serverName", name)
	sp, _ := strconv.Atoi(portStr)

	params := ReadParams(s.confPath(), func(m string) { s.log.Warn(m) })
	params.I1 = s.ensureI1() // I1 — из БД, не из conf (см. i1.go)
	gen := params.Gen()
	module, tools := readVersions()
	s.c = Config{
		ServerIP: ip, ServerPort: sp, ServerName: name,
		DNS1: "1.1.1.1", DNS2: "1.0.0.1", MTU: 1376, Subnet: "10.9",
		Keepalive: keepaliveByGen[gen], Params: params, Gen: gen,
		AwgModule: module, AwgTools: tools,
	}
	s.log.Info("awg params loaded from conf", "Jc", params.Jc, "H1", params.H1, "gen", gen)
	s.shaping = newShaping(s)
	s.torrent = newTorrent(s)
	return s, nil
}

// Run слушает 127.0.0.1:PORT и выполняет стартовую последовательность Node-версии:
// listen → ensureInterfaceUp → syncPeers → шейпинг. Нет интерфейса — завершаемся с ошибкой:
// health-gate CLI опирается на это (без поднятого awg1 awg-ctrl не нужен).
func (s *Server) Run() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 15 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	c := s.cfg()
	s.log.Info("Server started", "port", s.port, "host", "127.0.0.1", "serverName", c.ServerName, "serverIp", c.ServerIP)

	if err := s.ensureInterfaceUp(); err != nil {
		return err
	}
	if err := s.syncPeers(); err != nil {
		return err
	}
	// Обязательно и на старте: при загрузке интерфейс поднимает systemd (ExecStartPre),
	// ни один из путей awg-quick up внутри awg-ctrl тогда не срабатывает.
	go s.shaping.Apply()
	s.shaping.Start()
	go s.torrent.Apply() // nft-таблицы после перезагрузки нет — ставим заново
	return <-errc
}
