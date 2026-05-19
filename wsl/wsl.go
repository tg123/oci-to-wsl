// Package wsl provides helpers for managing WSL (Windows Subsystem for Linux) distributions.
//
// It is a thin wrapper around github.com/ubuntu/gowsl, which talks to the
// native WslApi.dll Win32 surface (and falls back to wsl.exe internally for
// the few operations that have no API equivalent). Using the API avoids the
// UTF-16 LE output that wsl.exe produces by default, so callers do not need
// to set WSL_UTF8=1 in their environment for output to be readable.
package wsl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/schollz/progressbar/v3"
	gowsl "github.com/ubuntu/gowsl"
)

// envVarNameRE matches a valid POSIX-style environment variable name
// (`[A-Za-z_][A-Za-z0-9_]*`). Names are written directly into the
// generated shell script as `export <NAME>=...`, so anything outside
// this character set (spaces, `;`, `$()`, `=`, etc.) could break the
// shell parse or inject extra statements; RunCommand rejects such
// names before constructing the command.
var envVarNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runAsUserRE matches a conservative POSIX username (the NAME_REGEX
// used by adduser/useradd: lowercase letter or underscore, followed
// by lowercase letters/digits/underscores/hyphens, optionally ending
// in `$`). The value is interpolated into `su <name> -c '...'`, so a
// name with whitespace or shell metacharacters could inject extra
// arguments or statements; RunCommand rejects such values up front.
var runAsUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

// ImportOptions controls how a WSL distribution is created.
type ImportOptions struct {
	// Name is the WSL distribution name.
	Name string

	// InstallDir is the directory where the WSL virtual disk will be stored.
	InstallDir string

	// RootfsTar is the path to the rootfs tar (or tar.gz) file to import.
	RootfsTar string
}

// Import creates a new WSL distribution from a rootfs tar.
//
// On non-Windows hosts the underlying gowsl backend returns an error,
// matching the previous behaviour of the manual wsl.exe wrapper.
func Import(opts ImportOptions) error {
	ctx := context.Background()

	slog.Info("creating WSL distribution",
		"name", opts.Name,
		"install_dir", opts.InstallDir,
		"rootfs_tar", opts.RootfsTar,
	)
	start := time.Now()

	// Spinner while the import runs (gowsl/wsl.exe gives no progress output).
	spinner := progressbar.NewOptions(-1,
		progressbar.OptionSetDescription("Importing into WSL"),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionOnCompletion(func() { fmt.Fprintln(os.Stderr) }),
	)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = spinner.Add(1)
			}
		}
	}()

	_, err := gowsl.Import(ctx, opts.Name, opts.RootfsTar, opts.InstallDir)
	close(done)
	_ = spinner.Close()
	slog.Debug("wsl import finished", "duration", time.Since(start))
	if err != nil {
		return fmt.Errorf("wsl import failed: %w", err)
	}
	return nil
}

// EnvVar is a single environment variable to export inside the WSL
// distribution before running an init command. Value is used verbatim
// (no host-side expansion happens here — callers should pre-expand any
// host-side references they want to forward).
type EnvVar struct {
	Name  string
	Value string
}

// RunOptions tunes how RunCommand executes a command inside a distro.
type RunOptions struct {
	// Env is exported in the in-distro shell before the command runs.
	Env []EnvVar
	// RunAs, when non-empty, runs the command as the named in-distro
	// user via `su <user> -c <body>`. When empty the command runs as
	// the distribution's default user (root for a freshly imported
	// rootfs unless /etc/wsl.conf overrides it).
	RunAs string
}

// RunCommand executes a shell command inside an existing WSL distribution.
//
// stdout/stderr of the in-distro process are streamed to the parent process's
// stdout/stderr. Because gowsl uses WslLaunch under the hood, the output is
// whatever bytes the Linux program writes (typically UTF-8) — there is no
// UTF-16 transcoding step as there is with `wsl.exe` on its own.
//
// If opts.Env is non-empty, each variable is exported in the in-distro
// shell before command runs. If opts.RunAs is non-empty, the whole body
// (exports + command) is executed via `su <user> -c <body>` so it runs
// as that user. Values are single-quote shell-escaped so arbitrary
// content (including quotes and `$`) is forwarded literally.
func RunCommand(distro, command string, opts RunOptions) error {
	ctx := context.Background()

	slog.Info("running command in WSL distribution",
		"distro", distro,
		"command", command,
		"env_count", len(opts.Env),
		"run_as", opts.RunAs,
	)
	start := time.Now()

	// Build the in-shell body: exports first, then the user's command.
	body := command
	if len(opts.Env) > 0 {
		var b strings.Builder
		for _, e := range opts.Env {
			if !envVarNameRE.MatchString(e.Name) {
				return fmt.Errorf("invalid env var name %q: must match [A-Za-z_][A-Za-z0-9_]*", e.Name)
			}
			b.WriteString("export ")
			b.WriteString(e.Name)
			b.WriteString("=")
			b.WriteString(shellSingleQuote(e.Value))
			b.WriteString("; ")
		}
		b.WriteString(command)
		body = b.String()
	}

	// If a target user was requested, wrap with `su` so the command
	// (including the exports above) runs in that user's shell. `su`
	// is universally available (util-linux on glibc distros, busybox
	// applet on alpine) and does not prompt for a password when the
	// caller is already root, which is the case for a freshly
	// imported OCI rootfs.
	full := body
	if opts.RunAs != "" {
		if !runAsUserRE.MatchString(opts.RunAs) {
			return fmt.Errorf("invalid run_as user %q: must match [a-z_][a-z0-9_-]*\\$?", opts.RunAs)
		}
		full = "su " + opts.RunAs + " -c " + shellSingleQuote(body)
	}

	d := gowsl.NewDistro(ctx, distro)
	cmd := d.Command(ctx, full)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	slog.Debug("wsl command finished", "distro", distro, "duration", time.Since(start))
	if err != nil {
		return fmt.Errorf("command %q failed in %q: %w", command, distro, err)
	}
	return nil
}

// shellSingleQuote wraps s in POSIX single quotes, escaping any embedded
// single quotes as the conventional `'\”` sequence (close-quote,
// backslash-escaped literal quote, re-open quote). The result is safe
// to use as a single shell word in any POSIX-compatible shell.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
