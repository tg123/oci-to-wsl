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
	"time"

	"github.com/schollz/progressbar/v3"
	gowsl "github.com/ubuntu/gowsl"
)

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

// RunCommand executes a shell command inside an existing WSL distribution.
//
// stdout/stderr of the in-distro process are streamed to the parent process's
// stdout/stderr. Because gowsl uses WslLaunch under the hood, the output is
// whatever bytes the Linux program writes (typically UTF-8) — there is no
// UTF-16 transcoding step as there is with `wsl.exe` on its own.
func RunCommand(distro, command string) error {
	ctx := context.Background()

	slog.Info("running command in WSL distribution", "distro", distro, "command", command)
	start := time.Now()

	d := gowsl.NewDistro(ctx, distro)
	cmd := d.Command(ctx, command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	slog.Debug("wsl command finished", "distro", distro, "duration", time.Since(start))
	if err != nil {
		return fmt.Errorf("command %q failed in %q: %w", command, distro, err)
	}
	return nil
}
