package migrate

// Перенос из панели в панель (вкладка «Перенос»): новая панель выдаёт одноразовый код
// awgmove://, старая шлёт по нему архив на её порт панели. Порт панели обычно — голый HTTP,
// а в архиве ключ подписи, поэтому и запрос, и ответ зашифрованы AES-256-GCM ключом из кода:
// без кода архив не прочесть и не подделать, а верный тег и есть доказательство, что код знают.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

const (
	CodePrefix = "awgmove://"
	keyLen     = 32
	// Разные AAD — ответ нельзя выдать за запрос и наоборот.
	aadRequest  = "awgmove-v1 request"
	aadResponse = "awgmove-v1 response"
)

// Code — что старая панель узнаёт из кода: куда слать и чем шифровать.
type Code struct {
	Host string
	Port int
	Key  []byte
}

func NewKey() []byte {
	k := make([]byte, keyLen)
	rand.Read(k)
	return k
}

func (c Code) String() string {
	return CodePrefix + b64url.EncodeToString(jsjson.MustMarshal(jsjson.Object{
		{Key: "v", Val: Version}, {Key: "h", Val: c.Host}, {Key: "p", Val: c.Port}, {Key: "k", Val: b64url.EncodeToString(c.Key)}}))
}

func ParseCode(s string) (Code, error) {
	bad := errors.New("код переноса неверного формата — скопируйте его целиком со вкладки «Перенос» новой панели")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, CodePrefix) {
		return Code{}, bad
	}
	raw, err := b64url.DecodeString(s[len(CodePrefix):])
	if err != nil {
		return Code{}, bad
	}
	v, err := jsjson.Decode(raw)
	o, ok := v.(jsjson.Object)
	if err != nil || !ok || o.Get2("v") != float64(Version) {
		return Code{}, bad
	}
	host, _ := o.Get2("h").(string)
	port, _ := o.Get2("p").(float64)
	k, _ := o.Get2("k").(string)
	key, err := b64url.DecodeString(k)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/?#@ ") || port <= 0 || port > 65535 ||
		port != float64(int(port)) || err != nil || len(key) != keyLen {
		return Code{}, bad
	}
	return Code{Host: host, Port: int(port), Key: key}, nil
}

// URL — куда слать архив.
func (c Code) URL() string {
	h := c.Host
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	return "http://" + h + ":" + strconv.Itoa(c.Port) + "/migrate/v1/import"
}

func gcm(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func seal(key []byte, aad string, plain []byte) []byte {
	g, err := gcm(key)
	if err != nil {
		panic(err) // ключ всегда 32 байта
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, []byte(aad))
}

var ErrSeal = errors.New("не расшифровать: код не тот или данные повреждены")

func open(key []byte, aad string, box []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil || len(box) < g.NonceSize() {
		return nil, ErrSeal
	}
	plain, err := g.Open(nil, box[:g.NonceSize()], box[g.NonceSize():], []byte(aad))
	if err != nil {
		return nil, ErrSeal
	}
	return plain, nil
}

func SealRequest(key, plain []byte) []byte         { return seal(key, aadRequest, plain) }
func OpenRequest(key, box []byte) ([]byte, error)  { return open(key, aadRequest, box) }
func SealResponse(key, plain []byte) []byte        { return seal(key, aadResponse, plain) }
func OpenResponse(key, box []byte) ([]byte, error) { return open(key, aadResponse, box) }
