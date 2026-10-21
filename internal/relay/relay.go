// Package relay — ретранслятор TCP на сервере, с которого переехала панель (`awg-control
// migrate`). В выданных ссылках sen://, у подключённых нод и у клиентов внешнего API остался
// старый адрес: relay слушает прежние порты подписки, хаба нод и панели и пересылает соединения
// на новую панель как есть — TLS идёт насквозь, ключи и сертификаты у панели прежние. Перед
// данными идёт заголовок PROXY v1 с адресом клиента: панель верит ему только от RELAY_FROM.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dialTimeout = 10 * time.Second

// Route — одна пересылка: локальный порт → адрес новой панели.
type Route struct {
	Listen int
	Target string // host:port
}

// ParseRoutes — RELAY: «порт=хост:порт» через запятую.
func ParseRoutes(s string) ([]Route, error) {
	var out []Route
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		l, t, ok := strings.Cut(part, "=")
		port, err := strconv.Atoi(l)
		if !ok || err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("relay: неверная пересылка %q (нужно порт=хост:порт)", part)
		}
		host, tp, err := net.SplitHostPort(t)
		if n, perr := strconv.Atoi(tp); err != nil || host == "" || perr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("relay: неверный адрес %q", t)
		}
		out = append(out, Route{port, t})
	}
	if len(out) == 0 {
		return nil, errors.New("relay: RELAY пуст — пересылать нечего")
	}
	return out, nil
}

// proxyHeader — строка PROXY v1 для соединения c.
func proxyHeader(c net.Conn) string {
	src, ok1 := c.RemoteAddr().(*net.TCPAddr)
	dst, ok2 := c.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return "PROXY UNKNOWN\r\n"
	}
	sip, dip := src.IP, dst.IP
	fam := "TCP6"
	if s4, d4 := sip.To4(), dip.To4(); s4 != nil && d4 != nil {
		fam, sip, dip = "TCP4", s4, d4
	} else if s4 != nil || d4 != nil {
		// Смешанные семейства (v4-mapped на v6-сокете и т. п.) — оба в v6.
		sip, dip = sip.To16(), dip.To16()
	}
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", fam, sip, dip, src.Port, dst.Port)
}

// pipe — копирование в обе стороны; конец одной стороны закрывает запись в другую.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

func serve(ctx context.Context, ln net.Listener, r Route, errorf func(string, ...any)) {
	var d net.Dialer
	d.Timeout = dialTimeout
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			errorf("relay: :%d: %v", r.Listen, err)
			time.Sleep(time.Second)
			continue
		}
		go func() {
			up, err := d.DialContext(ctx, "tcp", r.Target)
			if err != nil {
				errorf("relay: :%d → %s: %v", r.Listen, r.Target, err)
				c.Close()
				return
			}
			if _, err := io.WriteString(up, proxyHeader(c)); err != nil {
				c.Close()
				up.Close()
				return
			}
			pipe(c, up)
		}()
	}
}

// Run слушает все порты и пересылает до отмены ctx. Порт, который не открылся, — ошибка сразу:
// значит, его ещё держит прежняя панель.
func Run(ctx context.Context, routes []Route, logf, errorf func(string, ...any)) error {
	var lns []net.Listener
	for _, r := range routes {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(r.Listen))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("relay: порт %d: %w", r.Listen, err)
		}
		lns = append(lns, ln)
		logf("relay: :%d → %s", r.Listen, r.Target)
	}
	for i, r := range routes {
		go serve(ctx, lns[i], r, errorf)
	}
	<-ctx.Done()
	for _, l := range lns {
		l.Close()
	}
	return ctx.Err()
}

// Main — роль `awg-control relay`: пересылки из RELAY.
func Main(ctx context.Context) error {
	routes, err := ParseRoutes(os.Getenv("RELAY"))
	if err != nil {
		return err
	}
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stdout, f+"\n", a...) }
	errorf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	return Run(ctx, routes, logf, errorf)
}
