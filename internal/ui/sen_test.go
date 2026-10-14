package ui

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// Эталон — вывод самого awg-ui/sen.ts (tools/golden/sen.ts).
type senFixture struct {
	SignSeed string `json:"sign_seed"`
	SignPub  string `json:"sign_pub"`
	CRC32    []struct {
		In  string `json:"in"`
		Out uint32 `json:"out"`
	} `json:"crc32"`
	Encode []struct {
		TLS    bool      `json:"tls"`
		Addrs  []SenAddr `json:"addrs"`
		Secret string    `json:"secret"`
		TLSPin string    `json:"tlsPin"`
		Name   string    `json:"name"`
		Out    string    `json:"out"`
		Error  string    `json:"error"`
	} `json:"encode"`
	EncodeErrors []struct {
		TLS    bool      `json:"tls"`
		Addrs  []SenAddr `json:"addrs"`
		Secret string    `json:"secret"`
		Name   string    `json:"name"`
		Error  string    `json:"error"`
	} `json:"encode_errors"`
	Decode []struct {
		In      string    `json:"in"`
		Error   string    `json:"error"`
		TLS     bool      `json:"tls"`
		Addrs   []SenAddr `json:"addrs"`
		Secret  string    `json:"secret"`
		SignPub string    `json:"signPub"`
		TLSPin  *string   `json:"tlsPin"`
		Name    string    `json:"name"`
	} `json:"decode"`
	SigString []struct {
		Method, Path, Body, Out, Sig string
		TS                           int64 `json:"ts"`
	} `json:"sig_string"`
	SignResponse []struct {
		In  json.RawMessage `json:"in"`
		Out struct{ Body, Sig string }
	} `json:"sign_response"`
}

func unhex(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSenAgainstNode(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/sen.json")
	if err != nil {
		t.Fatal(err)
	}
	var f senFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(unhex(t, f.SignSeed))
	signPub := key.Public().(ed25519.PublicKey)
	if hex.EncodeToString(signPub) != f.SignPub {
		t.Fatal("sign key mismatch")
	}

	for _, c := range f.CRC32 {
		if got := crc32IEEE([]byte(c.In)); got != c.Out {
			t.Errorf("crc32(%q) = %d, want %d", c.In, got, c.Out)
		}
	}
	for _, c := range f.Encode {
		l := SenLink{TLS: c.TLS, Addrs: c.Addrs, Secret: unhex(t, c.Secret), SignPub: signPub, Name: c.Name}
		if c.TLSPin != "" {
			l.TLSPin = unhex(t, c.TLSPin)
		}
		got, err := EncodeSenLink(l)
		if err != nil || got != c.Out {
			t.Errorf("encode %+v:\n got %s %v\nwant %s", c.Addrs, got, err, c.Out)
		}
	}
	for _, c := range f.EncodeErrors {
		_, err := EncodeSenLink(SenLink{TLS: c.TLS, Addrs: c.Addrs, Secret: unhex(t, c.Secret), SignPub: signPub, Name: c.Name})
		if err == nil || err.Error() != c.Error {
			t.Errorf("encode error %+v: got %v, want %s", c.Addrs, err, c.Error)
		}
	}
	for _, c := range f.Decode {
		l, err := DecodeSenLink(c.In)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("decode %q: got %v, want error %s", c.In, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("decode %q: %v", c.In, err)
			continue
		}
		gotAddrs, _ := json.Marshal(l.Addrs)
		wantAddrs, _ := json.Marshal(c.Addrs)
		pin := ""
		if c.TLSPin != nil {
			pin = *c.TLSPin
		}
		if l.TLS != c.TLS || string(gotAddrs) != string(wantAddrs) || hex.EncodeToString(l.Secret) != c.Secret ||
			hex.EncodeToString(l.SignPub) != c.SignPub || hex.EncodeToString(l.TLSPin) != pin || l.Name != c.Name {
			t.Errorf("decode %q:\n got %+v %s", c.In, l, gotAddrs)
		}
	}
	for _, c := range f.SigString {
		if got := requestSigString(c.Method, c.Path, c.TS, []byte(c.Body)); got != c.Out {
			t.Errorf("sig string: got %q want %q", got, c.Out)
		}
		if !verifyRequest(c.Method, c.Path, c.TS, []byte(c.Body), c.Sig, signPub) {
			t.Errorf("Node signature for %s %s does not verify", c.Method, c.Path)
		}
		if verifyRequest(c.Method, c.Path, c.TS+1, []byte(c.Body), c.Sig, signPub) {
			t.Error("signature verifies for another ts")
		}
	}
	for _, c := range f.SignResponse {
		v, _ := jsjson.Decode(c.In)
		got := signResponse(v, key)
		if got.Get2("body") != c.Out.Body || got.Get2("sig") != c.Out.Sig {
			t.Errorf("signResponse: got %v want %+v", got, c.Out)
		}
	}
}
