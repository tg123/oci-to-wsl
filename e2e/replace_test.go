//go:build e2e

// Package e2e contains end-to-end tests that drive a real `oci-to-wsl.exe`
// binary against a real `wsl.exe` host. They are guarded by the `e2e` build
// tag so `go test ./...` from a developer machine stays hermetic; the CI
// workflow opts in with `go test -tags=e2e ./e2e/...`.
package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFilesReplaceSemantics imports the alpine image twice, staging a
// host-side directory at /etc/apk. The replacement tree intentionally does
// NOT contain a "repositories" file (which the upstream alpine image ships
// at /etc/apk/repositories): that asymmetry is what lets us distinguish
// "replaced the whole subtree" from "overlayed onto the existing subtree".
//
//   - default replace (omitted)   -> upstream /etc/apk/repositories must be gone
//   - explicit replace: false     -> upstream /etc/apk/repositories must remain
func TestFilesReplaceSemantics(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("e2e test requires windows + wsl.exe")
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Skip("wsl.exe not on PATH")
	}

	exe := findOciToWslExe(t)

	work := t.TempDir()
	repl := filepath.Join(work, "apk-replacement")
	if err := os.MkdirAll(repl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repl, "marker.txt"), []byte("replace-marker"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		distro    string
		profile   string
		wantRepos bool // whether upstream /etc/apk/repositories must remain
	}{
		{
			name:      "default replace drops upstream subtree",
			distro:    "e2e-replace-default",
			profile:   "name: e2e-replace-default\nimage: alpine:latest\nfiles:\n  - src: ./apk-replacement\n    dst: /etc/apk\n",
			wantRepos: false,
		},
		{
			name:      "explicit replace=false preserves upstream subtree",
			distro:    "e2e-replace-false",
			profile:   "name: e2e-replace-false\nimage: alpine:latest\nfiles:\n  - src: ./apk-replacement\n    dst: /etc/apk\n    replace: false\n",
			wantRepos: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			profilePath := filepath.Join(work, tc.distro+".yaml")
			if err := os.WriteFile(profilePath, []byte(tc.profile), 0o644); err != nil {
				t.Fatal(err)
			}
			installDir := filepath.Join(work, "wsl-"+tc.distro)

			t.Cleanup(func() {
				// Best-effort unregister so a failing assertion still
				// leaves the runner in a clean state for the next run.
				_ = exec.Command("wsl.exe", "--unregister", tc.distro).Run()
			})

			cmd := exec.Command(exe, "--profile", profilePath, "--dir", installDir)
			cmd.Dir = work // so the profile's `./apk-replacement` resolves
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("oci-to-wsl failed: %v\n%s", err, out)
			}

			// The marker we staged must always be present.
			marker, err := wslRun(tc.distro, "/bin/cat", "/etc/apk/marker.txt")
			if err != nil {
				t.Fatalf("marker.txt missing in %s: %v\n%s", tc.distro, err, marker)
			}
			if !strings.Contains(marker, "replace-marker") {
				t.Errorf("marker content: got %q, want it to contain %q", marker, "replace-marker")
			}

			// Now assert presence/absence of the upstream /etc/apk/repositories.
			_, reposErr := wslRun(tc.distro, "/bin/cat", "/etc/apk/repositories")
			if tc.wantRepos {
				if reposErr != nil {
					ls, _ := wslRun(tc.distro, "/bin/ls", "-la", "/etc/apk")
					t.Fatalf("upstream /etc/apk/repositories missing under replace=false: %v\nls:\n%s", reposErr, ls)
				}
			} else {
				if reposErr == nil {
					ls, _ := wslRun(tc.distro, "/bin/ls", "-la", "/etc/apk")
					t.Fatalf("upstream /etc/apk/repositories still present under default replace\nls:\n%s", ls)
				}
			}
		})
	}
}

// findOciToWslExe locates the built binary. CI builds it at the repo root as
// `oci-to-wsl.exe`; a developer can override with OCI_TO_WSL_BIN.
func findOciToWslExe(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OCI_TO_WSL_BIN"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join("..", "oci-to-wsl.exe"),
		"oci-to-wsl.exe",
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Fatal("oci-to-wsl.exe not found; build it at repo root or set OCI_TO_WSL_BIN")
	return ""
}

// wslRun executes a command inside the given distribution as root and
// returns its combined stdout/stderr. A non-nil error means wsl.exe (or the
// command inside the distro) exited non-zero.
func wslRun(distro string, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("wslRun: empty argv")
	}
	args := append([]string{"-d", distro, "--user", "root", "--exec"}, argv...)
	out, err := exec.Command("wsl.exe", args...).CombinedOutput()
	return string(out), err
}
