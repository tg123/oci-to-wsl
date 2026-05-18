package wsl

import (
	"strings"
	"testing"
)

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

func TestRunCommand_RejectsInvalidEnvVarName(t *testing.T) {
	// We can't actually launch WSL in unit tests, but RunCommand must
	// validate env var names *before* attempting any in-distro work, so
	// the error surfaces synchronously regardless of platform.
	cases := []string{
		"BAD NAME",    // space
		"BAD;NAME",    // shell metachar
		"BAD=NAME",    // equals
		"$(injected)", // command substitution
		"1STARTS_WITH_DIGIT",
		"name-with-dash",
	}
	for _, name := range cases {
		err := RunCommand("does-not-matter", "true", RunOptions{
			Env: []EnvVar{{Name: name, Value: "x"}},
		})
		if err == nil {
			t.Errorf("RunCommand with env name %q: expected error, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), "invalid env var name") {
			t.Errorf("RunCommand with env name %q: error = %v, want 'invalid env var name'", name, err)
		}
	}
}
