package ctrl

import "testing"

func TestClientEndpoint(t *testing.T) {
	ok := map[string]struct {
		host string
		port int
	}{
		"203.0.113.7:51820":     {"203.0.113.7", 51820},
		"[2001:db8::1]:443":     {"2001:db8::1", 443},
		"host.example.com:8443": {"host.example.com", 8443},
	}
	for v, w := range ok {
		h, p, err := ClientEndpoint(v)
		if err != nil || h != w.host || p != w.port {
			t.Errorf("ClientEndpoint(%q): %q %d %v", v, h, p, err)
		}
	}
	for _, v := range []string{"", "203.0.113.7", ":51820", "h:", "h:0", "h:70000", "h:x"} {
		if _, _, err := ClientEndpoint(v); err == nil {
			t.Errorf("ClientEndpoint(%q): ожидали ошибку", v)
		}
	}
}
