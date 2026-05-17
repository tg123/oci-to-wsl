package registry

import (
	"testing"
)

func TestIsLocalDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"YES", true},
	}
	for _, tc := range cases {
		t.Setenv(envDisableLocal, tc.val)
		if got := isLocalDisabled(); got != tc.want {
			t.Errorf("isLocalDisabled() with %s=%q = %v, want %v", envDisableLocal, tc.val, got, tc.want)
		}
	}
}

func TestLoadFromLocal_DisabledReturnsNotFound(t *testing.T) {
	t.Setenv(envDisableLocal, "1")
	img, found, err := loadFromLocal("alpine:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found || img != nil {
		t.Fatalf("expected (nil, false, nil) when local lookup is disabled, got (%v, %v)", img, found)
	}
}
