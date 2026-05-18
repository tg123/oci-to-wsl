// Package logging configures the process-wide slog logger used by oci-to-wsl.
//
// The logger writes to stderr (so it does not pollute stdout, which is
// reserved for command results such as the resolved tar path or login
// destination) and uses a level controlled by the --loglevel CLI flag
// (debug, info, warn, error; default info).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// DefaultLevel is the level used when --loglevel is not provided.
const DefaultLevel = "info"

// Setup installs a slog text handler at the requested level as the default
// logger. The level string is matched case-insensitively against
// debug/info/warn/error; any other value returns an error.
func Setup(level string) error {
	lvl, err := ParseLevel(level)
	if err != nil {
		return err
	}
	setup(os.Stderr, lvl)
	return nil
}

// ParseLevel converts a human-readable level string into a slog.Level.
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid --loglevel %q (expected one of: debug, info, warn, error)", level)
	}
}

// setup installs a text handler writing to w at the given level. Split out
// from Setup to keep tests independent of os.Stderr.
func setup(w io.Writer, lvl slog.Level) {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}
