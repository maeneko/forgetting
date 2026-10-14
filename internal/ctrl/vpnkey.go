package ctrl

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"io"
	"strconv"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// Keys — пара ключей клиента и PSK.
type Keys struct{ Private, Public, PSK string }

// Подмножество CONFIG, нужное для сборки ключа.
type keyEnv struct {
	P          Params
	ServerIP   string
	ServerPort int
	ServerName string
	DNS1, DNS2 string
	MTU        int
	Keepalive  string
	Gen        string
	Subnet     string
}

// buildClientConf — клиентский .conf. Порядок: PrivateKey → AWG params (Jc,S,H,I) →
// 3.x-ключи → Address → DNS.
//
// 🛑 Пустые I2–I5 выводятся как «I2 = » с ОДНИМ хвостовым пробелом (так в рабочих ключах
// Amnezia). Пробел дан отдельной константой, чтобы его не срезала ни IDE, ни инструмент
// правки. Не «чистить».
func buildClientConf(e keyEnv, k Keys, ip, serverPub string) string {
	const sp = " "
	var awg3 strings.Builder
	for _, key := range AWG3Keys {
		if v := e.P.a3(key); v != "" {
			awg3.WriteString(key + " = " + v + "\n")
		}
	}
	var b strings.Builder
	b.WriteString("[Interface]\n")
	b.WriteString("PrivateKey = " + k.Private + "\n")
	b.WriteString("Jc = " + strconv.Itoa(e.P.Jc) + "\n")
	b.WriteString("Jmin = " + strconv.Itoa(e.P.Jmin) + "\n")
	b.WriteString("Jmax = " + strconv.Itoa(e.P.Jmax) + "\n")
	b.WriteString("S1 = " + strconv.Itoa(e.P.S1) + "\n")
	b.WriteString("S2 = " + strconv.Itoa(e.P.S2) + "\n")
	b.WriteString("S3 = " + strconv.Itoa(e.P.S3) + "\n")
	b.WriteString("S4 = " + strconv.Itoa(e.P.S4) + "\n")
	b.WriteString("H1 = " + e.P.H1 + "\n")
	b.WriteString("H2 = " + e.P.H2 + "\n")
	b.WriteString("H3 = " + e.P.H3 + "\n")
	b.WriteString("H4 = " + e.P.H4 + "\n")
	b.WriteString("I1 = " + e.P.I1 + "\n")
	b.WriteString("I2 =" + sp + "\n")
	b.WriteString("I3 =" + sp + "\n")
	b.WriteString("I4 =" + sp + "\n")
	b.WriteString("I5 =" + sp + "\n")
	b.WriteString(awg3.String())
	b.WriteString("Address = " + ip + "/32\n")
	b.WriteString("DNS = " + e.DNS1 + ", " + e.DNS2 + "\n")
	b.WriteString("\n[Peer]\n")
	b.WriteString("PublicKey = " + serverPub + "\n")
	b.WriteString("PresharedKey = " + k.PSK + "\n")
	b.WriteString("AllowedIPs = 0.0.0.0/0, ::/0\n")
	b.WriteString("Endpoint = " + e.ServerIP + ":" + strconv.Itoa(e.ServerPort) + "\n")
	b.WriteString("PersistentKeepalive = " + e.Keepalive + "\n")
	return b.String()
}

// awgObject собирает объект параметров в ASCII-алфавитном порядке, в каком его
// сериализует QJsonObject клиента AmneziaVPN. Незаданные 3.x-ключи выбрасываются.
func awgObject(p Params) jsjson.Object {
	a := func(k string) any {
		if p.a3(k) == "" {
			return jsjson.Undefined
		}
		return p.a3(k)
	}
	return jsjson.Object{
		{"ContentPaddingAddition", a("ContentPaddingAddition")},
		{"DisableCookies", a("DisableCookies")},
		{"H1", p.H1}, {"H2", p.H2}, {"H3", p.H3}, {"H4", p.H4},
		{"HeaderProtectionKey", a("HeaderProtectionKey")},
		{"I1", p.I1}, {"I2", ""}, {"I3", ""}, {"I4", ""}, {"I5", ""},
		{"Jc", strconv.Itoa(p.Jc)},
		{"Jmax", strconv.Itoa(p.Jmax)},
		{"Jmin", strconv.Itoa(p.Jmin)},
		{"KeepaliveTimeout", a("KeepaliveTimeout")},
		{"MaxHandshakeAttempts", a("MaxHandshakeAttempts")},
		{"RandomTrailers", a("RandomTrailers")},
		{"RejectAfterTime", a("RejectAfterTime")},
		{"RekeyAfterTime", a("RekeyAfterTime")},
		{"RekeyTimeout", a("RekeyTimeout")},
		{"S1", strconv.Itoa(p.S1)}, {"S2", strconv.Itoa(p.S2)},
		{"S3", strconv.Itoa(p.S3)}, {"S4", strconv.Itoa(p.S4)},
	}
}

