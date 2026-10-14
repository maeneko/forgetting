package ctrl

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

var reOwner = regexp.MustCompile(`^[\w/.-]{0,64}$`)

// checkPubKeyAny — checkPubKey для значения из JSON (не строка → тот же 400).
func checkPubKeyAny(v any) (string, error) {
	k, ok := v.(string)
	if !ok {
		return "", httpErr(400, "Неверный публичный ключ")
	}
	return checkPubKey(k)
}

func checkPubKeys(arr []any) ([]string, error) {
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		k, err := checkPubKeyAny(v)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// jsToNumber — Number(v) для значений из JSON.
func jsToNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		return jsNumber(x)
	}
	return math.NaN()
}

func (s *Server) knownPubKeys() (map[string]bool, error) {
	known := map[string]bool{}
	rows, err := s.db.Query("SELECT pub_key FROM users")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		rows.Scan(&k)
		known[k] = true
	}
	rows.Close()
	peers, err := s.peersAll()
	if err != nil {
		return nil, err
	}
	for _, p := range peers {
		known[p.PubKey] = true
	}
	return known, nil
}

func (s *Server) isKnownPub(k string) (bool, error) {
	if p, err := s.peerGet(k); err != nil || p != nil {
		return p != nil, err
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users WHERE pub_key = ?", k).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// profileAwg — параметры для /api/profile в порядке CONFIG.awgParams (порядок объявления в
// DEFAULT_AWG_PARAMS), без пустых значений; числа — строками.
func profileAwg(p Params) jsjson.Object {
	o := jsjson.Object{}
	add := func(k, v string) {
		if v != "" {
			o = append(o, jsjson.KV{Key: k, Val: v})
		}
	}
	itoa := strconv.Itoa
	add("Jc", itoa(p.Jc))
	add("Jmin", itoa(p.Jmin))
	add("Jmax", itoa(p.Jmax))
	add("S1", itoa(p.S1))
	add("S2", itoa(p.S2))
	add("S3", itoa(p.S3))
	add("S4", itoa(p.S4))
	add("H1", p.H1)
	add("H2", p.H2)
	add("H3", p.H3)
	add("H4", p.H4)
	add("I1", p.I1)
	add("I2", p.I2)
	add("I3", p.I3)
	add("I4", p.I4)
	add("I5", p.I5)
	for _, k := range AWG3Keys {
		add(k, p.A3[k])
	}
	return o
}

var _ = strings.TrimSpace
