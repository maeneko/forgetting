package ui

// Минимальный роутер с поведением Express 4, на которое опирается фронтенд и внешние клиенты:
//   - тело разбирает express.json() до маршрутизации и для всех путей: без тела или не JSON —
//     пустой объект {}, битый JSON / не объект и не массив — HTML 400, больше лимита — HTML 413;
//   - пути без учёта регистра и с необязательным «/» в конце; :param декодируется;
//   - use(prefix) совпадает по границе сегмента; первое подходящее объявление выигрывает;
//   - неизвестный путь — HTML 404 «Cannot METHOD /path», как finalhandler.

import (
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/maeneko/forgetting/internal/jsjson"
)

type Ctx struct {
	W       http.ResponseWriter
	R       *http.Request
	Params  map[string]string
	Body    any    // jsjson.Object, []any; без тела — пустой Object
	RawBody []byte // как пришло (для подписи /sub/v1)
	Path    string // путь внутри смонтированного use(prefix), как req.path в под-роутере

	// Внешний API: ключ запроса, его серверы и выбранный сервер.
	apiKey   *apiKeyRow
	servers  []int
	serverID int
	device   *deviceRow
}

// handler возвращает false, если ответ уже отправлен и цепочка дальше не идёт.
type handler func(c *Ctx) bool

type route struct {
	method string // "" — любой (use)
	prefix bool   // use(): совпадение по префиксу
	segs   []string
	hs     []handler
}

type Router struct {
	routes []route
	// notFound — ответ, если ничего не совпало (у /sub/v1 он подписанный).
	notFound func(c *Ctx)
	// badBody — ответ на неразобранное тело (у /sub/v1 подписанный 400).
	badBody   func(w http.ResponseWriter, r *http.Request, status int, msg string)
	bodyLimit int64
}

func newRouter() *Router { return &Router{bodyLimit: 100 * 1024} }

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (rt *Router) add(method string, prefix bool, pattern string, hs ...handler) {
	rt.routes = append(rt.routes, route{method: method, prefix: prefix, segs: splitPath(pattern), hs: hs})
}

func (rt *Router) Get(p string, hs ...handler)    { rt.add("GET", false, p, hs...) }
func (rt *Router) Post(p string, hs ...handler)   { rt.add("POST", false, p, hs...) }
func (rt *Router) Put(p string, hs ...handler)    { rt.add("PUT", false, p, hs...) }
func (rt *Router) Patch(p string, hs ...handler)  { rt.add("PATCH", false, p, hs...) }
func (rt *Router) Delete(p string, hs ...handler) { rt.add("DELETE", false, p, hs...) }
func (rt *Router) Use(p string, hs ...handler)    { rt.add("", true, p, hs...) }

// Mount — app.use(prefix, mw..., router): маршруты под-роутера с префиксом и общими mw.
func (rt *Router) Mount(prefix string, sub *Router, mw ...handler) {
	pre := splitPath(prefix)
	for _, r := range sub.routes {
		segs := append(append([]string{}, pre...), r.segs...)
		rt.routes = append(rt.routes, route{method: r.method, prefix: r.prefix, segs: segs, hs: append(append([]handler{}, mw...), r.hs...)})
	}
}

// errDecode — %-последовательность в параметре не декодируется (Express: 400 URIError).
type errDecode struct{}

