package ctrl

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

// I1 — «сигнатурный» пакет, который клиент шлёт перед рукопожатием, чтобы начало сессии
// выглядело как другой протокол. Сервер I-пакеты отбрасывает (они не совпадают ни с одним
// H-заголовком), поэтому совпадать у сервера и клиента I1 не обязан: выданные vpn:// ключи со
// старым I1 продолжают подключаться, а смена касается только новых конфигов.
//
// Поэтому I1 живёт не в awg1.conf, а в БД awg-ctrl (config.i1): в ядро он не уходит — нет
// риска, что модуль не примет шаблон и интерфейс не поднимется, — и переживает KEEP_DATA вместе
// с базой. Если I1 вписан в awg1.conf руками, на первом старте он и берётся.
//
// Шаблон — один из нескольких правдоподобных (DNS-запрос, начало QUIC Initial, STUN binding
// request) со случайными полями: у каждого сервера свой, общий для всех I1 был бы сигнатурой.
// Только теги <b 0x…> и <r N>: их понимают все клиенты с I1–I5. Идентификаторы транзакций и
// соединений — <r N>, они случайны в каждом пакете, как у настоящих протоколов.
const cfgI1 = "i1"

// Популярные домены, запросы к которым в любой сети выглядят обычно. Заблокированных в РФ
// нет: запрос к ним сам по себе привлекал бы внимание.
var i1Domains = []string{
	"google.com", "www.google.com", "youtube.com", "gstatic.com", "googleapis.com",
	"apple.com", "icloud.com", "microsoft.com", "bing.com", "live.com", "cloudflare.com",
	"amazon.com", "wikipedia.org", "github.com", "yandex.ru", "ya.ru", "vk.com", "mail.ru",
	"ok.ru", "avito.ru", "ozon.ru", "wildberries.ru", "gosuslugi.ru", "dzen.ru", "2gis.ru",
}

// GenI1 — случайный I1 из одного из шаблонов.
func GenI1() string {
	switch rand.IntN(3) {
	case 0:
		return i1DNS()
	case 1:
		return i1QUIC()
	default:
		return i1STUN()
	}
}

func pick[T any](v ...T) T { return v[rand.IntN(len(v))] }

// DNS-запрос: ID — <r 2>, флаги RD, один вопрос A/AAAA/HTTPS, иногда EDNS0 (как у systemd-resolved,
// браузеров и роутеров).
func i1DNS() string {
	var b []byte
	edns := rand.IntN(2) == 0
	ar := uint16(0)
	if edns {
		ar = 1
	}
	b = binary.BigEndian.AppendUint16(b, 0x0100)               // флаги: стандартный запрос, RD
	b = append(b, 0, 1, 0, 0, 0, 0)                            // QDCOUNT 1, ANCOUNT 0, NSCOUNT 0
	b = binary.BigEndian.AppendUint16(b, ar)                   // ARCOUNT
	for _, l := range strings.Split(pick(i1Domains...), ".") { // QNAME
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, pick[uint16](1, 1, 28, 65)) // QTYPE: A чаще, AAAA, HTTPS
	b = append(b, 0, 1)                                              // QCLASS IN
	if edns {
		b = append(b, 0, 0, 41) // OPT, имя — корень
		b = binary.BigEndian.AppendUint16(b, pick[uint16](1232, 4096, 1400))
		b = append(b, 0, 0)                                           // расширенный RCODE, версия
		b = binary.BigEndian.AppendUint16(b, pick[uint16](0, 0x8000)) // флаг DO
		b = append(b, 0, 0)                                           // RDLEN
	}
	return "<r 2><b 0x" + hex.EncodeToString(b) + ">"
}

// Начало QUIC v1 Initial: длинный заголовок (младшие биты первого байта под header protection —
// случайны), DCID 8 байт, SCID 0 или 8, без токена; остаток до 1200/1250 байт — шифрованная
// полезная нагрузка, её изображает <r N>.
func i1QUIC() string {
	scid := pick(0, 8)
	total := pick(1200, 1250, 1252)
	head := 1 + 4 + 1 + 8 + 1 + scid + 1 + 2
	rest := total - head
	s := "<b 0x" + hex.EncodeToString([]byte{byte(0xc0 | rand.IntN(16)), 0, 0, 0, 1, 8}) + "><r 8>"
	if scid > 0 {
		s += "<b 0x08><r 8>"
	} else {
		s += "<b 0x00>"
	}
	return s + "<b 0x00" + hex.EncodeToString(binary.BigEndian.AppendUint16(nil, 0x4000|uint16(rest))) + "><r " + strconv.Itoa(rest) + ">"
}

