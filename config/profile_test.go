package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tg123/oci-to-wsl/config"
)

func TestLoadProfile_FullFields(t *testing.T) {
	yaml := `
name: test-distro
image: ubuntu:22.04
install_dir: C:\WSL\test
init_cmds:
  - apt-get update -y
  - apt-get install -y curl
`
	p := writeAndLoad(t, yaml)

	if p.Name != "test-distro" {
		t.Errorf("Name: got %q, want %q", p.Name, "test-distro")
	}
	if p.Image != "ubuntu:22.04" {
		t.Errorf("Image: got %q, want %q", p.Image, "ubuntu:22.04")
	}
	if p.InstallDir != `C:\WSL\test` {
		t.Errorf("InstallDir: got %q, want %q", p.InstallDir, `C:\WSL\test`)
	}
	if len(p.InitCmds) != 2 {
		t.Fatalf("InitCmds length: got %d, want 2", len(p.InitCmds))
	}
	if p.InitCmds[0] != "apt-get update -y" {
		t.Errorf("InitCmds[0]: got %q, want %q", p.InitCmds[0], "apt-get update -y")
	}
}

func TestLoadProfile_MinimalFields(t *testing.T) {
	yaml := "name: minimal\nimage: alpine:latest\n"
	p := writeAndLoad(t, yaml)

	if p.Name != "minimal" {
		t.Errorf("Name: got %q, want %q", p.Name, "minimal")
	}
	if p.Image != "alpine:latest" {
		t.Errorf("Image: got %q, want %q", p.Image, "alpine:latest")
	}
	if p.InstallDir != "" {
		t.Errorf("InstallDir: expected empty, got %q", p.InstallDir)
	}
	if len(p.InitCmds) != 0 {
		t.Errorf("InitCmds: expected empty slice, got %v", p.InitCmds)
	}
}

