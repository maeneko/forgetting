package ui

import (
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"
)

// jsToNumber — Number(s) для строки: пробелы по краям, пусто → 0, 0x/0o/0b, Infinity;
// всё прочее нечисловое — NaN.
func jsToNumber(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(t) > 2 && t[0] == '0' {
		base := 0
		switch t[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, err := strconv.ParseUint(t[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r == '.' || r == 'e' || r == 'E' || r == '+' || r == '-') {
			return math.NaN()
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// jsToNumberAny — Number(v) для значения из JSON.
func jsToNumberAny(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		return jsToNumber(x)
	}
	return math.NaN()
}

// encodeURIComponent — как в JS: не трогает A-Z a-z 0-9 - _ . ! ~ * ' ( ).
func encodeURIComponent(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

func readAll(resp *http.Response) ([]byte, error) { return io.ReadAll(resp.Body) }

// utf16Len — длина строки в JS (кодовые единицы UTF-16).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// lenOK — `{min,max}` в JS-регулярке над строкой целиком: длина в единицах UTF-16.
func lenOK(s string, min, max int) bool {
	n := utf16Len(s)
	return n >= min && n <= max
}

// sliceUTF16 — s.slice(0, n) в JS. Разрезанная суррогатная пара при записи в SQLite
// превращается в U+FFFD — так же делаем и здесь.
func sliceUTF16(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}

func quietLog() *log.Logger { return log.New(io.Discard, "", 0) }
