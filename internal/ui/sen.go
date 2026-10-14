package ui

// sen:// — ссылка-подписка для SenAWG и подписи протокола /sub/v1 (порт awg-ui/sen.ts).
// Байтовая раскладка и схема подписей описаны в docs/sen-link.md; клиент реализуется по
// ней, поэтому любое изменение здесь — изменение протокола.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	senPrefix  = "sen://"
	senVersion = 1
	flagTLS    = 0x01

	addrV4     = 0x04
	addrV6     = 0x06
	addrDomain = 0x44
)

type senLinkError struct{ msg string }

func (e *senLinkError) Error() string { return e.msg }

func senErr(f string, a ...any) error { return &senLinkError{fmt.Sprintf(f, a...)} }

type SenAddr struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type SenLink struct {
	TLS     bool
	Addrs   []SenAddr
	Secret  []byte // 16 байт
	SignPub []byte // 32 байта, Ed25519
	TLSPin  []byte // 32 байта, только при TLS
	Name    string
}

// b64url — Buffer.toString("base64url"): без паддинга.
var b64url = base64.RawURLEncoding

// fromB64url повторяет Buffer.from(s, "base64url") в Node: терпит паддинг и «+/», хвостовой
// одиночный символ (неполный байт) молча отбрасывает, мусор после первого чужого символа — тоже.
func fromB64url(s string) []byte {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	if i := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}); i >= 0 {
		s = s[:i]
	}
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	b, _ := b64url.DecodeString(s)
	return b
}

// fromB64 — Buffer.from(s, "base64") (обычный алфавит, тоже нестрого).
func fromB64(s string) []byte {
	return fromB64url(strings.TrimRight(s, "="))
}

// ── Адреса ─────────────────────────────────────────────────────────────────

var (
	reHex4   = regexp.MustCompile(`^[0-9a-fA-F]{1,4}$`)
	reIPv4   = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	reDomain = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
)

func ipv6ToBytes(host string) []byte {
	parts := strings.Split(host, "::")
	if len(parts) > 2 {
		return nil
	}
	var head, tail []string
	if parts[0] != "" {
		head = strings.Split(parts[0], ":")
	}
	if len(parts) == 2 && parts[1] != "" {
		tail = strings.Split(parts[1], ":")
	}
	missing := 8 - len(head) - len(tail)
	if len(parts) == 1 && len(head) != 8 || len(parts) == 2 && missing < 1 {
		return nil
	}
	groups := append([]string{}, head...)
	if len(parts) == 2 {
		for range missing {
			groups = append(groups, "0")
		}
	}
	groups = append(groups, tail...)
	if len(groups) != 8 {
		return nil
	}
	out := make([]byte, 16)
	for i, g := range groups {
		if !reHex4.MatchString(g) {
			return nil
		}
		n, _ := strconv.ParseUint(g, 16, 16)
		binary.BigEndian.PutUint16(out[i*2:], uint16(n))
	}
	return out
}

func bytesToIPv6(b []byte) string {
	g := make([]string, 8)
	for i := range 8 {
		g[i] = strconv.FormatUint(uint64(binary.BigEndian.Uint16(b[i*2:])), 16)
	}
	// Самый длинный run нулей сворачиваем в «::» — как принято в текстовой записи.
	bestStart, bestLen := -1, 0
	for i := 0; i < 8; {
		if g[i] != "0" {
			i++
			continue
		}
		j := i
		for j < 8 && g[j] == "0" {
			j++
		}
		if j-i > bestLen {
			bestStart, bestLen = i, j-i
		}
		i = j
	}
	if bestLen < 2 {
		return strings.Join(g, ":")
	}
	return strings.Join(g[:bestStart], ":") + "::" + strings.Join(g[bestStart+bestLen:], ":")
}

