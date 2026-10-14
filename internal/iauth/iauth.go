// Package iauth — внутренние токены между awg-ui / awg-agent / CLI и awg-ctrl.
//
// Формат — «JWT» с EdDSA: header {"alg":"EdDSA","typ":"JWT"}, payload {iss,iat,exp},
// подпись Ed25519 над ASCII "h.p". Ключи — PEM от openssl: PKCS#8 (закрытый) и SPKI
// (открытый). Проверка намеренно такая же нестрогая, как у Node-версии: только подпись
// и exp (alg/iss/лишние сегменты не смотрим) — ужесточение отдельным шагом (zero trust).
package iauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func decodePEM(file string) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("iauth: not a PEM file: " + file)
	}
	return blk.Bytes, nil
}

// LoadPublic читает открытый ключ (SPKI PEM).
func LoadPublic(file string) (ed25519.PublicKey, error) {
	der, err := decodePEM(file)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("iauth: not an Ed25519 public key")
	}
	return pub, nil
}

// LoadPrivate читает закрытый ключ (PKCS#8 PEM).
func LoadPrivate(file string) (ed25519.PrivateKey, error) {
	der, err := decodePEM(file)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("iauth: not an Ed25519 private key")
	}
	return priv, nil
}

var b64 = base64.RawURLEncoding

// Mint выпускает токен на 60 с.
func Mint(priv ed25519.PrivateKey, iss string, now time.Time) string {
	h := b64.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	t := now.Unix()
	p := b64.EncodeToString(jsjson.MustMarshal(jsjson.Object{{Key: "iss", Val: iss}, {Key: "iat", Val: t}, {Key: "exp", Val: t + 60}}))
	sig := ed25519.Sign(priv, []byte(h+"."+p))
	return h + "." + p + "." + b64.EncodeToString(sig)
}

// lenient повторяет Buffer.from(s, "base64url") в Node: терпит паддинг, «+», «/».
func lenient(s string) ([]byte, error) {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	return b64.DecodeString(s)
}

// Verify — подпись верна и exp в будущем.
func Verify(pub ed25519.PublicKey, token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	sig, err := lenient(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return false
	}
	raw, err := lenient(parts[1])
	if err != nil {
		return false
	}
	v, err := jsjson.Decode(raw)
	if err != nil {
		return false
	}
	obj, ok := v.(jsjson.Object)
	if !ok {
		return false
	}
	var exp float64
	switch e := obj.Get2("exp").(type) {
	case float64:
		exp = e
	case string: // JS сравнил бы числовую строку как число
		f, err := strconv.ParseFloat(strings.TrimSpace(e), 64)
		if err != nil {
			return false
		}
		exp = f
	default:
		return false
	}
	return exp > float64(now.Unix())
}
