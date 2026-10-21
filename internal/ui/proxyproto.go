package ui

// PROXY protocol v1 от ретранслятора (`awg-control relay`): после переноса панели старый
// сервер пересылает на неё подписку, хаб нод и панель. Без заголовка все клиенты пришли бы с
// его IP и делили бы на всех лимиты по адресу (неудачные запросы, регистрации, вход). Заголовку
// верим только от адресов из RELAY_FROM; от остальных он — просто байты соединения.

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const proxyHeaderTimeout = 5 * time.Second

// relayFrom — RELAY_FROM: IP ретрансляторов через запятую.
func relayFrom() map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("RELAY_FROM"), ",") {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			out[ip.String()] = true
		}
	}
	return out
}

// listen — net.Listen("tcp", :port), а при заданном RELAY_FROM — с разбором PROXY v1.
func listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	if trusted := relayFrom(); len(trusted) > 0 {
		return newProxyListener(ln, trusted), nil
	}
	return ln, nil
}

type accepted struct {
	c   net.Conn
	err error
}

// proxyListener читает заголовок в своей горутине на соединение: медленный ретранслятор не
// держит Accept для остальных.
type proxyListener struct {
	net.Listener
	trusted map[string]bool
	ch      chan accepted
}

func newProxyListener(ln net.Listener, trusted map[string]bool) *proxyListener {
	l := &proxyListener{Listener: ln, trusted: trusted, ch: make(chan accepted)}
	go l.loop()
	return l
}

func (l *proxyListener) loop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.ch <- accepted{nil, err}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		if ip := net.ParseIP(host); ip == nil || !l.trusted[ip.String()] {
			l.ch <- accepted{c, nil}
			continue
		}
		go func() {
			if pc := readProxyHeader(c); pc != nil {
				l.ch <- accepted{pc, nil}
			}
		}()
	}
}

func (l *proxyListener) Accept() (net.Conn, error) {
	a := <-l.ch
	return a.c, a.err
}

type proxyConn struct {
	net.Conn
	r      *bufio.Reader
	remote net.Addr
}

func (c *proxyConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *proxyConn) RemoteAddr() net.Addr       { return c.remote }

// readProxyHeader — соединение без заголовка остаётся как есть (с ретранслятора может прийти и
// обычный клиент); битый заголовок — закрываем. nil — соединение закрыто.
func readProxyHeader(c net.Conn) net.Conn {
	c.SetReadDeadline(time.Now().Add(proxyHeaderTimeout))
	defer c.SetReadDeadline(time.Time{})
	r := bufio.NewReaderSize(c, 256)
	pc := &proxyConn{Conn: c, r: r, remote: c.RemoteAddr()}
	if sig, err := r.Peek(6); err != nil || string(sig) != "PROXY " {
		if err != nil && len(sig) == 0 {
			c.Close()
			return nil
		}
		return pc
	}
	// Строка v1 — не длиннее 107 байт вместе с CRLF.
	line, err := r.ReadSlice('\n')
	if err != nil || len(line) > 107 || !strings.HasSuffix(string(line), "\r\n") {
		c.Close()
		return nil
	}
	if addr, ok := parseProxyV1(strings.TrimSuffix(string(line), "\r\n")); ok {
		if addr != nil {
			pc.remote = addr
		}
		return pc
	}
	c.Close()
	return nil
}

// parseProxyV1 — «PROXY TCP4|TCP6 src dst sport dport» или «PROXY UNKNOWN …» (addr == nil).
func parseProxyV1(line string) (*net.TCPAddr, bool) {
	f := strings.Split(line, " ")
	if len(f) >= 2 && f[0] == "PROXY" && f[1] == "UNKNOWN" {
		return nil, true
	}
	if len(f) != 6 || f[0] != "PROXY" || f[1] != "TCP4" && f[1] != "TCP6" {
		return nil, false
	}
	ip := net.ParseIP(f[2])
	if ip == nil || net.ParseIP(f[3]) == nil || (ip.To4() != nil) != (f[1] == "TCP4") {
		return nil, false
	}
	port, err := strconv.Atoi(f[4])
	if err != nil || port < 0 || port > 65535 {
		return nil, false
	}
	if p, err := strconv.Atoi(f[5]); err != nil || p < 0 || p > 65535 {
		return nil, false
	}
	return &net.TCPAddr{IP: ip, Port: port}, true
}