// STUN binding request (RFC 5389): magic cookie, ID транзакции — <r 12>; иногда с
// CHANGE-REQUEST (проверка NAT, RFC 5780).
func i1STUN() string {
	attrs := pick([]byte{}, []byte{0, 3, 0, 4, 0, 0, 0, 0})
	b := []byte{0, 1}
	b = binary.BigEndian.AppendUint16(b, uint16(len(attrs)))
	b = append(b, 0x21, 0x12, 0xa4, 0x42)
	s := "<b 0x" + hex.EncodeToString(b) + "><r 12>"
	if len(attrs) > 0 {
		s += "<b 0x" + hex.EncodeToString(attrs) + ">"
	}
	return s
}

var reI1Tag = regexp.MustCompile(`<(b|r) ([^>]*)>`)

// i1Bytes раскрывает шаблон в байты (<r N> — нули); ok=false для тегов, кроме <b>/<r>.
func i1Bytes(s string) ([]byte, bool) {
	var out []byte
	rest := strings.TrimSpace(s)
	for rest != "" {
		m := reI1Tag.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			return nil, false
		}
		kind, val := rest[m[2]:m[3]], rest[m[4]:m[5]]
		if kind == "b" {
			h, err := hex.DecodeString(strings.TrimPrefix(val, "0x"))
			if err != nil {
				return nil, false
			}
			out = append(out, h...)
		} else {
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 65535 {
				return nil, false
			}
			out = append(out, make([]byte, n)...)
		}
		rest = rest[m[1]:]
	}
	return out, len(out) > 0
}

// dnsName — имя первого вопроса DNS-пакета.
func dnsName(b []byte) string {
	var parts []string
	for i := 12; i < len(b) && b[i] != 0; {
		n := int(b[i])
		if n > 63 || i+1+n > len(b) {
			return ""
		}
		parts = append(parts, string(b[i+1:i+1+n]))
		i += 1 + n
	}
	return strings.Join(parts, ".")
}

// DescribeI1 — под что маскируется I1, для панели: вид (dns, quic, stun, custom, none) и подпись.
func DescribeI1(s string) (kind, label string) {
	if strings.TrimSpace(s) == "" {
		return "none", "без маскировки"
	}
	b, ok := i1Bytes(s)
	switch {
	case !ok:
	case len(b) >= 20 && binary.BigEndian.Uint32(b[4:8]) == 0x2112a442 && b[0] == 0 && b[1] == 1:
		return "stun", "STUN binding request"
	case len(b) >= 6 && b[0]&0xc0 == 0xc0 && binary.BigEndian.Uint32(b[1:5]) == 1:
		return "quic", "QUIC Initial, " + strconv.Itoa(len(b)) + " байт"
	case len(b) > 12 && b[2]&0x80 == 0 && binary.BigEndian.Uint16(b[4:6]) == 1:
		if n := dnsName(b); n != "" {
			return "dns", "DNS-запрос к " + n
		}
	case len(b) > 12 && b[2]&0x80 != 0:
		if n := dnsName(b); n != "" {
			return "dns", "DNS-ответ от " + n
		}
	}
	return "custom", "свой шаблон"
}

// i1FromConf — I1, вписанный в [Interface] awg1.conf руками (ReadParams без него отдаёт
// значение по умолчанию).
func i1FromConf(confFile string) string {
	p := ReadParams(confFile, nil)
	if p.I1 == DefaultParams().I1 {
		return ""
	}
	return p.I1
}

// ensureI1 — на старте: I1 этого сервера из БД; нет — из awg1.conf, нет и там — новый случайный.
func (s *Server) ensureI1() string {
	if v := s.getCfg(cfgI1, ""); v != "" {
		return v
	}
	v := i1FromConf(s.confPath())
	if v == "" {
		v = GenI1()
	}
	s.setCfg(cfgI1, v)
	kind, label := DescribeI1(v)
	s.log.Info("I1 assigned", "kind", kind, "label", label)
	return v
}

// regenI1 — новый случайный I1. Клиенты по sen:// получают его со следующим обновлением
// профиля; vpn:// ключи несут прежний и продолжают работать, пока их не перевыпустят.
func (s *Server) regenI1() string {
	v := GenI1()
	s.setCfg(cfgI1, v)
	s.cfgMu.Lock()
	s.c.Params.I1 = v
	s.cfgMu.Unlock()
	kind, label := DescribeI1(v)
	s.log.Info("I1 regenerated", "kind", kind, "label", label)
	return v
}

func maskingInfo(i1 string) ob {
	kind, label := DescribeI1(i1)
	return ob{{"i1", i1}, {"kind", kind}, {"label", label}}
}