func TestLoadProfile_FileNotFound(t *testing.T) {
	_, err := config.LoadProfile(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadProfile_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(":\tinvalid: yaml: ["), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadProfile(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadProfile_FilesResolveRelativeSrc(t *testing.T) {
	dir := t.TempDir()
	yaml := `
name: copy-distro
image: alpine:latest
files:
  - src: ./scripts/bootstrap.sh
    dst: /usr/local/bin/bootstrap.sh
    mode: "0755"
  - src: /absolute/path/file
    dst: /etc/file
  - src: assets
    dst: /opt/assets
    mode: "777"
`
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(p.Files) != 3 {
		t.Fatalf("Files length: got %d, want 3", len(p.Files))
	}

	wantRel0 := filepath.Join(dir, "scripts", "bootstrap.sh")
	if p.Files[0].Src != wantRel0 {
		t.Errorf("Files[0].Src: got %q, want %q", p.Files[0].Src, wantRel0)
	}
	if p.Files[0].Dst != "/usr/local/bin/bootstrap.sh" {
		t.Errorf("Files[0].Dst: got %q", p.Files[0].Dst)
	}
	if p.Files[0].Mode != "0755" {
		t.Errorf("Files[0].Mode: got %q", p.Files[0].Mode)
	}

	// Absolute paths must be left untouched.
	if p.Files[1].Src != "/absolute/path/file" {
		t.Errorf("Files[1].Src: got %q, want unchanged absolute path", p.Files[1].Src)
	}

	wantRel2 := filepath.Join(dir, "assets")
	if p.Files[2].Src != wantRel2 {
		t.Errorf("Files[2].Src: got %q, want %q", p.Files[2].Src, wantRel2)
	}
	if p.Files[2].Mode != "777" {
		t.Errorf("Files[2].Mode: got %q", p.Files[2].Mode)
	}
}

func TestLoadProfile_FilesExpandWindowsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OCI_TO_WSL_TEST_ROOT", dir)
	t.Setenv("OCI_TO_WSL_TEST_NAME", "thing")

	yaml := `
name: env-distro
image: alpine:latest
files:
  - src: '%OCI_TO_WSL_TEST_ROOT%/sub/%OCI_TO_WSL_TEST_NAME%.txt'
    dst: /tmp/win.txt
  - src: '$OCI_TO_WSL_TEST_ROOT/posix/${OCI_TO_WSL_TEST_NAME}.txt'
    dst: /tmp/posix.txt
  - src: '%OCI_TO_WSL_TEST_UNSET_VAR%/literal'
    dst: /tmp/unset.txt
`
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}

	want0 := filepath.Join(dir, "sub", "thing.txt")
	if p.Files[0].Src != want0 {
		t.Errorf("Files[0].Src: got %q, want %q", p.Files[0].Src, want0)
	}
	want1 := filepath.Join(dir, "posix", "thing.txt")
	if p.Files[1].Src != want1 {
		t.Errorf("Files[1].Src: got %q, want %q", p.Files[1].Src, want1)
	}
	// Unknown %VAR% must be preserved (not silently expanded to empty),
	// then resolved relative to the profile dir.
	want2 := filepath.Join(dir, "%OCI_TO_WSL_TEST_UNSET_VAR%", "literal")
	if p.Files[2].Src != want2 {
		t.Errorf("Files[2].Src: got %q, want %q", p.Files[2].Src, want2)
	}
}

func TestExpandHostPath_TildeAndUnknownVars(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no user home dir available")
	}

	if got := config.ExpandHostPath("~"); got != home {
		t.Errorf("~: got %q, want %q", got, home)
	}
	want := filepath.Join(home, "sub", "file.txt")
	if got := config.ExpandHostPath("~/sub/file.txt"); got != want {
		t.Errorf("~/sub/file.txt: got %q, want %q", got, want)
	}

	// Unknown $VAR and ${VAR} are preserved in their original form.
	if got := config.ExpandHostPath("/a/$OCI_TO_WSL_DEFINITELY_UNSET/b"); got != "/a/$OCI_TO_WSL_DEFINITELY_UNSET/b" {
		t.Errorf("unknown $VAR not preserved: got %q", got)
	}
	if got := config.ExpandHostPath("/a/${OCI_TO_WSL_DEFINITELY_UNSET}/b"); got != "/a/${OCI_TO_WSL_DEFINITELY_UNSET}/b" {
		t.Errorf("unknown ${VAR} not preserved: got %q", got)
	}
}

func TestLoadProfile_ExpandsUserEnvVars(t *testing.T) {
	t.Setenv("OCI_TO_WSL_TEST_USER", "alice")
	t.Setenv("OCI_TO_WSL_TEST_SHELL", "/bin/zsh")
	yaml := `
name: test
image: ubuntu:22.04
users:
  - name: "%OCI_TO_WSL_TEST_USER%"
    home: "/home/$OCI_TO_WSL_TEST_USER"
    shell: "${OCI_TO_WSL_TEST_SHELL}"
    gecos: "Hello %OCI_TO_WSL_TEST_USER%"
    groups: ["%OCI_TO_WSL_TEST_USER%-admins"]
    password_plain: "pw-%OCI_TO_WSL_TEST_USER%"
    password_hash: "$6$abc$def"
`
	p := writeAndLoad(t, yaml)
	if len(p.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(p.Users))
	}
	u := p.Users[0]
	if u.Name != "alice" {
		t.Errorf("Name: got %q, want alice", u.Name)
	}
	if u.Home != "/home/alice" {
		t.Errorf("Home: got %q, want /home/alice", u.Home)
	}
	if u.Shell != "/bin/zsh" {
		t.Errorf("Shell: got %q, want /bin/zsh", u.Shell)
	}
	if u.Gecos != "Hello alice" {
		t.Errorf("Gecos: got %q", u.Gecos)
	}
	if len(u.Groups) != 1 || u.Groups[0] != "alice-admins" {
		t.Errorf("Groups: got %v", u.Groups)
	}
	if u.PasswordPlain != "pw-alice" {
		t.Errorf("PasswordPlain: got %q", u.PasswordPlain)
	}
	// PasswordHash must be left untouched even though it contains '$' sigils.
	if u.PasswordHash != "$6$abc$def" {
		t.Errorf("PasswordHash should not be expanded: got %q", u.PasswordHash)
	}
}