func encodeAddr(a SenAddr) ([]byte, error) {
	if a.Port < 1 || a.Port > 65535 {
		return nil, senErr("Неверный порт: %d", a.Port)
	}
	p := binary.BigEndian.AppendUint16(nil, uint16(a.Port))
	if reIPv4.MatchString(a.Host) {
		out := []byte{addrV4}
		for _, o := range strings.Split(a.Host, ".") {
			n, _ := strconv.Atoi(o)
			if n > 255 {
				return nil, senErr("Неверный IPv4: %s", a.Host)
			}
			out = append(out, byte(n))
		}
		return append(out, p...), nil
	}
	if strings.Contains(a.Host, ":") {
		b := ipv6ToBytes(a.Host)
		if b == nil {
			return nil, senErr("Неверный IPv6: %s", a.Host)
		}
		return append(append([]byte{addrV6}, b...), p...), nil
	}
	if len(a.Host) < 1 || len(a.Host) > 253 || !reDomain.MatchString(a.Host) {
		return nil, senErr("Неверный домен: %s", a.Host)
	}
	return append(append([]byte{addrDomain, byte(len(a.Host))}, a.Host...), p...), nil
}

// ── Ссылка ─────────────────────────────────────────────────────────────────

func EncodeSenLink(l SenLink) (string, error) {
	if len(l.Addrs) < 1 || len(l.Addrs) > 3 {
		return "", senErr("Адресов должно быть от 1 до 3")
	}
	if len(l.Secret) != 16 {
		return "", senErr("secret: 16 байт")
	}
	if len(l.SignPub) != 32 {
		return "", senErr("signPub: 32 байта")
	}
	if l.TLS && len(l.TLSPin) != 32 {
		return "", senErr("tlsPin: 32 байта при TLS")
	}
	if len(l.Name) > 64 {
		return "", senErr("Имя: до 64 байт")
	}
	var flags byte
	if l.TLS {
		flags = flagTLS
	}
	body := []byte{senVersion, flags, byte(len(l.Addrs))}
	for _, a := range l.Addrs {
		b, err := encodeAddr(a)
		if err != nil {
			return "", err
		}
		body = append(body, b...)
	}
	body = append(body, l.Secret...)
	body = append(body, l.SignPub...)
	if l.TLS {
		body = append(body, l.TLSPin...)
	}
	body = append(body, byte(len(l.Name)))
	body = append(body, l.Name...)
	body = binary.BigEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
	return senPrefix + b64url.EncodeToString(body), nil
}

var (
	reB64urlChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	reSpaces      = regexp.MustCompile(`\s+`)
)

