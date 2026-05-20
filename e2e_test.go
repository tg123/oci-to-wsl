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
			name:   "profile_users",
			distro: "e2e-users",
			setup: func(t *testing.T, workDir string) []string {
				profile := `name: e2e-users
image: alpine:latest
users:
  - name: alice
    uid: 1500
    gid: 1500
    shell: /bin/sh
    gecos: "Alice E2E"
    groups: [wheel, doesnotexist]
    password_hash: "!"
  - name: bob
    shell: /bin/sh
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-users")
				return []string{
					"--profile", profilePath,
					"--dir", installDir,
				}
			},
			verifies: []verify{
				{name: "alice_passwd", script: "getent passwd alice", wantSub: "alice:x:1500:1500:Alice E2E:/home/alice:/bin/sh"},
				{name: "alice_home_exists", script: "test -d /home/alice && stat -c '%u:%g:%a' /home/alice", want: "1500:1500:700"},
				{name: "alice_shadow", script: "grep '^alice:' /etc/shadow", wantSub: "alice:!:"},
				{name: "alice_in_wheel", script: "getent group wheel", wantSub: "alice"},
				{name: "doesnotexist_not_created", script: "if getent group doesnotexist >/dev/null 2>&1; then echo CREATED; else echo MISSING; fi", want: "MISSING"},
				{name: "bob_passwd", script: "getent passwd bob", wantSub: "bob:x:"},
				{name: "bob_home_owned", script: "stat -c '%U' /home/bob", want: "bob"},
				{name: "alice_can_su", script: "su -s /bin/sh alice -c 'id -un'", want: "alice"},
				// Ownership-end-to-end: the new user must actually be
				// able to write to their own home directory on first
				// boot. This catches regressions where the home dir
				// header lands in the tar with wrong uid/gid (e.g.,
				// when the upstream rootfs already ships an explicit
				// /home/<name> entry owned by root and ApplyUsers
				// skips emitting a corrective trailing header).
				{name: "alice_can_write_home", script: "su -s /bin/sh alice -c 'touch /home/alice/.ownership-probe && stat -c %U /home/alice/.ownership-probe'", want: "alice"},
				{name: "bob_can_write_home", script: "su -s /bin/sh bob -c 'touch /home/bob/.ownership-probe && stat -c %U /home/bob/.ownership-probe'", want: "bob"},
				// Isolation: alice (uid 1500, mode-0700 home) must not
				// be able to write into bob's mode-0700 home or into
				// root-owned /root. This guards against ApplyUsers
				// regressing home ownership/permissions back to
				// world-writable or to the wrong uid.
				{name: "alice_cannot_write_bob_home", script: "su -s /bin/sh alice -c 'if touch /home/bob/.intrusion-probe 2>/dev/null; then echo WROTE; else echo DENIED; fi'", want: "DENIED"},
				{name: "alice_cannot_write_root_home", script: "su -s /bin/sh alice -c 'if touch /root/.intrusion-probe 2>/dev/null; then echo WROTE; else echo DENIED; fi'", want: "DENIED"},
			},
		},
		{
			// PR#17 regression: %VAR% / $VAR / ${VAR} in `files.dst` and
			// `deletes` must be expanded against the host environment at
			// profile-load time, mirroring the expansion already applied
			// to `files.src`, `users.*`, and `wsl_conf.content`. Without
			// expansion the literal "%E2E_EXPAND_USER%" lands in the tar
			// as a directory name and `deletes` silently no-ops on a
			// path that doesn't exist, both of which we assert against.
			name:   "files_dst_and_deletes_expand_env_vars",
			distro: "e2e-expand",
			setup: func(t *testing.T, workDir string) []string {
				t.Setenv("E2E_EXPAND_USER", "alice")
				// Use an upstream-shipped file as the deletes target so
				// we can prove the path was actually expanded: if the
				// literal "/etc/%E2E_EXPAND_TARGET%" went through, the
				// file at /etc/alpine-release would still exist.
				t.Setenv("E2E_EXPAND_TARGET", "alpine-release")

				mustMkdir(t, filepath.Join(workDir, "dotconfig"))
				mustWrite(t, filepath.Join(workDir, "dotconfig", "marker"), "config-from-host")

				profile := `name: e2e-expand
image: alpine:latest
users:
  - name: '%E2E_EXPAND_USER%'
    shell: /bin/sh
files:
  # %VAR%, $VAR, ${VAR} must all expand in dst — exercise each form.
  - src: ./dotconfig
    dst: '/home/%E2E_EXPAND_USER%/.config'
  - src: ./dotconfig/marker
    dst: '/home/$E2E_EXPAND_USER/posix-bare'
  - src: ./dotconfig/marker
    dst: '/home/${E2E_EXPAND_USER}/posix-braced'
deletes:
  - '/etc/%E2E_EXPAND_TARGET%'
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-expand")
				return []string{"--profile", profilePath, "--dir", installDir}
			},
			verifies: []verify{
				// users: still creates the account (profile parsed OK
				// after the new expansion pass).
				{name: "alice_passwd", script: "getent passwd alice", wantSub: "alice:x:"},
				// files.dst with %VAR% expanded — directory and content
				// land under /home/alice, NOT under a literal
				// /home/%E2E_EXPAND_USER%.
				{name: "config_dir_present", script: "test -d /home/alice/.config && echo ok", want: "ok"},
				{name: "config_content", script: "cat /home/alice/.config/marker", want: "config-from-host"},
				// $VAR (bare) and ${VAR} (braced) forms expand the same
				// way (mirroring ExpandHostPath / users.* behaviour).
				{name: "posix_bare_content", script: "cat /home/alice/posix-bare", want: "config-from-host"},
				{name: "posix_braced_content", script: "cat /home/alice/posix-braced", want: "config-from-host"},
				// No literal "%E2E_EXPAND_USER%" directory leaked into
				// the tar. The shell glob is quoted so the literal %...%
				// has to match a real filename to expand.
				{
					name:   "no_literal_dst_token_in_home",
					script: "if ls -d '/home/%E2E_EXPAND_USER%' >/dev/null 2>&1; then echo LEAKED; else echo OK; fi",
					want:   "OK",
				},
				// deletes with %VAR% expanded — alpine's stock
				// /etc/alpine-release must be GONE. Without expansion
				// the literal "/etc/%E2E_EXPAND_TARGET%" would not
				// match, the upstream file would remain, and this
				// would fail.
				{name: "deletes_expanded_removed_upstream", script: "test ! -e /etc/alpine-release && echo ok", want: "ok"},
				{
					name:   "no_literal_delete_token_path",
					script: "if ls '/etc/%E2E_EXPAND_TARGET%' >/dev/null 2>&1; then echo LEAKED; else echo OK; fi",
					want:   "OK",
				},
				// PR#16 ownership preservation must still hold when
				// files.dst is templated: the parent /home/alice that
				// users: created has to survive files: re-emitting its
				// parent dirs, and alice must be able to write into it.
				{name: "alice_home_owned", script: "stat -c '%U:%G:%a' /home/alice", want: "alice:alice:700"},
				{
					name:   "alice_can_write_home",
					script: "su -s /bin/sh alice -c 'touch /home/alice/.ownership-probe && stat -c %U /home/alice/.ownership-probe'",
					want:   "alice",
				},
			},
		},
		{
			// Regression: when a profile both creates a user and stages
			// files under that user's home, /home/<name> must remain
			// owned by the new user. Earlier InjectCopies re-emitted the
			// parent directory header as root:root 0755, leaving the
			// user unable to write to their own home on first login.
			name:   "files_under_user_home_preserve_ownership",
			distro: "e2e-home-ownership",
			setup: func(t *testing.T, workDir string) []string {
				mustMkdir(t, filepath.Join(workDir, "dotazure"))
				mustWrite(t, filepath.Join(workDir, "dotazure", "config"), "azure-config-from-host")

				profile := `name: e2e-home-ownership
image: alpine:latest
users:
  - name: bob
    shell: /bin/sh
files:
  - src: ./dotazure
    dst: /home/bob/.azure
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)

				installDir := filepath.Join(workDir, "wsl-e2e-home-ownership")
				return []string{"--profile", profilePath, "--dir", installDir}
			},
			verifies: []verify{
				{name: "bob_home_owned_by_bob", script: "stat -c '%U' /home/bob", want: "bob"},
				{name: "bob_home_mode", script: "stat -c '%a' /home/bob", want: "700"},
				{name: "azure_dir_present", script: "test -d /home/bob/.azure && echo ok", want: "ok"},
				{name: "azure_config_content", script: "cat /home/bob/.azure/config", want: "azure-config-from-host"},
				// The exact ownership-on-first-login symptom from the bug
				// report: bob must be able to write to his own home dir.
				{name: "bob_can_write_home", script: "su -s /bin/sh bob -c 'touch /home/bob/.ownership-probe && stat -c %U /home/bob/.ownership-probe'", want: "bob"},
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
			// PR#17 follow-up: %VAR% / $VAR / ${VAR} must also be
			// expanded in the top-level `name`, `image`, and
			// `install_dir` fields, so a single profile can produce
			// per-operator distro names ("%USERNAME%-ubuntu"),
			// per-environment image refs ("$ACR_REGISTRY/img:tag"),
			// and per-user install dirs ("%USERPROFILE%\WSL\..."). The
			// chosen env values below resolve such that the imported
			// distro name equals tc.distro and the install_dir is a
			// subdirectory under workDir.
			name:   "top_level_fields_expand_env_vars",
			distro: "e2e-toplevel-alice",
			setup: func(t *testing.T, workDir string) []string {
				t.Setenv("E2E_TOPLEVEL_USER", "alice")
				t.Setenv("E2E_TOPLEVEL_IMAGE", "alpine")
				// Use the dir flag-style %VAR% in install_dir. Anchor
				// it to an absolute path under workDir so we can also
				// assert the directory actually got created on the
				// host (proving install_dir expansion really ran).
				installRoot := filepath.Join(workDir, "wsl-toplevel")
				t.Setenv("E2E_TOPLEVEL_DIR", installRoot)

				profile := `name: 'e2e-toplevel-%E2E_TOPLEVEL_USER%'
image: '${E2E_TOPLEVEL_IMAGE}:latest'
install_dir: '$E2E_TOPLEVEL_DIR/${E2E_TOPLEVEL_USER}'
`
				profilePath := filepath.Join(workDir, "profile.yaml")
				mustWrite(t, profilePath, profile)
				// No --name / --dir flags: rely purely on the profile
				// so the test exercises LoadProfile's expansion pass
				// rather than the CLI override path.
				return []string{"--profile", profilePath}
			},
			verifies: []verify{
				// The distro booted under the EXPANDED name (tc.distro
				// is "e2e-toplevel-alice"); wslExec only succeeds if
				// `wsl --import` used that name, which in turn proves
				// `name:` was expanded at profile-load time.
				{name: "boot", script: "/bin/true"},
				// `image:` was expanded to alpine:latest — the proof
				// is that we landed on an alpine rootfs.
				{name: "alpine_release_present", script: "test -s /etc/alpine-release && echo ok", want: "ok"},
				{name: "uname_linux", script: "uname -s", want: "Linux"},
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
