// Package wsl provides helpers for managing WSL (Windows Subsystem for Linux) distributions.
package wsl

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/schollz/progressbar/v3"
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

// Import creates a new WSL distribution by calling "wsl.exe --import".
// This is Windows-only; on non-Windows hosts wsl.exe cannot be located and
// findWSL returns an error indicating WSL is unavailable on this OS.
func Import(opts ImportOptions) error {
	wslPath, err := findWSL()
	if err != nil {
		return err
	}

	args := []string{"--import", opts.Name, opts.InstallDir, opts.RootfsTar}
	fmt.Printf("Creating WSL distribution %q from %s ...\n", opts.Name, opts.RootfsTar)
	cmd := exec.Command(wslPath, args...) //nolint:gosec
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Spinner while wsl.exe runs (it gives no native progress output).
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

	out, err := cmd.CombinedOutput()
	close(done)
	_ = spinner.Close()
	if len(out) > 0 {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("wsl --import failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunCommand executes a shell command inside an existing WSL distribution.
func RunCommand(distro, command string) error {
	wslPath, err := findWSL()
	if err != nil {
		return err
	}

	args := []string{"--distribution", distro, "--", "sh", "-c", command}
	fmt.Printf("[%s] $ %s\n", distro, command)
	cmd := exec.Command(wslPath, args...) //nolint:gosec
	cmd.Stdout = nil
	cmd.Stderr = nil
	out, err := cmd.CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("command %q failed in %q: %w", command, distro, err)
	}
	return nil
}

// findWSL locates wsl.exe; it must be available on the PATH or at the standard Windows location.
func findWSL() (string, error) {
	if path, err := exec.LookPath("wsl.exe"); err == nil {
		return path, nil
	}
	// Fall back to the well-known system location on Windows.
	const winPath = `C:\Windows\System32\wsl.exe`
	if fi, err := os.Stat(winPath); err == nil && !fi.IsDir() {
		return winPath, nil
	}
	return "", fmt.Errorf("wsl.exe not found on %s; ensure you are running on Windows with WSL installed", runtime.GOOS)
}
