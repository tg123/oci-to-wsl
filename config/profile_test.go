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

	// Unknown $VAR and ${VAR} are preserved.
	if got := config.ExpandHostPath("/a/$OCI_TO_WSL_DEFINITELY_UNSET/b"); got != "/a/${OCI_TO_WSL_DEFINITELY_UNSET}/b" {
		t.Errorf("unknown $VAR not preserved: got %q", got)
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
	if p.WslConf == nil || !strings.Contains(p.WslConf.Content, "${OCI_TO_WSL_TEST_MISSING}") {
		t.Fatalf("missing var should be preserved as ${...}, got %q", p.WslConf.Content)
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
