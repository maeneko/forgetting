// Расширение пакета relay для роли «релей» (awgrelay://): пересылка UDP-трафика VPN-клиентов
// к ноде по IPv6 и прозрачного TCP (без PROXY-заголовка — им распоряжается режим migrate
// через RELAY и RELAY_FROM).
//
// Здесь же общий Main роли `awg-control relay`: собирает маршруты из трёх переменных
// окружения — RELAY (TCP + PROXY, перенесённая панель), RELAY_TCP (TCP без заголовка) и
// RELAY_UDP (VPN-трафик).
package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RouteX — маршрут с сетью и флагом PROXY v1 (только TCP).
type RouteX struct {
	Route
	Net   string // "tcp" | "udp"
	Proxy bool
}

// ParseTCPRoutes — RELAY_TCP: «порт=хост:порт» через запятую; TCP без PROXY-заголовка.
func ParseTCPRoutes(s string) ([]RouteX, error) {
	raw, err := ParseRoutes(s)
	if err != nil {
		return nil, err
	}
	out := make([]RouteX, len(raw))
	for i, r := range raw {
		out[i] = RouteX{r, "tcp", false}
	}
	return out, nil
}

// ParseUDPRoutes — RELAY_UDP, формат тот же.
func ParseUDPRoutes(s string) ([]RouteX, error) {
	raw, err := ParseRoutes(s)
	if err != nil {
		return nil, err
	}
	out := make([]RouteX, len(raw))
	for i, r := range raw {
		out[i] = RouteX{r, "udp", false}
	}
	return out, nil
}

// RunRoutes — TCP (прозрачно или с PROXY) и UDP-маршруты одним списком. Блокируется до
// отмены ctx. Дубликат локального порта — ошибка сразу: след от опечатки в конфиге. Ошибка
// любого маршрута целиком отменяет запуск: частично запущенный релей хуже не запущенного.
func RunRoutes(ctx context.Context, routes []RouteX, logf, errorf func(string, ...any)) error {
	seen := map[int]string{}
	for _, r := range routes {
		if r.Net != "tcp" && r.Net != "udp" {
			return fmt.Errorf("relay: неизвестная сеть %q", r.Net)
		}
		if other, dup := seen[r.Listen]; dup {
			return fmt.Errorf("relay: порт %d занят двумя маршрутами (%s и %s)", r.Listen, other, r.Net+":"+r.Target)
		}
		seen[r.Listen] = r.Net + ":" + r.Target
	}
	var plain, proxy []Route
	for _, r := range routes {
		if r.Net == "udp" {
			if err := runUDPRoute(ctx, r, logf, errorf); err != nil {
				return err
			}
			continue
		}
		if r.Proxy {
			proxy = append(proxy, r.Route)
		} else {
			plain = append(plain, r.Route)
		}
	}
	// Порты уже проверены запусками слушателей; дальше всё работает до отмены ctx.
	if len(plain) > 0 {
		runPlainTCP(ctx, plain, logf, errorf)
	}
	if len(proxy) > 0 {
		// Run блокируется до отмены: общий ход в нём уже считается завершённым.
		Run(ctx, proxy, logf, errorf)
	}
	<-ctx.Done()
	return ctx.Err()
}

// runPlainTCP — TCP без PROXY v1 (в отличие от serve() режима migrate там не пишется
// заголовок PROXY v1). Ошибка порта возвращается сразу; слушатели закрываются по ctx.
func runPlainTCP(ctx context.Context, routes []Route, logf, errorf func(string, ...any)) error {
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
		logf("relay: tcp :%d → %s", r.Listen, r.Target)
	}
	go func() {
		<-ctx.Done()
		for _, l := range lns {
			l.Close()
		}
	}()
	for i, r := range routes {
		go func(ln net.Listener, target string, port int) {
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
					errorf("relay: tcp :%d: %v", port, err)
					time.Sleep(time.Second)
					continue
				}
				go func() {
					up, err := d.DialContext(ctx, "tcp", target)
					if err != nil {
						errorf("relay: tcp :%d → %s: %v", port, target, err)
						c.Close()
						return
					}
					pipe(c, up)
				}()
			}
		}(lns[i], r.Target, r.Listen)
	}
	return nil
}

// Таймеры UDP: WireGuard сам не рвёт сессию, клиент молчит подолгу (keepalive 25–35 с),
// поэтому «недавняя активность» — пакеты за последнюю минуту. Записи тише минуты чистит свип:
// при следующем пакете откроется новая сессия.
const (
	udpIdle     = 60 * time.Second
	udpSweep    = 10 * time.Second
	udpBuf      = 4 << 20 // SO_RCVBUF/SO_SNDBUF: скорость канала не должна упереться в релей
	udpReadSize = 65535
)

// udpSession — пара «адрес клиента → сокет до цели» и метка последней активности.
type udpSession struct {
	up   *net.UDPConn
	last time.Time
}

