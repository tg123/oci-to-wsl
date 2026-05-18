package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"", slog.LevelInfo, true},
		{"info", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{" Debug ", slog.LevelDebug, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"trace", 0, false},
		{"verbose", 0, false},
	}
	for _, tc := range cases {
		got, err := ParseLevel(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ParseLevel(%q) unexpected error: %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		} else if err == nil {
			t.Errorf("ParseLevel(%q) expected error, got nil", tc.in)
		}
	}
}

func TestSetupRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	setup(&buf, slog.LevelInfo)

	slog.Debug("debug-msg")
	slog.Info("info-msg")
	out := buf.String()
	if strings.Contains(out, "debug-msg") {
		t.Errorf("debug message leaked at info level: %q", out)
	}
	if !strings.Contains(out, "info-msg") {
		t.Errorf("info message missing: %q", out)
	}

	buf.Reset()
	setup(&buf, slog.LevelDebug)
	slog.Debug("debug-msg")
	if !strings.Contains(buf.String(), "debug-msg") {
		t.Errorf("debug message missing at debug level: %q", buf.String())
	}
}