// Profiles often want a single template that targets per-host paths under
// /home/$USERNAME via files: and deletes:. Both fields should expand
// %NAME% / $NAME / ${NAME} the same way users.* does so the YAML stays
// portable across operators whose Windows username differs from one
// another.
func TestLoadProfile_ExpandsFilesDstAndDeletesEnvVars(t *testing.T) {
	t.Setenv("OCI_TO_WSL_TEST_USER", "alice")
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	yaml := `
name: test
image: ubuntu:22.04
files:
  - src: ` + srcDir + `
    dst: "/home/%OCI_TO_WSL_TEST_USER%/.azure"
  - src: ` + srcDir + `
    dst: "/home/${OCI_TO_WSL_TEST_USER}/code"
  - content: "hello"
    dst: "/home/$OCI_TO_WSL_TEST_USER/notes"
deletes:
  - "/home/$OCI_TO_WSL_TEST_USER/.cache"
  - "/etc/skel/%OCI_TO_WSL_TEST_USER%.conf"
`
	p := writeAndLoad(t, yaml)

	if len(p.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(p.Files))
	}
	if p.Files[0].Dst != "/home/alice/.azure" {
		t.Errorf("Files[0].Dst: got %q, want /home/alice/.azure", p.Files[0].Dst)
	}
	if p.Files[1].Dst != "/home/alice/code" {
		t.Errorf("Files[1].Dst: got %q, want /home/alice/code", p.Files[1].Dst)
	}
	if p.Files[2].Dst != "/home/alice/notes" {
		t.Errorf("Files[2].Dst (content entry): got %q, want /home/alice/notes", p.Files[2].Dst)
	}

	if len(p.Deletes) != 2 {
		t.Fatalf("expected 2 deletes, got %d", len(p.Deletes))
	}
	if p.Deletes[0] != "/home/alice/.cache" {
		t.Errorf("Deletes[0]: got %q, want /home/alice/.cache", p.Deletes[0])
	}
	if p.Deletes[1] != "/etc/skel/alice.conf" {
		t.Errorf("Deletes[1]: got %q, want /etc/skel/alice.conf", p.Deletes[1])
	}
}

// Profiles often want to template the top-level distro name, image
// reference, and install_dir per-host (e.g. a single profile that
// produces a "%USERNAME%-ubuntu" distribution under
// "%USERPROFILE%\WSL\..." with an image pulled from "%ACR_REGISTRY%").
// All three fields should expand %NAME% / $NAME / ${NAME} the same way
// users.* / files.dst / deletes already do. InstallDir is a host path,
// so it should additionally expand a leading ~ via ExpandHostPath.
func TestLoadProfile_ExpandsTopLevelEnvVars(t *testing.T) {
	t.Setenv("OCI_TO_WSL_TEST_USER", "alice")
	t.Setenv("OCI_TO_WSL_TEST_REGISTRY", "myacr.example.com")
	yaml := `
name: '%OCI_TO_WSL_TEST_USER%-ubuntu'
image: '${OCI_TO_WSL_TEST_REGISTRY}/ubuntu:22.04'
install_dir: '/srv/wsl/$OCI_TO_WSL_TEST_USER/ubuntu'
`
	p := writeAndLoad(t, yaml)

	if p.Name != "alice-ubuntu" {
		t.Errorf("Name: got %q, want alice-ubuntu", p.Name)
	}
	if p.Image != "myacr.example.com/ubuntu:22.04" {
		t.Errorf("Image: got %q, want myacr.example.com/ubuntu:22.04", p.Image)
	}
	if p.InstallDir != filepath.FromSlash("/srv/wsl/alice/ubuntu") &&
		p.InstallDir != "/srv/wsl/alice/ubuntu" {
		t.Errorf("InstallDir: got %q, want /srv/wsl/alice/ubuntu", p.InstallDir)
	}
}

