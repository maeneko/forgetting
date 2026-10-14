package jsjson

import (
	"encoding/json"
	"os"
	"testing"
)

type fixture struct {
	Strings []struct{ In, Out string } `json:"strings"`
	Numbers []struct {
		In  json.RawMessage `json:"in"`
		Out string          `json:"out"`
		Str string          `json:"str"`
	} `json:"numbers"`
	Objects []struct {
		In      json.RawMessage `json:"in"`
		Compact string          `json:"compact"`
		Indent2 string          `json:"indent2"`
	} `json:"objects"`
}

// Эталон — вывод JSON.stringify самого Node (tools/golden/jsjson.mjs).
func TestAgainstNode(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/jsjson.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Strings {
		if got := string(MustMarshal(c.In)); got != c.Out {
			t.Errorf("string %q: got %s want %s", c.In, got, c.Out)
		}
	}
	for _, c := range f.Numbers {
		v, err := Decode(c.In)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := v.(string); ok { // NaN / Infinity переданы строкой
			_ = s
			continue
		}
		x := v.(float64)
		if got := Number(x); got != c.Str {
			t.Errorf("String(%s): got %s want %s", c.In, got, c.Str)
		}
		if got := string(MustMarshal(x)); got != c.Out {
			t.Errorf("JSON.stringify(%s): got %s want %s", c.In, got, c.Out)
		}
	}
	for _, c := range f.Objects {
		v, err := Decode(c.In)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(MustMarshal(v)); got != c.Compact {
			t.Errorf("compact:\n got %s\nwant %s", got, c.Compact)
		}
		got, _ := MarshalIndent(v, "  ")
		if string(got) != c.Indent2 {
			t.Errorf("indent:\n got %s\nwant %s", got, c.Indent2)
		}
	}
}

func TestStructOrderAndOmit(t *testing.T) {
	type S struct {
		B    string `json:"b"`
		A    int    `json:"a"`
		Opt  string `json:"opt,omitempty"`
		Skip string `json:"-"`
	}
	if got := string(MustMarshal(S{B: "x", A: 1})); got != `{"b":"x","a":1}` {
		t.Fatal(got)
	}
	o := Object{{"x", 1}, {"gone", Undefined}, {"y", []string(nil)}}
	if got := string(MustMarshal(o)); got != `{"x":1,"y":[]}` {
		t.Fatal(got)
	}
}