func DecodeSenLink(link string) (SenLink, error) {
	var l SenLink
	t := strings.TrimSpace(link)
	if !strings.HasPrefix(strings.ToLower(t), senPrefix) {
		return l, senErr("Ссылка должна начинаться с sen://")
	}
	payload := reSpaces.ReplaceAllString(t[len(senPrefix):], "")
	if !reB64urlChars.MatchString(payload) {
		return l, senErr("Ссылка повреждена: недопустимые символы")
	}
	raw := fromB64url(payload)
	if len(raw) < 3+4 {
		return l, senErr("Ссылка слишком короткая")
	}
	body := raw[:len(raw)-4]
	if binary.BigEndian.Uint32(raw[len(raw)-4:]) != crc32.ChecksumIEEE(body) {
		return l, senErr("Ссылка повреждена: не сошлась контрольная сумма")
	}

	off := 0
	errCut := senErr("Ссылка обрезана")
	take := func(n int) ([]byte, error) {
		if off+n > len(body) {
			return nil, errCut
		}
		s := body[off : off+n]
		off += n
		return s, nil
	}
	b1 := func() (byte, error) {
		b, err := take(1)
		if err != nil {
			return 0, err
		}
		return b[0], nil
	}

	version, err := b1()
	if err != nil {
		return l, err
	}
	if version != senVersion {
		return l, senErr("Неизвестная версия ссылки: %d", version)
	}
	flags, err := b1()
	if err != nil {
		return l, err
	}
	if flags&^flagTLS != 0 {
		return l, senErr("Неизвестные флаги ссылки")
	}
	n, err := b1()
	if err != nil {
		return l, err
	}
	if n < 1 || n > 3 {
		return l, senErr("Неверное число адресов")
	}
	for range n {
		typ, err := b1()
		if err != nil {
			return l, err
		}
		var host string
		switch typ {
		case addrV4:
			b, err := take(4)
			if err != nil {
				return l, err
			}
			host = fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
		case addrV6:
			b, err := take(16)
			if err != nil {
				return l, err
			}
			host = bytesToIPv6(b)
		case addrDomain:
			ln, err := b1()
			if err != nil {
				return l, err
			}
			if ln < 1 {
				return l, senErr("Пустой домен")
			}
			b, err := take(int(ln))
			if err != nil {
				return l, err
			}
			host = jsUTF8(b)
			if !reDomain.MatchString(host) {
				return l, senErr("Неверный домен")
			}
		default:
			return l, senErr("Неизвестный тип адреса: %d", typ)
		}
		pb, err := take(2)
		if err != nil {
			return l, err
		}
		port := int(binary.BigEndian.Uint16(pb))
		if port == 0 {
			return l, senErr("Порт 0")
		}
		l.Addrs = append(l.Addrs, SenAddr{host, port})
	}
	if l.Secret, err = take(16); err != nil {
		return l, err
	}
	if l.SignPub, err = take(32); err != nil {
		return l, err
	}
	l.TLS = flags&flagTLS != 0
	if l.TLS {
		if l.TLSPin, err = take(32); err != nil {
			return l, err
		}
	}
	nameLen, err := b1()
	if err != nil {
		return l, err
	}
	if nameLen > 64 {
		return l, senErr("Имя длиннее 64 байт")
	}
	nb, err := take(int(nameLen))
	if err != nil {
		return l, err
	}
	l.Name = jsUTF8(nb)
	if off != len(body) {
		return l, senErr("Лишние данные в конце ссылки")
	}
	l.Secret, l.SignPub = bytes.Clone(l.Secret), bytes.Clone(l.SignPub)
	l.TLSPin = bytes.Clone(l.TLSPin)
	return l, nil
}

// jsUTF8 — Buffer.toString("utf8"): битые последовательности заменяются на U+FFFD.
func jsUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// ── Ключи Ed25519 ──────────────────────────────────────────────────────────

// loadEd25519Private читает PKCS#8 PEM (как crypto.createPrivateKey в Node).
func loadEd25519Private(raw []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("not a PEM key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return priv, nil
}

// spkiPin — SHA-256 от SPKI DER сертификата (первого в PEM) — то, что кладётся в ссылку как pin.
func spkiPin(certPEM []byte) ([]byte, error) {
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return nil, errors.New("not a PEM certificate")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return sum[:], nil
}

// ── Подписи протокола ──────────────────────────────────────────────────────

// signedResponse — {"body": "<JSON-строка>", "sig": "<base64url>"}; sig — Ed25519 над
// UTF-8 байтами строки body. body — именно строка, чтобы клиенту не нужна была
// каноническая сериализация JSON.
func signResponse(payload any, key ed25519.PrivateKey) jsjson.Object {
	body := string(jsjson.MustMarshal(payload))
	return jsjson.Object{{Key: "body", Val: body}, {Key: "sig", Val: b64url.EncodeToString(ed25519.Sign(key, []byte(body)))}}
}

// requestSigString — METHOD \n /path \n ts \n hex(sha256(тело запроса)); path — без query.
func requestSigString(method, path string, ts int64, body []byte) string {
	h := sha256.Sum256(body)
	return strings.ToUpper(method) + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(h[:])
}

func verifyRequest(method, path string, ts int64, body []byte, sig string, authPub []byte) bool {
	if len(authPub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(authPub, []byte(requestSigString(method, path, ts, body)), fromB64url(sig))
}

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }
