package relay

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseRoutes(t *testing.T) {
	r, err := ParseRoutes(" 40123=203.0.113.7:40123, 8443=[2001:db8::1]:9000 ,")
	if err != nil || len(r) != 2 || r[0] != (Route{40123, "203.0.113.7:40123"}) || r[1] != (Route{8443, "[2001:db8::1]:9000"}) {
		t.Fatalf("ParseRoutes: %v %v", r, err)
	}
	for _, bad := range []string{"", ",", "40123", "0=h:1", "70000=h:1", "1=h", "1=:5", "1=h:0", "x=h:1"} {
		if _, err := ParseRoutes(bad); err == nil {
			t.Errorf("ParseRoutes(%q): ожидали ошибку", bad)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Поток идёт насквозь в обе стороны, первой строкой у панели — PROXY с адресом клиента.
func TestRelayForwardsWithProxyHeader(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	got := make(chan string, 2)
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		hdr, _ := r.ReadString('\n')
		got <- hdr
		body, _ := io.ReadAll(r)
		got <- string(body)
		io.WriteString(c, "pong")
	}()

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, []Route{{port, target.Addr().String()}}, func(string, ...any) {}, func(f string, a ...any) { t.Logf(f, a...) })
	}()

	var cl net.Conn
	for range 50 {
		if cl, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	io.WriteString(cl, "ping")
	cl.(*net.TCPConn).CloseWrite()

	hdr := <-got
	src := cl.LocalAddr().(*net.TCPAddr)
	if want := "PROXY TCP4 127.0.0.1 127.0.0.1 " + strconv.Itoa(src.Port) + " " + strconv.Itoa(port) + "\r\n"; hdr != want {
		t.Errorf("заголовок %q, ожидали %q", hdr, want)
	}
	if body := <-got; body != "ping" {
		t.Errorf("до панели дошло %q", body)
	}
	cl.SetReadDeadline(time.Now().Add(3 * time.Second))
	if back, _ := io.ReadAll(cl); string(back) != "pong" {
		t.Errorf("обратно пришло %q", back)
	}

	// Занятый порт — ошибка сразу: значит, его держит прежняя панель.
	busy, _ := net.Listen("tcp", ":0")
	defer busy.Close()
	if err := Run(context.Background(), []Route{{busy.Addr().(*net.TCPAddr).Port, "127.0.0.1:1"}}, func(string, ...any) {}, t.Logf); err == nil ||
		!strings.Contains(err.Error(), "порт") {
		t.Errorf("занятый порт: %v", err)
	}
	cancel()
	<-errs
}
