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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ociToWSLBinary returns the absolute path to the oci-to-wsl binary that
// the e2e suite should exercise. The caller is responsible for building
// the binary and exporting its path via OCI_TO_WSL_BIN before invoking
// `go test -tags=e2e` — this keeps the test runner narrowly focused on
// exercising the binary rather than building it.
func ociToWSLBinary(t *testing.T) string {
	t.Helper()
	override := os.Getenv("OCI_TO_WSL_BIN")
	if override == "" {
		t.Fatal("OCI_TO_WSL_BIN must be set to the absolute path of a prebuilt oci-to-wsl binary")
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		t.Fatalf("resolving OCI_TO_WSL_BIN %q: %v", override, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("OCI_TO_WSL_BIN %q: %v", abs, err)
	}
	return abs
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
// oci-to-wsl invocation and returns the CLI args to append to the
// oci-to-wsl binary path. distro is the WSL distribution the scenario
// creates and is also used to unregister it on teardown.
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

	bin := ociToWSLBinary(t)

	cases := []e2eCase{
		// NOTE: distro names below are also unregistered up-front (and on
		// t.Cleanup per case) so a prior aborted run cannot cause
		// `wsl --import` to fail with "distribution already registered".
		// The CI workflow therefore does not need its own teardown step.
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
				{name: "alpine_release_nonempty", script: "test -s /etc/alpine-release && cat /etc/alpine-release"},
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
files:
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
		{
			// Default replace=true: the staged directory at /etc/apk fully
			// replaces the upstream subtree, so alpine's stock
			// /etc/apk/repositories must NOT be present.
			name:   "files_replace_default_drops_upstream",
			distro: "e2e-replace-default",
			setup: func(t *testing.T, workDir string) []string {
				mustMkdir(t, filepath.Join(workDir, "apk-replacement"))
				mustWrite(t, filepath.Join(workDir, "apk-replacement", "marker.txt"), "replace-marker")

				profile := `name: e2e-replace-default
image: alpine:latest
files:
  - src: ./apk-replacement
    dst: /etc/apk
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-replace-default")
				return []string{"--profile", profilePath, "--dir", installDir}
			},
			verifies: []verify{
				{name: "marker_present", script: "cat /etc/apk/marker.txt", want: "replace-marker"},
				// `test ! -e` succeeds (exit 0) only when the file is
				// absent — exactly what we want for the default replace.
				{name: "upstream_repositories_gone", script: "test ! -e /etc/apk/repositories"},
			},
		},
		{
			// Explicit replace=false: the staged directory at /etc/apk
			// overlays onto the upstream subtree, so alpine's stock
			// /etc/apk/repositories must still be present.
			name:   "files_replace_false_preserves_upstream",
			distro: "e2e-replace-false",
			setup: func(t *testing.T, workDir string) []string {
				mustMkdir(t, filepath.Join(workDir, "apk-replacement"))
				mustWrite(t, filepath.Join(workDir, "apk-replacement", "marker.txt"), "replace-marker")

				profile := `name: e2e-replace-false
image: alpine:latest
files:
  - src: ./apk-replacement
    dst: /etc/apk
    replace: false
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-replace-false")
				return []string{"--profile", profilePath, "--dir", installDir}
			},
			verifies: []verify{
				{name: "marker_present", script: "cat /etc/apk/marker.txt", want: "replace-marker"},
				{name: "upstream_repositories_present", script: "test -s /etc/apk/repositories"},
			},
		},
		{
			// Inline content / content_base64: no host file is read; the
			// body comes straight from the profile YAML.
			name:   "files_inline_content",
			distro: "e2e-content",
			setup: func(t *testing.T, workDir string) []string {
				// "binary-from-base64\n" base64-encoded.
				profile := `name: e2e-content
image: alpine:latest
files:
  - dst: /etc/motd
    content: |
      inline-motd-from-profile
    mode: "0644"
  - dst: /opt/binary.bin
    content_base64: YmluYXJ5LWZyb20tYmFzZTY0Cg==
    mode: "0600"
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-content")
				return []string{"--profile", profilePath, "--dir", installDir}
			},
			verifies: []verify{
				{name: "motd_content", script: "cat /etc/motd", wantSub: "inline-motd-from-profile"},
				{name: "motd_mode", script: "stat -c '%a' /etc/motd", want: "644"},
				{name: "binary_content", script: "cat /opt/binary.bin", want: "binary-from-base64"},
				{name: "binary_mode", script: "stat -c '%a' /opt/binary.bin", want: "600"},
			},
		},
		{
			name:   "profile_wsl_conf_ini_content",
			distro: "e2e-wslconf",
			setup: func(t *testing.T, workDir string) []string {
				// alpine:latest does not ship /etc/wsl.conf, so merge
				// effectively writes a fresh file. The [user] section
				// uses %WSL_CONF_E2E_USER% which must be expanded to
				// "alice" by ExpandEnvVars at profile-load time.
				t.Setenv("WSL_CONF_E2E_USER", "alice")
				profile := `name: e2e-wslconf
image: alpine:latest
wsl_conf:
  mode: merge
  content: |
    [boot]
    systemd=false
    [user]
    default=%WSL_CONF_E2E_USER%
    [interop]
    appendWindowsPath=false
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)
				installDir := filepath.Join(workDir, "wsl-e2e-wslconf")
				return []string{
					"--profile", profilePath,
					"--dir", installDir,
				}
			},
			verifies: []verify{
				{name: "wsl_conf_exists", script: "test -s /etc/wsl.conf && echo ok", want: "ok"},
				{name: "env_var_expanded", script: "cat /etc/wsl.conf | tr -d ' '", wantSub: "default=alice"},
				{name: "no_unexpanded_token", script: "grep -c WSL_CONF_E2E_USER /etc/wsl.conf || true", want: "0"},
				{name: "boot_section", script: "cat /etc/wsl.conf", wantSub: "[boot]"},
				{name: "systemd_false", script: "cat /etc/wsl.conf | tr -d ' '", wantSub: "systemd=false"},
				{name: "interop_section", script: "cat /etc/wsl.conf", wantSub: "[interop]"},
				{name: "append_windows_path", script: "cat /etc/wsl.conf | tr -d ' '", wantSub: "appendWindowsPath=false"},
			},
		},
		{
			name:   "profile_wsl_conf_yaml_mapping_content",
			distro: "e2e-wslconf-yaml",
			setup: func(t *testing.T, workDir string) []string {
				// Same effective config as the INI-string case above, but
				// `content` is a YAML mapping of sections instead of a
				// raw INI string. Validates that the mapping form renders
				// to the same INI and that env-var expansion still runs
				// over the rendered text.
				t.Setenv("WSL_CONF_E2E_USER", "alice")
				profile := `name: e2e-wslconf-yaml
image: alpine:latest
wsl_conf:
  mode: merge
  content:
    boot:
      systemd: false
    user:
      default: "%WSL_CONF_E2E_USER%"
    interop:
      appendWindowsPath: false
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)
				installDir := filepath.Join(workDir, "wsl-e2e-wslconf-yaml")
				return []string{
					"--profile", profilePath,
					"--dir", installDir,
				}
			},
			verifies: []verify{
				{name: "wsl_conf_exists", script: "test -s /etc/wsl.conf && echo ok", want: "ok"},
				{name: "env_var_expanded", script: "cat /etc/wsl.conf", wantSub: "default = alice"},
				{name: "no_unexpanded_token", script: "grep -c WSL_CONF_E2E_USER /etc/wsl.conf || true", want: "0"},
				{name: "boot_section", script: "cat /etc/wsl.conf", wantSub: "[boot]"},
				{name: "systemd_false", script: "cat /etc/wsl.conf", wantSub: "systemd = false"},
				{name: "interop_section", script: "cat /etc/wsl.conf", wantSub: "[interop]"},
				{name: "append_windows_path", script: "cat /etc/wsl.conf", wantSub: "appendWindowsPath = false"},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Best-effort cleanup of leftovers from prior aborted runs
			// before the test attempts to import the same distro name.
			// Done inside t.Run so a `-run` filter that excludes this
			// case also skips its cleanup.
			_, _ = runOutput("wsl.exe", "--unregister", tc.distro)
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