// runUDPRoute — сессия «клиент ↔ upstream»: новый адрес клиента — отдельный сокет к цели,
// ответы upstream уходят обратно только ему. Один upstream на клиента, как у conntrack ната.
// Ошибка порта возвращается сразу; остальное живо до отмены ctx.
func runUDPRoute(ctx context.Context, r RouteX, logf, errorf func(string, ...any)) error {
	laddr, err := net.ResolveUDPAddr("udp", ":"+strconv.Itoa(r.Listen))
	if err != nil {
		return fmt.Errorf("relay: udp :%d: %w", r.Listen, err)
	}
	ln, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return fmt.Errorf("relay: udp порт %d: %w", r.Listen, err)
	}
	ln.SetReadBuffer(udpBuf)
	ln.SetWriteBuffer(udpBuf)
	logf("relay: udp :%d → %s", r.Listen, r.Target)

	target, err := net.ResolveUDPAddr("udp", r.Target)
	if err != nil {
		ln.Close()
		return fmt.Errorf("relay: udp :%d → %s: %w", r.Listen, r.Target, err)
	}

	var (
		mu   sync.Mutex
		open = map[string]*udpSession{}
	)
	closeSession := func(key string, s *udpSession) {
		mu.Lock()
		cur, exists := open[key]
		if !exists || cur != s {
			mu.Unlock()
			return
		}
		delete(open, key)
		mu.Unlock()
		s.up.Close()
	}
	// Свип: чистка неактивных сессий и полный останов по отмене ctx.
	go func() {
		tick := time.NewTicker(udpSweep)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				ln.Close()
				mu.Lock()
				for _, s := range open {
					s.up.Close()
				}
				open = map[string]*udpSession{}
				mu.Unlock()
				return
			case <-tick.C:
				mu.Lock()
				deadline := time.Now().Add(-udpIdle)
				for key, s := range open {
					if s.last.Before(deadline) {
						delete(open, key)
						s.up.Close()
					}
				}
				mu.Unlock()
			}
		}
	}()

	go func() {
		buf := make([]byte, udpReadSize)
		for {
			n, client, err := ln.ReadFromUDP(buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				errorf("relay: udp :%d: чтение: %v", r.Listen, err)
				time.Sleep(time.Second)
				continue
			}
			key := client.IP.String() + ":" + strconv.Itoa(client.Port)
			mu.Lock()
			s := open[key]
			if s != nil {
				s.last = time.Now()
			}
			mu.Unlock()
			if s == nil {
				up, derr := net.DialUDP("udp", nil, target)
				if derr != nil {
					errorf("relay: udp :%d → %s: %v", r.Listen, r.Target, derr)
					continue
				}
				up.SetReadBuffer(udpBuf)
				up.SetWriteBuffer(udpBuf)
				ns := &udpSession{up: up, last: time.Now()}
				mu.Lock()
				if cur, existed := open[key]; existed {
					// Гонка двух пакетов одного клиента: оставляем первую сессию.
					ns = cur
					ns.last = time.Now()
					up.Close()
				} else {
					open[key] = ns
					go pumpUDP(ln, up, client, closeSession, key, ns)
				}
				mu.Unlock()
				s = ns
			}
			if _, werr := s.up.Write(buf[:n]); werr != nil {
				errorf("relay: udp :%d → %s: запись: %v", r.Listen, r.Target, werr)
				closeSession(key, s)
			}
		}
	}()
	return nil
}

// pumpUDP — ответы upstream → конкретному клиенту. Ошибка чтения — сессия мертва.
func pumpUDP(ln *net.UDPConn, up *net.UDPConn, client *net.UDPAddr,
	closeSession func(string, *udpSession), key string, s *udpSession) {
	buf := make([]byte, udpReadSize)
	for {
		n, err := up.Read(buf)
		if err != nil {
			closeSession(key, s)
			return
		}
		if _, err := ln.WriteToUDP(buf[:n], client); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				closeSession(key, s)
				return
			}
		}
	}
}

// Main — роль `awg-control relay`: маршруты из RELAY (TCP+PROXY, прежняя панель),
// RELAY_TCP (TCP прозрачно) и RELAY_UDP (VPN-трафик). Хотя бы один список должен быть задан.
func Main(ctx context.Context) error {
	var routes []RouteX
	if s := strings.TrimSpace(os.Getenv("RELAY")); s != "" {
		raw, err := ParseRoutes(s)
		if err != nil {
			return err
		}
		for _, r := range raw {
			routes = append(routes, RouteX{r, "tcp", true})
		}
	}
	for _, item := range []struct {
		env   string
		parse func(string) ([]RouteX, error)
	}{
		{"RELAY_TCP", ParseTCPRoutes},
		{"RELAY_UDP", ParseUDPRoutes},
	} {
		if s := strings.TrimSpace(os.Getenv(item.env)); s != "" {
			list, err := item.parse(s)
			if err != nil {
				return err
			}
			routes = append(routes, list...)
		}
	}
	if len(routes) == 0 {
		return errors.New("relay: не задан ни RELAY, ни RELAY_TCP, ни RELAY_UDP — пересылать нечего")
	}
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stdout, f+"\n", a...) }
	errorf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	return RunRoutes(ctx, routes, logf, errorf)
}
