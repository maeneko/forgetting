package relay

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// udpEcho — эхо-сервер UDP (имитация awg-сервера: ответ адресует кто спросил).
func udpEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, udpReadSize)
		for {
			n, from, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			ln.WriteToUDP([]byte("back:"+string(buf[:n])), from)
		}
	}()
	return ln.LocalAddr().String(), func() { ln.Close() }
}

// Через UDP-релей доходят оба направления, и ответ чужому клиенту не попадает.
func TestUDPForward(t *testing.T) {
	target, stopEcho := udpEcho(t)
	defer stopEcho()

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		errs <- RunRoutes(ctx, []RouteX{{Route{port, target}, "udp", false}}, func(string, ...any) {}, t.Logf)
	}()
	stopped := false
	safeStop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-errs:
		case <-time.After(2 * time.Second):
			t.Error("RunRoutes не завершился")
		}
	}
	defer safeStop()

	// Готовность релея: порт открыт (в остальном тесты дата-гонки так же Trait).
	waitPortOpen(t, port)

	var conns []*net.UDPConn
	for range 2 {
		c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	ask := func(c *net.UDPConn, msg string) string {
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, udpReadSize)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("ответ через релей не пришёл: %v", err)
		}
		return string(buf[:n])
	}
	for range 3 { // несколько пакетов по одной сессии
		if back := ask(conns[0], "ping-1"); back != "back:ping-1" {
			t.Fatalf("клиент 1: %q", back)
		}
	}
	if back := ask(conns[1], "ping-2"); back != "back:ping-2" {
		t.Fatalf("клиент 2: %q", back)
	}
	if back := ask(conns[0], "again"); back != "back:again" {
		t.Fatalf("сессия клиента 1 после пакетов клиента 2: %q", back)
	}
	safeStop()
}

// TCP без PROXY: идёт насквозь, никаких заголовков не появляется (это TCP-половина роли релея).
func TestPlainTCPForward(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 128)
				n, _ := c.Read(buf)
				io.WriteString(c, "plain:"+string(buf[:n]))
				c.Close()
			}(c)
		}
	}()

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunRoutes(ctx, []RouteX{{Route{port, target.Addr().String()}, "tcp", false}}, func(string, ...any) {}, t.Logf)
	time.Sleep(100 * time.Millisecond)

	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hi"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "plain:hi" {
		t.Fatalf("ответ: %q ошибка %v", string(buf[:n]), err)
	}
}

// Дубликат порта между UDP и TCP-маршрутами — ошибка сразу.
func TestRunRoutesDuplicatePort(t *testing.T) {
	err := RunRoutes(context.Background(), []RouteX{
		{Route{9000, "127.0.0.1:1"}, "udp", false},
		{Route{9000, "127.0.0.1:1"}, "tcp", false},
	}, func(string, ...any) {}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "занят") {
		t.Fatalf("ожидали ошибку занятого порта, получили: %v", err)
	}
}

// itoa — strconv.Itoa без лишнего импорта: тесты уже используют net и strings.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}

// waitPortOpen — UDP-сокет нельзя «проверить на открытость» без пакета; ради читабельности
// тестов просто ждём заведомо достаточное время (иначе первый ICMP refused портит замер).
func waitPortOpen(t *testing.T, port int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
}