// Unknown variables in top-level fields must round-trip verbatim (same
// guarantee ExpandEnvVars makes everywhere else), so an existing profile
// whose name/image/install_dir happened to contain a literal "$" or "%"
// that doesn't match a host env var keeps working unchanged.
func TestLoadProfile_TopLevelPreservesUnknownVars(t *testing.T) {
	_ = os.Unsetenv("OCI_TO_WSL_TEST_DEFINITELY_UNSET")
	yaml := `
name: '$OCI_TO_WSL_TEST_DEFINITELY_UNSET-distro'
image: 'ubuntu:22.04'
install_dir: '/srv/%OCI_TO_WSL_TEST_DEFINITELY_UNSET%/wsl'
`
	p := writeAndLoad(t, yaml)
	if p.Name != "$OCI_TO_WSL_TEST_DEFINITELY_UNSET-distro" {
		t.Errorf("Name: got %q, want literal preserved", p.Name)
	}
	if !strings.Contains(p.InstallDir, "%OCI_TO_WSL_TEST_DEFINITELY_UNSET%") {
		t.Errorf("InstallDir: got %q, want %% token preserved", p.InstallDir)
	}
}

func TestLoadProfile_FilesReplaceDefaultAndExplicit(t *testing.T) {
	yaml := `
name: replace-distro
image: alpine:latest
files:
  - src: /a
    dst: /etc/a
  - src: /b
    dst: /etc/b
    replace: true
  - src: /c
    dst: /etc/c
    replace: false
`
	p := writeAndLoad(t, yaml)
	if len(p.Files) != 3 {
		t.Fatalf("Files length: got %d, want 3", len(p.Files))
	}
	if p.Files[0].Replace != nil {
		t.Errorf("Files[0].Replace (omitted): got non-nil pointer, want nil")
	}
	if !p.Files[0].ReplaceEnabled() {
		t.Errorf("Files[0].ReplaceEnabled (omitted): got false, want true (default)")
	}
	if !p.Files[1].ReplaceEnabled() {
		t.Errorf("Files[1].ReplaceEnabled (explicit true): got false, want true")
	}
	if p.Files[2].ReplaceEnabled() {
		t.Errorf("Files[2].ReplaceEnabled (explicit false): got true, want false")
	}
}

func TestLoadProfile_FilesContentAndContentBase64(t *testing.T) {
	yaml := `
name: content-distro
image: alpine:latest
files:
  - dst: /etc/motd
    content: "hello-inline\n"
  - dst: /opt/bin
    content_base64: aGVsbG8K
    mode: "0600"
`
	p := writeAndLoad(t, yaml)
	if len(p.Files) != 2 {
		t.Fatalf("Files length: got %d, want 2", len(p.Files))
	}
	if p.Files[0].Content == nil || *p.Files[0].Content != "hello-inline\n" {
		t.Errorf("Files[0].Content: got %v", p.Files[0].Content)
	}
	if p.Files[0].Src != "" {
		t.Errorf("Files[0].Src: expected empty, got %q", p.Files[0].Src)
	}
	if p.Files[1].ContentBase64 == nil || *p.Files[1].ContentBase64 != "aGVsbG8K" {
		t.Errorf("Files[1].ContentBase64: got %v", p.Files[1].ContentBase64)
	}
	if p.Files[1].Mode != "0600" {
		t.Errorf("Files[1].Mode: got %q", p.Files[1].Mode)
	}
}

