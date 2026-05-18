package wsl

import (
	"strings"
	"testing"
)

func TestWSLEnvAddsUTF8WhenMissing(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/root"}
	out := wslEnv(in)

	if count := countKey(out, "WSL_UTF8"); count != 1 {
		t.Fatalf("expected exactly one WSL_UTF8 entry, got %d in %v", count, out)
	}
	if !contains(out, "WSL_UTF8=1") {
		t.Fatalf("expected WSL_UTF8=1 in %v", out)
	}
	if !contains(out, "PATH=/usr/bin") || !contains(out, "HOME=/root") {
		t.Fatalf("original entries lost: %v", out)
	}
}

func TestWSLEnvOverridesExisting(t *testing.T) {
	in := []string{"WSL_UTF8=0", "FOO=bar"}
	out := wslEnv(in)

	if count := countKey(out, "WSL_UTF8"); count != 1 {
		t.Fatalf("expected exactly one WSL_UTF8 entry, got %d in %v", count, out)
	}
	if !contains(out, "WSL_UTF8=1") {
		t.Fatalf("expected WSL_UTF8=1 to override existing value: %v", out)
	}
	if contains(out, "WSL_UTF8=0") {
		t.Fatalf("old WSL_UTF8=0 should be removed: %v", out)
	}
}

func TestWSLEnvOverridesCaseInsensitive(t *testing.T) {
	// Windows env var names are case-insensitive; ensure we replace rather
	// than emit two conflicting entries.
	in := []string{"wsl_utf8=0", "FOO=bar"}
	out := wslEnv(in)

	if count := countKeyFold(out, "WSL_UTF8"); count != 1 {
		t.Fatalf("expected exactly one WSL_UTF8 entry (case-insensitive), got %d in %v", count, out)
	}
	if !contains(out, "WSL_UTF8=1") {
		t.Fatalf("expected canonical WSL_UTF8=1: %v", out)
	}
}

func TestWSLEnvDoesNotMutateInput(t *testing.T) {
	in := []string{"FOO=bar"}
	_ = wslEnv(in)
	if len(in) != 1 || in[0] != "FOO=bar" {
		t.Fatalf("input slice was mutated: %v", in)
	}
}

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func countKey(env []string, key string) int {
	n := 0
	prefix := key + "="
	for _, e := range env {
		if e == key || strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func countKeyFold(env []string, key string) int {
	n := 0
	for _, e := range env {
		eq := strings.IndexByte(e, '=')
		name := e
		if eq >= 0 {
			name = e[:eq]
		}
		if strings.EqualFold(name, key) {
			n++
		}
	}
	return n
}