// EncodeVpnKey собирает vpn://-ключ в формате AmneziaVPN: JSON → zlib → 4 байта длины
// (BE, несжатой) → base64url без «=».
//
// Содержимое после распаковки совпадает с Node-версией байт в байт (порядок ключей,
// типы: port — число внутри last_config и строка снаружи). Сам поток deflate у Go и
// у C-zlib разный, поэтому строка vpn:// не совпадает побайтно — решение владельца
// проекта: AmneziaVPN читает оба одинаково (qUncompress).
func EncodeVpnKey(e keyEnv, k Keys, ip, serverPub string) (string, error) {
	clientConf := buildClientConf(e, k, ip, serverPub)

	lastConfig := append(awgObject(e.P),
		jsjson.KV{"allowed_ips", []string{"0.0.0.0/0", "::/0"}},
		jsjson.KV{"clientId", k.Public},
		jsjson.KV{"client_ip", ip},
		jsjson.KV{"client_priv_key", k.Private},
		jsjson.KV{"client_pub_key", k.Public},
		jsjson.KV{"config", clientConf},
		jsjson.KV{"hostName", e.ServerIP},
		jsjson.KV{"mtu", strconv.Itoa(e.MTU)},
		jsjson.KV{"persistent_keep_alive", e.Keepalive},
		jsjson.KV{"port", e.ServerPort},
		jsjson.KV{"psk_key", k.PSK},
		jsjson.KV{"server_pub_key", serverPub},
	)
	lastConfigJSON, err := jsjson.MarshalIndent(lastConfig, "  ")
	if err != nil {
		return "", err
	}

	awg := append(awgObject(e.P),
		jsjson.KV{"last_config", string(lastConfigJSON)},
		jsjson.KV{"port", strconv.Itoa(e.ServerPort)},
		jsjson.KV{"protocol_version", e.Gen},
		jsjson.KV{"subnet_address", e.Subnet + ".0.0"},
		jsjson.KV{"transport_proto", "udp"},
	)
	outer := jsjson.Object{
		{"containers", []any{jsjson.Object{{"container", "amnezia-awg2"}, {"awg", awg}}}},
		{"defaultContainer", "amnezia-awg2"},
		{"description", e.ServerName},
		{"dns1", e.DNS1},
		{"dns2", e.DNS2},
		{"hostName", e.ServerIP},
		{"nameOverriddenByUser", true},
	}
	js, err := jsjson.Marshal(outer)
	if err != nil {
		return "", err
	}

	var zb bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zb, zlib.DefaultCompression)
	if _, err := zw.Write(js); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(js)))
	return "vpn://" + base64.RawURLEncoding.EncodeToString(append(hdr[:], zb.Bytes()...)), nil
}

// DecodeVpnKey — обратная к EncodeVpnKey. Как Buffer.from(…, "base64url") в Node,
// терпим паддинг, «+» и «/». Любая ошибка → nil.
func DecodeVpnKey(key string) jsjson.Object {
	s := strings.TrimPrefix(key, "vpn://")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	buf, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(buf) <= 4 {
		return nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(buf[4:]))
	if err != nil {
		return nil
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil
	}
	v, err := jsjson.Decode(raw)
	if err != nil {
		return nil
	}
	o, _ := v.(jsjson.Object)
	return o
}

// clientPrivKeyFrom достаёт client_priv_key из last_config: приватный ключ клиента
// нигде больше не хранится, и только так перевыпуск сохраняет личность пира.
func clientPrivKeyFrom(vpnKey string) string {
	o := DecodeVpnKey(vpnKey)
	if o == nil {
		return ""
	}
	containers, _ := o.Get2("containers").([]any)
	if len(containers) == 0 {
		return ""
	}
	c, _ := containers[0].(jsjson.Object)
	awg, _ := c.Get2("awg").(jsjson.Object)
	lc, ok := awg.Get2("last_config").(string)
	if !ok {
		return ""
	}
	v, err := jsjson.Decode([]byte(lc))
	if err != nil {
		return ""
	}
	obj, _ := v.(jsjson.Object)
	priv, _ := obj.Get2("client_priv_key").(string)
	return priv
}
