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
		"",            // empty (typo / missing name)
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

func TestRunCommand_RejectsInvalidRunAs(t *testing.T) {
	// run_as is concatenated into `su <name> -c '...'`, so any value
	// outside a strict POSIX username regex would let a malicious
	// profile add extra args or shell statements; RunCommand must
	// reject such values before launching anything.
	cases := []string{
		"alice bob",     // space
		"alice;reboot",  // shell metachar
		"alice|whoami",  // pipe
		"alice`id`",     // backticks
		"$(id)",         // command substitution
		"-froot",        // leading dash (looks like an su flag)
		"Alice",         // uppercase (not a valid login name)
		"alice/../root", // path traversal
		"alice'whoami'", // embedded quote
	}
	for _, name := range cases {
		err := RunCommand("does-not-matter", "true", RunOptions{RunAs: name})
		if err == nil {
			t.Errorf("RunCommand with run_as %q: expected error, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), "invalid run_as user") {
			t.Errorf("RunCommand with run_as %q: error = %v, want 'invalid run_as user'", name, err)
		}
	}
}
