package cli

import "testing"

func TestUpdateEnvVar(t *testing.T) {
	for _, c := range []struct{ in, key, val, want string }{
		{"", "A", "1", "\nA=1\n"},
		{"A=0\nB=2\n", "A", "1", "A=1\nB=2\n"},
		{"B=2\n\n\n", "A", "1", "B=2\nA=1\n"},
		{"XA=0\nA=0\n", "A", "$&1", "XA=0\nA=$&1\n"},
		{"A=0\nA=9\n", "A", "1", "A=1\nA=9\n"}, // как String.replace без g — только первое
	} {
		if got := updateEnvVar(c.in, c.key, c.val); got != c.want {
			t.Errorf("updateEnvVar(%q, %s, %s) = %q, want %q", c.in, c.key, c.val, got, c.want)
		}
	}
	if got := removeEnvVar("A=1\nJOIN_SECRET=x\nB=2\n", "JOIN_SECRET"); got != "A=1\nB=2\n" {
		t.Errorf("removeEnvVar: %q", got)
	}
}

func TestAreaOf(t *testing.T) {
	for owner, want := range map[string]string{
		"abcdef123456/m1/d2": "abcdef123456",
		"m1/d2":              "legacy",
		"alice":              "",
		"ABCDEF123456/m1/d2": "",
		"abc/m1/d2":          "",
		"":                   "",
	} {
		if got := areaOf(owner); got != want {
			t.Errorf("areaOf(%q) = %q, want %q", owner, got, want)
		}
	}
}

func TestPeersWord(t *testing.T) {
	for n, want := range map[int]string{0: "пиров", 1: "пир", 2: "пира", 4: "пира", 5: "пиров", 11: "пиров", 12: "пиров",
		21: "пир", 22: "пира", 101: "пир", 111: "пиров", 114: "пиров", 1004: "пира"} {
		if got := peersWord(n); got != want {
			t.Errorf("peersWord(%d) = %s, want %s", n, got, want)
		}
	}
}
