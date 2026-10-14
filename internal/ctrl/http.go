package ctrl

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
)

// Минимальный роутер с поведением Express 4, на которое опираются клиенты:
//   - тело JSON разбирается ДО авторизации и маршрутизации (битый JSON без токена → 400, не 401);
//   - ошибки разбора и неизвестные пути отвечают HTML, как finalhandler Express;
//   - пути без учёта регистра и с необязательным «/» в конце; :param декодируется.
// Первое совпавшее объявление выигрывает — порядок ниже важен («reissue» до «:name», «sync» до «:pub»).

type rctx struct {
	w      http.ResponseWriter
	r      *http.Request
	s      *Server
	params map[string]string
	body   any // jsjson.Object, []any или nil (= {})
}

type result struct {
	status int
	v      any
}

type route struct {
	method string
	segs   []string
	mw     []func(*rctx) error
	lock   bool
	h      func(*rctx) (result, error)
}

const (
	bodyLimitSmall = 1024
	bodyLimitBig   = 512 * 1024
)

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
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>%s</pre>\n</body>\n</html>\n", text)
}

// readBody — express.json({limit}): только application/json, объект или массив.
func readBody(r *http.Request, limit int64) (any, int, string) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" || r.ContentLength == 0 && r.Body == http.NoBody {
		return nil, 0, ""
	}
	if r.ContentLength > limit {
		return nil, 413, "PayloadTooLargeError: request entity too large"
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, 400, "BadRequestError: request aborted"
	}
	if int64(len(raw)) > limit {
		return nil, 413, "PayloadTooLargeError: request entity too large"
	}
	trimmed := strings.TrimLeftFunc(string(raw), unicode.IsSpace)
	if trimmed == "" {
		return nil, 0, ""
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, 400, "SyntaxError: Unexpected token in JSON"
	}
	v, err := jsjson.Decode(raw)
	if err != nil {
		return nil, 400, "SyntaxError: " + err.Error()
	}
	return v, 0, ""
}

func (c *rctx) obj() jsjson.Object {
	o, _ := c.body.(jsjson.Object)
	return o
}

func match(rt route, method string, segs []string) map[string]string {
	if rt.method != method || len(rt.segs) != len(segs) {
		return nil
	}
	params := map[string]string{}
	for i, p := range rt.segs {
		if strings.HasPrefix(p, ":") {
			v, ok := unescape(segs[i])
			if !ok {
				return nil
			}
			params[p[1:]] = v
		} else if !strings.EqualFold(p, segs[i]) {
			return nil
		}
	}
	return params
}

func unescape(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s)+0 && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		} else if s[i] == '%' {
			return "", false
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String(), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func (s *Server) handler() http.Handler {
	routes := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Тело разбирается первым и для всех путей; лимит выбирает точный путь.
		limit := int64(bodyLimitSmall)
		p := strings.TrimSuffix(r.URL.EscapedPath(), "/")
		if strings.EqualFold(p, "/api/peers/sync") || strings.EqualFold(p, "/api/audit/clean") {
			limit = bodyLimitBig
		}
		body, status, msg := readBody(r, limit)
		if status != 0 {
			writeHTMLError(w, status, msg)
			return
		}
		segs := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
		for _, rt := range routes {
			params := match(rt, r.Method, segs)
			if params == nil {
				continue
			}
			c := &rctx{w: w, r: r, s: s, params: params, body: body}
			s.serve(c, rt)
			return
		}
		writeHTMLError(w, 404, "Cannot "+r.Method+" "+r.URL.Path)
	})
}

func (s *Server) serve(c *rctx, rt route) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("handler panic", "error", fmt.Sprint(p))
			writeJSON(c.w, 500, jsjson.Object{{Key: "error", Val: "Internal server error"}})
		}
	}()
	for _, mw := range rt.mw {
		if err := mw(c); err != nil {
			s.fail(c, err)
			return
		}
	}
	if rt.lock {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	res, err := rt.h(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	writeJSON(c.w, res.status, res.v)
}

func (s *Server) fail(c *rctx, err error) {
	if he, ok := err.(*HTTPError); ok {
		writeJSON(c.w, he.Status, jsjson.Object{{Key: "error", Val: he.Msg}})
		return
	}
	s.log.Error("handler error", "error", err)
	writeJSON(c.w, 500, jsjson.Object{{Key: "error", Val: "Internal server error"}})
}

// ───────── middleware ─────────

func (s *Server) auth(c *rctx) error {
	h := c.r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(h, "Bearer ") {
		tok = h[7:]
	}
	if tok == "" || !iauth.Verify(s.pub, tok, nowFn()) {
		return httpErr(401, "Неверная авторизация")
	}
	return nil
}

var reName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

func validateName(c *rctx) error {
	name, ok := c.params["name"]
	if !ok {
		name, _ = c.obj().Get2("name").(string)
	}
	if name == "" || !reName.MatchString(name) {
		return httpErr(400, "Имя: буквы, цифры, _ и -, до 32 символов")
	}
	return nil
}
