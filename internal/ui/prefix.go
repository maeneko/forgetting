package ui

// Секретный префикс панели (UI_PREFIX): снаружи панель отвечает только на /<префикс>/…, на всё
// остальное — как пустой Go-сервер (`404 page not found`). Порт панели открыт в интернет, и без
// префикса сканер узнавал её по странице логина и JS-бандлу — одним GET.
//
// UI_PREFIX_MOVED — префикс панели, переехавшей сюда (`migrate`): её закладки и внешний API
// приходят через ретранслятор старого сервера со своим путём. Принимаются оба.
//
// С loopback префикс не нужен: CLI, Prometheus этой машины (127.0.0.1:PORT/metrics) и nginx
// на этой же машине ходят по-старому (nginx в подпапке и есть свой секретный путь). Адрес —
// соединения, не X-Forwarded-For: заголовок подделывается.

import (
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/maeneko/forgetting/internal/migrate"
)

// uiPrefixes — UI_PREFIX и UI_PREFIX_MOVED из окружения; пустые и кривые пропускаются.
func uiPrefixes() []string {
	var out []string
	for _, k := range []string{"UI_PREFIX", "UI_PREFIX_MOVED"} {
		p := strings.Trim(strings.TrimSpace(os.Getenv(k)), "/")
		switch {
		case p == "":
		case !migrate.ValidUIPrefix(p):
			warnf("ui: %s «%s» не подходит (8–64 символа A–Z a–z 0–9 _ -) — пропущен", k, p)
		default:
			out = append(out, p)
		}
	}
	return out
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// prefixGate пропускает в панель только запросы под префиксом и срезает его: дальше роутер
// видит те же пути, что без префикса. allow — исключения вне префикса (приём переноса).
func prefixGate(prefixes []string, next http.Handler, allow func(*http.Request) bool) http.Handler {
	if len(prefixes) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		for _, px := range prefixes {
			root := "/" + px
			if p == root {
				// Относительный адрес, как у static: за прокси в подпапке абсолютный увёл бы из неё.
				w.Header().Set("Location", px+"/")
				w.WriteHeader(http.StatusMovedPermanently)
				return
			}
			if strings.HasPrefix(p, root+"/") {
				r2 := r.Clone(r.Context())
				r2.URL.Path = p[len(root):]
				if r.URL.RawPath != "" {
					r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, root)
				}
				r2.RequestURI = r2.URL.RequestURI()
				next.ServeHTTP(w, r2)
				return
			}
		}
		if isLoopback(r.RemoteAddr) || allow != nil && allow(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}
