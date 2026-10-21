package ui

// Панель за reverse-proxy (nginx и т. п., в корне домена или в подпапке): соединение приходит от
// прокси, а адрес клиента — в X-Forwarded-For / X-Real-IP. Иначе все клиенты были бы одним IP и
// делили бы лимиты по адресу (неудачные входы, регистрации). Заголовкам верим только от прокси:
// loopback (прокси на этой же машине) всегда, остальные адреса и сети — из TRUST_PROXY. От всех
// прочих заголовки игнорируются — подделать свой адрес клиенту нечем.

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// trustedProxies — TRUST_PROXY: IP и сети (CIDR) через запятую; loopback доверен без него.
var trustedProxies = sync.OnceValue(func() []*net.IPNet { return parseTrustProxy(os.Getenv("TRUST_PROXY")) })

func parseTrustProxy(v string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				bits := 128
				if ip.To4() != nil {
					bits = 32
				}
				s = ip.String() + "/" + strconv.Itoa(bits)
			}
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		} else {
			warnf("TRUST_PROXY: «%s» — не IP и не сеть, пропущено", s)
		}
	}
	return out
}

func isTrustedProxy(ip net.IP, nets []*net.IPNet) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP — адрес клиента: от доверенного прокси — из X-Forwarded-For (справа налево, первый
// адрес не из доверенных: левые части клиент мог дописать сам) или X-Real-IP; иначе — сам peer.
func clientIP(peer string, h interface{ Values(string) []string }, nets []*net.IPNet) string {
	ip := net.ParseIP(peer)
	if ip == nil || !isTrustedProxy(ip, nets) {
		return peer
	}
	var hops []string
	for _, v := range h.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	found := ""
	for i := len(hops) - 1; i >= 0; i-- {
		hop := net.ParseIP(strings.TrimSpace(hops[i]))
		if hop == nil {
			break
		}
		found = hop.String()
		if !isTrustedProxy(hop, nets) {
			return found
		}
	}
	if found != "" {
		return found
	}
	for _, v := range h.Values("X-Real-IP") {
		if r := net.ParseIP(strings.TrimSpace(v)); r != nil {
			return r.String()
		}
	}
	return peer
}
