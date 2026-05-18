//go:build e2e

// Package main_test contains end-to-end tests for oci-to-wsl that exercise
// the real `oci-to-wsl.exe` binary against a real WSL installation on
// Windows. They are gated behind the `e2e` build tag so they are skipped by
// the default `go test ./...` run on Linux contributors and CI lint/test
// jobs; the dedicated E2E workflow opts in with `-tags=e2e`.
//
// Adding a new scenario is intentionally cheap: append another entry to
// the e2eCases slice in TestE2E. Each case owns its own setup, the
// oci-to-wsl invocation, and a list of verification steps to run inside
// the resulting distribution. No new workflow job or YAML editing is
// required.
package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ociToWSL caches the absolute path to the freshly built oci-to-wsl.exe so
// every subtest reuses the same binary.
var (
	ociToWSLOnce sync.Once
	ociToWSLPath string
	ociToWSLErr  error
)

// buildBinary builds oci-to-wsl.exe in a temp dir (or reuses the OCI_TO_WSL_BIN
// override) and returns its absolute path. The override lets the workflow
// build the binary once with its own flags and just point the tests at it.
func buildBinary(t *testing.T) string {
	t.Helper()
	ociToWSLOnce.Do(func() {
		if override := os.Getenv("OCI_TO_WSL_BIN"); override != "" {
			abs, err := filepath.Abs(override)
			if err != nil {
				ociToWSLErr = fmt.Errorf("resolving OCI_TO_WSL_BIN %q: %w", override, err)
				return
			}
			if _, err := os.Stat(abs); err != nil {
				ociToWSLErr = fmt.Errorf("OCI_TO_WSL_BIN %q: %w", abs, err)
				return
			}
			ociToWSLPath = abs
			return
		}
		dir, err := os.MkdirTemp("", "oci-to-wsl-e2e-bin-")
		if err != nil {
			ociToWSLErr = err
			return
		}
		exe := "oci-to-wsl"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		out := filepath.Join(dir, exe)
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if buildOut, err := cmd.CombinedOutput(); err != nil {
			ociToWSLErr = fmt.Errorf("go build oci-to-wsl: %w\n%s", err, buildOut)
			return
		}
		ociToWSLPath = out
	})
	if ociToWSLErr != nil {
		t.Fatalf("building oci-to-wsl: %v", ociToWSLErr)
	}
	return ociToWSLPath
}

// runOutput runs name with args and returns combined stdout+stderr along
// with any error. The full output is always returned so callers can include
// it in t.Fatalf messages.
func runOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// wslExec runs `wsl.exe -d <distro> --user root -- sh -c <script>` and
// returns the trimmed stdout/stderr plus any error.
func wslExec(distro, script string) (string, error) {
	out, err := runOutput("wsl.exe", "-d", distro, "--user", "root", "--", "sh", "-c", script)
	return strings.TrimSpace(out), err
}

// verify is a single assertion to run against a freshly imported
// distribution. Each verify shells out to `sh -c script` inside the distro
// and either checks the trimmed output against want (when want != "") or
// just asserts the script exited 0.
type verify struct {
	name    string
	script  string
	want    string // expected exact trimmed output; "" means "only check exit code"
	wantSub string // substring required in output; "" means no substring check
}

// e2eCase is one full end-to-end scenario. setup runs before the
// oci-to-wsl invocation; if it returns a non-nil cleanup, that cleanup
// runs via t.Cleanup. args is appended to the oci-to-wsl.exe command line
// (the binary path is filled in by the runner). distro is the WSL
// distribution the scenario creates and is also used to unregister it on
// teardown.
type e2eCase struct {
	name     string
	distro   string
	setup    func(t *testing.T, workDir string) (args []string)
	verifies []verify
}

