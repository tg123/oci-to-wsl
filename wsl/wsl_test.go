package wsl

import "testing"

func TestShellSingleQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"alice", "'alice'"},
		{"hello world", "'hello world'"},
		{"a'b", `'a'\''b'`},
		{`'`, `''\'''`},
		{"$USER", "'$USER'"}, // dollar must NOT expand inside single quotes
		{"a\nb", "'a\nb'"},
	}
	for _, c := range cases {
		if got := shellSingleQuote(c.in); got != c.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