func match(r route, method string, segs []string) (map[string]string, int, error) {
	if r.method != "" && r.method != method && !(r.method == "GET" && method == "HEAD") {
		return nil, 0, nil
	}
	if len(segs) < len(r.segs) || !r.prefix && len(segs) != len(r.segs) {
		return nil, 0, nil
	}
	params := map[string]string{}
	for i, p := range r.segs {
		if strings.HasPrefix(p, ":") {
			v, err := url.PathUnescape(segs[i])
			if err != nil {
				return nil, 0, fmt.Errorf("URIError: Failed to decode param '%s'", segs[i])
			}
			params[p[1:]] = v
		} else if !strings.EqualFold(p, segs[i]) {
			return nil, 0, nil
		}
	}
	return params, len(r.segs), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := jsjson.Marshal(v)
	if err != nil {
		status, b = 500, []byte(`{"error":"Internal server error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}

func writeHTMLError(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>%s</pre>\n</body>\n</html>\n", html.EscapeString(text))
}

// readBody — express.json({limit}): только application/json, объект или массив.
func readBody(r *http.Request, limit int64) (body any, raw []byte, status int, msg string) {
	empty := jsjson.Object{}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" || r.Body == nil || r.Body == http.NoBody {
		return empty, nil, 0, ""
	}
	if r.ContentLength > limit {
		return nil, nil, 413, "PayloadTooLargeError: request entity too large"
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, nil, 400, "BadRequestError: request aborted"
	}
	if int64(len(raw)) > limit {
		return nil, nil, 413, "PayloadTooLargeError: request entity too large"
	}
	trimmed := strings.TrimLeftFunc(string(raw), unicode.IsSpace)
	if trimmed == "" {
		return empty, raw, 0, ""
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, nil, 400, "SyntaxError: Unexpected token in JSON"
	}
	v, err := jsjson.Decode(raw)
	if err != nil {
		return nil, nil, 400, "SyntaxError: " + err.Error()
	}
	return v, raw, 0, ""
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, raw, status, msg := readBody(r, rt.bodyLimit)
	if status != 0 {
		if rt.badBody != nil {
			rt.badBody(w, r, status, msg)
		} else {
			writeHTMLError(w, status, msg)
		}
		return
	}
	segs := splitPath(r.URL.EscapedPath())
	c := &Ctx{W: w, R: r, Body: body, RawBody: raw}
	for _, route := range rt.routes {
		params, n, err := match(route, r.Method, segs)
		if err != nil {
			writeHTMLError(w, 400, err.Error())
			return
		}
		if params == nil {
			continue
		}
		c.Params, c.Path = params, "/"+strings.Join(segs[n:], "/")
		for _, h := range route.hs {
			if !h(c) {
				return // ответ отправлен
			}
		}
		// Все обработчики вызвали next() — ищем дальше, как Express.
	}
	if rt.notFound != nil {
		rt.notFound(c)
		return
	}
	writeHTMLError(w, 404, "Cannot "+r.Method+" "+r.URL.EscapedPath())
}

// ── Ответы ──────────────────────────────────────────────────────────────────

func (c *Ctx) JSON(status int, v any) bool {
	writeJSON(c.W, status, v)
	return false
}

func errObj(msg string) jsjson.Object { return jsjson.Object{{Key: "error", Val: msg}} }

func (c *Ctx) Error(status int, msg string) bool { return c.JSON(status, errObj(msg)) }

func (c *Ctx) Obj() jsjson.Object {
	o, _ := c.Body.(jsjson.Object)
	return o
}

// remoteIP — req.socket.remoteAddress.
func (c *Ctx) remoteIP() string {
	host, _, err := net.SplitHostPort(c.R.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	return host
}

// hostname — req.hostname в Express (trust proxy выключен): Host без порта, у IPv6 — в скобках.
func (c *Ctx) hostname() string {
	h := c.R.Host
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			return h[:i+1]
		}
		return h
	}
	if i := strings.Index(h, ":"); i >= 0 {
		return h[:i]
	}
	return h
}

// ── Значения из разобранного JSON (как обращение к полям в JS) ──────────────

// field — v.a.b.c; чего нет — nil (undefined).
func field(v any, keys ...string) any {
	for _, k := range keys {
		o, ok := v.(jsjson.Object)
		if !ok {
			return nil
		}
		v = o.Get2(k)
	}
	return v
}

func has(o jsjson.Object, k string) bool {
	_, ok := o.Get(k)
	return ok
}

func str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// isInt — Number.isInteger.
func isInt(v any) bool {
	f, ok := v.(float64)
	return ok && f == float64(int64(f)) && f >= -9007199254740991*1024 && f <= 9007199254740991*1024
}

// isSafeInt — Number.isSafeInteger.
func isSafeInt(f float64) bool {
	return f == float64(int64(f)) && f >= -9007199254740991 && f <= 9007199254740991
}

// numOr — `x ?? def` для числового поля.
func numOr(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	if v == nil {
		return def
	}
	return def
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && x == x
	case string:
		return x != ""
	}
	return true
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}