func TestE2E(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("e2e tests require Windows + wsl.exe (GOOS=%s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		t.Skipf("wsl.exe not found on PATH: %v", err)
	}

	bin := buildBinary(t)

	cases := []e2eCase{
		{
			name:   "alpine_import",
			distro: "e2e-alpine",
			setup: func(t *testing.T, workDir string) []string {
				installDir := filepath.Join(workDir, "wsl-e2e-alpine")
				return []string{
					"--image", "alpine:latest",
					"--name", "e2e-alpine",
					"--dir", installDir,
				}
			},
			verifies: []verify{
				{name: "boot", script: "/bin/true"},
				{name: "ls_root", script: "ls -la /"},
				{name: "alpine_release_nonempty", script: "test -s /etc/alpine-release && cat /etc/alpine-release", wantSub: "."},
				{name: "uname", script: "uname -a", wantSub: "Linux"},
				{name: "echo", script: "echo hello-from-wsl", want: "hello-from-wsl"},
			},
		},
		{
			name:   "profile_copies_and_init_cmds",
			distro: "e2e-copy",
			setup: func(t *testing.T, workDir string) []string {
				// Lay out the files the profile references. Critically the
				// shell script must use LF line endings: a CRLF shebang
				// turns "#!/bin/sh\r" into a missing interpreter and
				// busybox reports a cryptic "not found" at exec time.
				mustMkdir(t, filepath.Join(workDir, "scripts"))
				mustMkdir(t, filepath.Join(workDir, "assets", "nested"))

				script := "#!/bin/sh\n" +
					"set -e\n" +
					"echo \"bootstrap-ran\" > /tmp/bootstrap-marker\n" +
					"cat /opt/assets/hello.txt > /tmp/assets-hello\n" +
					"cat /opt/assets/nested/inner.txt > /tmp/assets-inner\n"
				mustWrite(t, filepath.Join(workDir, "scripts", "bootstrap.sh"), script)
				mustWrite(t, filepath.Join(workDir, "assets", "hello.txt"), "hello-from-host")
				mustWrite(t, filepath.Join(workDir, "assets", "nested", "inner.txt"), "nested-from-host")

				profile := `name: e2e-copy
image: alpine:latest
copies:
  - src: ./scripts/bootstrap.sh
    dst: /usr/local/bin/bootstrap.sh
    mode: "0755"
  - src: ./assets
    dst: /opt/assets
    mode: "0777"
init_cmds:
  - /usr/local/bin/bootstrap.sh
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-copy")
				// The profile uses relative `src:` paths, so oci-to-wsl
				// must be invoked with workDir as cwd. We achieve that by
				// passing absolute paths to --profile and --dir and
				// chdir'ing in the runner below via a sentinel arg list.
				return []string{
					"--profile", profilePath,
					"--dir", installDir,
				}
			},
			verifies: []verify{
				{name: "bootstrap_content", script: "cat /usr/local/bin/bootstrap.sh", wantSub: "bootstrap-ran"},
				{name: "bootstrap_mode", script: "stat -c '%a' /usr/local/bin/bootstrap.sh", want: "755"},
				{name: "hello_content", script: "cat /opt/assets/hello.txt", want: "hello-from-host"},
				{name: "inner_content", script: "cat /opt/assets/nested/inner.txt", want: "nested-from-host"},
				{name: "assets_dir_mode", script: "stat -c '%a' /opt/assets", want: "777"},
				{name: "assets_recursive_mode", script: "stat -c '%a' /opt/assets/nested/inner.txt", want: "777"},
				// init_cmds ran after copy
				{name: "bootstrap_marker", script: "cat /tmp/bootstrap-marker", wantSub: "bootstrap-ran"},
				{name: "init_read_asset", script: "cat /tmp/assets-hello", wantSub: "hello-from-host"},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			// Always try to unregister at the end - the case may have
			// failed mid-import and left a partial registration behind.
			t.Cleanup(func() {
				out, err := runOutput("wsl.exe", "--unregister", tc.distro)
				if err != nil {
					t.Logf("wsl --unregister %s failed (non-fatal): %v\n%s", tc.distro, err, out)
				}
			})

			args := tc.setup(t, workDir)
			t.Logf("running %s %s (cwd=%s)", bin, strings.Join(args, " "), workDir)

			cmd := exec.Command(bin, args...)
			cmd.Dir = workDir
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			if err := cmd.Run(); err != nil {
				t.Fatalf("oci-to-wsl failed: %v\n--- output ---\n%s", err, buf.String())
			}
			t.Logf("oci-to-wsl output:\n%s", buf.String())

			// Force the distro to fully boot before issuing verifications;
			// the first command sometimes races with WSL's VM startup.
			if out, err := runOutput("wsl.exe", "-d", tc.distro, "--user", "root", "--exec", "/bin/true"); err != nil {
				t.Fatalf("failed to start %s: %v\n%s", tc.distro, err, out)
			}

			for _, v := range tc.verifies {
				v := v
				t.Run(v.name, func(t *testing.T) {
					got, err := wslExec(tc.distro, v.script)
					if err != nil {
						t.Fatalf("script %q failed: %v\noutput: %s", v.script, err, got)
					}
					if v.want != "" && got != v.want {
						t.Fatalf("script %q: got %q, want %q", v.script, got, v.want)
					}
					if v.wantSub != "" && !strings.Contains(got, v.wantSub) {
						t.Fatalf("script %q: output %q missing substring %q", v.script, got, v.wantSub)
					}
				})
			}
		})
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
