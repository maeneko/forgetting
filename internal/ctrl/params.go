package ctrl

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// AWG3Keys — ключи AmneziaWG 3.0/3.1. Порядок — порядок строк в клиентском .conf
// (client/server_scripts/awg/template.conf в amnezia-client). Пустое значение =
// ключ не задан: он не попадает ни в conf, ни в vpn://, и 2.0 остаётся побайтно прежним.
var AWG3Keys = []string{
	"HeaderProtectionKey",
	"ContentPaddingAddition",
	"RekeyAfterTime",
	"RekeyTimeout",
	"RejectAfterTime",
	"KeepaliveTimeout",
	"MaxHandshakeAttempts",
	"RandomTrailers",
	"DisableCookies",
}

// Params — параметры обфускации из [Interface] awg1.conf. Числовые — числа (в
// vpn:// они уходят строками), остальные — строки.
type Params struct {
	Jc, Jmin, Jmax     int
	S1, S2, S3, S4     int
	H1, H2, H3, H4     string
	I1, I2, I3, I4, I5 string
	A3                 map[string]string // AWG3Keys → значение ("" = не задано)
}

// DefaultParams — запасные значения на случай, когда awg1.conf не найден.
func DefaultParams() Params {
	return Params{
		Jc: 6, Jmin: 10, Jmax: 50,
		S1: 90, S2: 45, S3: 37, S4: 14,
		H1: "1224800044-2116730834",
		H2: "2122053282-2133204808",
		H3: "2133604274-2140756116",
		H4: "2143656228-2147444225",
		I1: "<r 2><b 0x858000010001000000000669636c6f756403636f6d0000010001c00c000100010000105a00044d583737>",
		A3: map[string]string{},
	}
}

func (p Params) a3(k string) string { return p.A3[k] }

// Gen выводится из параметров, отдельного флага нет: conf — единственный источник
// правды. Header protection — то, что ломает совместимость с 2.0.
func (p Params) Gen() string {
	if p.a3("HeaderProtectionKey") != "" {
		return "3.1"
	}
	return "2"
}

var keepaliveByGen = map[string]string{"2": "25", "3.1": "25-35"}

// AWG31Defaults — значения 3.1-параметров для перехода 2.0 → 3.1 (как у install.sh),
// в порядке записи. HeaderProtectionKey генерируется на месте.
var AWG31Defaults = [][2]string{
	{"ContentPaddingAddition", "10-100"},
	{"RekeyAfterTime", "100-120"},
	{"RekeyTimeout", "3-7"},
	{"RejectAfterTime", "150-180"},
	{"KeepaliveTimeout", "5-15"},
	{"MaxHandshakeAttempts", "15-20"},
}

// При заданном HeaderProtectionKey ни один из S1–S4 не может быть меньше 12.
const minSForHeaderProtection = 12

var (
	pPeerSplit = regexp.MustCompile(`(?m)^\[Peer\]`)
	numKeys    = []string{"Jc", "Jmin", "Jmax", "S1", "S2", "S3", "S4"}
	strKeys    = append([]string{"H1", "H2", "H3", "H4", "I1", "I2", "I3", "I4", "I5"}, AWG3Keys...)
)

// ReadParams читает параметры из [Interface] awg1.conf (всё до первого [Peer]).
//
// Повторяет регулярки Node-версии: `^\s*K\s*=\s*(\d+)` с флагом m. `\s*` в JS
// захватывает и перевод строки, так что строка «I2 = » без значения «съела» бы
// следующую — install.sh таких не пишет, но поведение сохранено намеренно.
func ReadParams(confFile string, warn func(string)) Params {
	p := DefaultParams()
	raw, err := os.ReadFile(confFile)
	if err != nil {
		if warn != nil {
			warn("awg1.conf не найден — параметры обфускации по умолчанию")
		}
		return p
	}
	iface := pPeerSplit.Split(string(raw), 2)[0]

	for _, k := range numKeys {
		m := regexp.MustCompile(`(?m)^\s*` + k + `\s*=\s*(\d+)`).FindStringSubmatch(iface)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		switch k {
		case "Jc":
			p.Jc = n
		case "Jmin":
			p.Jmin = n
		case "Jmax":
			p.Jmax = n
		case "S1":
			p.S1 = n
		case "S2":
			p.S2 = n
		case "S3":
			p.S3 = n
		case "S4":
			p.S4 = n
		}
	}
	for _, k := range strKeys {
		// В JS `.` не совпадает с \n, \r, U+2028, U+2029; в Go-режиме (?m) без (?s) — только с \n.
		m := regexp.MustCompile(`(?m)^\s*` + k + `\s*=\s*([^\n\r\x{2028}\x{2029}]*)$`).FindStringSubmatch(iface)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[1])
		switch k {
		case "H1":
			p.H1 = v
		case "H2":
			p.H2 = v
		case "H3":
			p.H3 = v
		case "H4":
			p.H4 = v
		case "I1":
			p.I1 = v
		case "I2":
			p.I2 = v
		case "I3":
			p.I3 = v
		case "I4":
			p.I4 = v
		case "I5":
			p.I5 = v
		default:
			p.A3[k] = v
		}
	}
	return p
}