func TestProfile_Validate(t *testing.T) {
	empty := ""
	s := func(v string) *string { return &v }
	cases := []struct {
		name    string
		profile config.Profile
		wantErr string // substring; empty means expect nil
	}{
		{
			name:    "missing image",
			profile: config.Profile{Name: "n"},
			wantErr: "'image' is required",
		},
		{
			name: "src only ok",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Src: "/host/x", Dst: "/x"},
			}},
		},
		{
			name: "content only ok (empty allowed)",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Content: &empty, Dst: "/x"},
			}},
		},
		{
			name: "src and content both set",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Src: "/host/x", Content: s("hi"), Dst: "/x"},
			}},
			wantErr: "mutually exclusive",
		},
		{
			name: "src and content_base64 both set",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Src: "/host/x", ContentBase64: s("aGk="), Dst: "/x"},
			}},
			wantErr: "mutually exclusive",
		},
		{
			name: "no source",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Dst: "/x"},
			}},
			wantErr: "exactly one of",
		},
		{
			name: "missing dst",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{Src: "/host/x"},
			}},
			wantErr: "'dst' is required",
		},
		{
			name: "invalid base64",
			profile: config.Profile{Image: "alpine", Files: []config.FileEntry{
				{ContentBase64: s("not!!base64"), Dst: "/x"},
			}},
			wantErr: "decoding content_base64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// writeAndLoad writes yaml content to a temp file and calls LoadProfile.
func writeAndLoad(t *testing.T, yamlContent string) *config.Profile {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: unexpected error: %v", err)
	}
	return p
}

func TestLoadProfile_WslConfExpandsEnvVars(t *testing.T) {
	t.Setenv("OCI_TO_WSL_TEST_USER", "alice")
	yaml := "name: n\nimage: i\nwsl_conf:\n  mode: replace\n  content: |\n    [user]\n    default=$OCI_TO_WSL_TEST_USER\n    home=%OCI_TO_WSL_TEST_USER%\n"
	p := writeAndLoad(t, yaml)
	if p.WslConf == nil {
		t.Fatal("WslConf nil")
	}
	want := "[user]\ndefault=alice\nhome=alice\n"
	if p.WslConf.Content != want {
		t.Fatalf("Content = %q, want %q", p.WslConf.Content, want)
	}
}

func TestLoadProfile_WslConfPreservesUnknownVars(t *testing.T) {
	if err := os.Unsetenv("OCI_TO_WSL_TEST_MISSING"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	yaml := "name: n\nimage: i\nwsl_conf:\n  content: |\n    [user]\n    default=$OCI_TO_WSL_TEST_MISSING\n"
	p := writeAndLoad(t, yaml)
	if p.WslConf == nil || !strings.Contains(p.WslConf.Content, "$OCI_TO_WSL_TEST_MISSING") {
		t.Fatalf("missing var should be preserved verbatim, got %q", p.WslConf.Content)
	}
}

func TestLoadProfile_WslConfContentAsYAMLMapping(t *testing.T) {
	yaml := "name: n\nimage: i\nwsl_conf:\n  mode: replace\n  content:\n    boot:\n      systemd: true\n      command: echo hi\n    user:\n      default: alice\n"
	p := writeAndLoad(t, yaml)
	if p.WslConf == nil {
		t.Fatal("WslConf nil")
	}
	if p.WslConf.Mode != "replace" {
		t.Fatalf("Mode = %q, want replace", p.WslConf.Mode)
	}
	want := "[boot]\nsystemd = true\ncommand = echo hi\n\n[user]\ndefault = alice\n"
	if p.WslConf.Content != want {
		t.Fatalf("Content = %q, want %q", p.WslConf.Content, want)
	}
}

func TestLoadProfile_WslConfContentMappingExpandsEnvVars(t *testing.T) {
	t.Setenv("OCI_TO_WSL_TEST_USER", "alice")
	yaml := "name: n\nimage: i\nwsl_conf:\n  content:\n    user:\n      default: $OCI_TO_WSL_TEST_USER\n"
	p := writeAndLoad(t, yaml)
	if p.WslConf == nil || !strings.Contains(p.WslConf.Content, "default = alice") {
		t.Fatalf("env var not expanded in mapping form, got %q", p.WslConf.Content)
	}
}

func TestLoadProfile_WslConfRejectsUnknownKey(t *testing.T) {
	t.Helper()
	yaml := "name: n\nimage: i\nwsl_conf:\n  boot:\n    systemd: true\n"
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadProfile(path); err == nil {
		t.Fatal("expected error for unknown key under wsl_conf")
	} else if !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("error = %v, want 'unknown key' message", err)
	}
}
