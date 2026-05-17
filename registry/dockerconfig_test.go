package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/types"
)

func TestDefaultDockerConfigPathRespectsEnv(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", filepath.Join("custom", "dir"))
	got, err := DefaultDockerConfigPath()
	if err != nil {
		t.Fatalf("DefaultDockerConfigPath: %v", err)
	}
	want := filepath.Join("custom", "dir", "config.json")
	if got != want {
		t.Errorf("DefaultDockerConfigPath = %q, want %q", got, want)
	}
}

func TestDefaultDockerConfigPathFallsBackToHome(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "")
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	got, err := DefaultDockerConfigPath()
	if err != nil {
		t.Fatalf("DefaultDockerConfigPath: %v", err)
	}
	want := filepath.Join(home, ".docker", "config.json")
	if got != want {
		t.Errorf("DefaultDockerConfigPath = %q, want %q", got, want)
	}
}

func TestNormalizeLoginServer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", DefaultDockerHubServer},
		{"   ", DefaultDockerHubServer},
		{"docker.io", DefaultDockerHubServer},
		{"ghcr.io", "ghcr.io"},
		{"myacr.azurecr.io", "myacr.azurecr.io"},
		{"quay.io:5000", "quay.io:5000"},
	}
	for _, tc := range cases {
		if got := normalizeLoginServer(tc.in); got != tc.want {
			t.Errorf("normalizeLoginServer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDockerLoginValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := DockerLogin(DockerLoginOptions{ConfigPath: path, Username: "", Password: "x"}); err == nil {
		t.Errorf("expected error when username is empty")
	}
	if _, err := DockerLogin(DockerLoginOptions{ConfigPath: path, Username: "x", Password: ""}); err == nil {
		t.Errorf("expected error when password is empty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config file should not exist after validation error, stat err = %v", err)
	}
}

// TestDockerLoginMatchesDockerCLIOutput is the golden test the PR review
// asked for: it writes a credential through our DockerLogin and through
// docker's own configfile package (the same code path `docker login` uses
// internally), then asserts the two on-disk files are byte-for-byte
// identical. Any future divergence — e.g. if docker changes its format or we
// accidentally stop delegating to the SDK — will fail this test.
func TestDockerLoginMatchesDockerCLIOutput(t *testing.T) {
	cases := []struct {
		name, server, username, password string
	}{
		{"docker hub (empty server)", "", "alice", "secret"},
		{"docker hub (docker.io alias)", "docker.io", "alice", "secret"},
		{"ghcr.io with token", "ghcr.io", "alice", "ghp_abcdef0123456789"},
		{"acr", "myacr.azurecr.io", "00000000-0000-0000-0000-000000000000", "long-acr-token-value"},
		{"password with colon", "ghcr.io", "alice", "pa:ss:word"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ourDir := t.TempDir()
			ourPath := filepath.Join(ourDir, "config.json")
			if _, err := DockerLogin(DockerLoginOptions{
				ConfigPath: ourPath,
				Server:     tc.server,
				Username:   tc.username,
				Password:   tc.password,
			}); err != nil {
				t.Fatalf("DockerLogin: %v", err)
			}

			// Produce the reference output using docker/cli directly,
			// exactly as `docker login` would.
			refDir := t.TempDir()
			refCF, err := config.Load(refDir)
			if err != nil {
				t.Fatalf("docker config.Load: %v", err)
			}
			server := tc.server
			if server == "" || server == "docker.io" {
				server = DefaultDockerHubServer
			}
			refCF.AuthConfigs[server] = types.AuthConfig{
				Username:      tc.username,
				Password:      tc.password,
				ServerAddress: server,
			}
			if err := refCF.Save(); err != nil {
				t.Fatalf("docker configFile.Save: %v", err)
			}

			ourBytes, err := os.ReadFile(ourPath)
			if err != nil {
				t.Fatalf("read our config: %v", err)
			}
			refBytes, err := os.ReadFile(filepath.Join(refDir, "config.json"))
			if err != nil {
				t.Fatalf("read reference config: %v", err)
			}
			if string(ourBytes) != string(refBytes) {
				t.Errorf("on-disk format differs from docker/cli output\n--- ours ---\n%s\n--- docker/cli ---\n%s",
					ourBytes, refBytes)
			}

			// And independently confirm the credential round-trips through
			// docker's loader (i.e. a real `docker` install would see what
			// we wrote).
			loaded, err := config.Load(ourDir)
			if err != nil {
				t.Fatalf("docker config.Load on our output: %v", err)
			}
			entry, ok := loaded.AuthConfigs[server]
			if !ok {
				t.Fatalf("docker loader did not find entry for %q in our output: %v", server, loaded.AuthConfigs)
			}
			if entry.Username != tc.username || entry.Password != tc.password {
				t.Errorf("docker loader decoded (%q,%q), want (%q,%q)",
					entry.Username, entry.Password, tc.username, tc.password)
			}
		})
	}
}

// TestDockerLoginEncodesBasicAuth pins the exact wire format we produce —
// base64("username:password") under the canonical key — so a casual reader
// can see what's on disk without running the docker CLI.
func TestDockerLoginEncodesBasicAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := DockerLogin(DockerLoginOptions{
		ConfigPath: path,
		Server:     "ghcr.io",
		Username:   "alice",
		Password:   "secret",
	}); err != nil {
		t.Fatalf("DockerLogin: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse config: %v\n%s", err, raw)
	}
	entry, ok := parsed.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("missing ghcr.io entry: %s", raw)
	}
	want := base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if entry.Auth != want {
		t.Errorf("entry.Auth = %q, want %q", entry.Auth, want)
	}
}

// TestDockerLoginPreservesExtras confirms unknown top-level fields written by
// the docker CLI (e.g. credsStore, psFormat, currentContext) survive an
// update by our subcommand. This is delegated to docker's own configfile
// package, but is worth a regression test in case we ever stop using it.
func TestDockerLoginPreservesExtras(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Seed the file the way docker would.
	cf, err := config.Load(dir)
	if err != nil {
		t.Fatalf("seed config.Load: %v", err)
	}
	cf.CredentialsStore = "desktop"
	cf.CurrentContext = "default"
	cf.AuthConfigs["existing.example.com"] = types.AuthConfig{
		Username:      "old-user",
		Password:      "old-pw",
		ServerAddress: "existing.example.com",
	}
	if err := cf.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	// Now log in to a different registry via our code.
	if _, err := DockerLogin(DockerLoginOptions{
		ConfigPath: path,
		Server:     "ghcr.io",
		Username:   "alice",
		Password:   "secret",
	}); err != nil {
		t.Fatalf("DockerLogin: %v", err)
	}

	reloaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.CredentialsStore != "desktop" {
		t.Errorf("credsStore not preserved: got %q", reloaded.CredentialsStore)
	}
	if reloaded.CurrentContext != "default" {
		t.Errorf("currentContext not preserved: got %q", reloaded.CurrentContext)
	}
	if _, ok := reloaded.AuthConfigs["existing.example.com"]; !ok {
		t.Errorf("existing auth entry was dropped: %v", reloaded.AuthConfigs)
	}
	if _, ok := reloaded.AuthConfigs["ghcr.io"]; !ok {
		t.Errorf("new auth entry was not written: %v", reloaded.AuthConfigs)
	}
}

// TestDockerLoginOverwrite confirms a second login for the same server
// replaces (rather than duplicates) the credential.
func TestDockerLoginOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, pw := range []string{"first", "second"} {
		if _, err := DockerLogin(DockerLoginOptions{
			ConfigPath: path, Server: "ghcr.io", Username: "alice", Password: pw,
		}); err != nil {
			t.Fatalf("DockerLogin(%q): %v", pw, err)
		}
	}
	cf, err := config.Load(filepath.Dir(path))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if n := len(cf.AuthConfigs); n != 1 {
		t.Errorf("expected 1 auth entry after overwrite, got %d (%v)", n, cf.AuthConfigs)
	}
	if got := cf.AuthConfigs["ghcr.io"].Password; got != "second" {
		t.Errorf("password not overwritten: got %q, want %q", got, "second")
	}
}

// TestDockerLoginToWriterMatchesDockerLogin ensures the writer-based output
// produces the same bytes as the file-based DockerLogin, so callers writing
// to stdout or a custom path get an identical config.json.
func TestDockerLoginToWriterMatchesDockerLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	opts := DockerLoginOptions{
		ConfigPath: path,
		Server:     "ghcr.io",
		Username:   "alice",
		Password:   "secret",
	}
	if _, err := DockerLogin(opts); err != nil {
		t.Fatalf("DockerLogin: %v", err)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var buf bytes.Buffer
	if err := DockerLoginToWriter(opts, &buf); err != nil {
		t.Fatalf("DockerLoginToWriter: %v", err)
	}
	if !bytes.Equal(fileBytes, buf.Bytes()) {
		t.Errorf("writer output differs from file output:\nfile:\n%s\nwriter:\n%s", fileBytes, buf.Bytes())
	}
}

// TestDockerLoginToWriterPreservesExistingEntries asserts that writing to a
// writer still reads from ConfigPath so unrelated auth entries survive.
func TestDockerLoginToWriterPreservesExistingEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := DockerLogin(DockerLoginOptions{
		ConfigPath: path, Server: "quay.io", Username: "bob", Password: "pw",
	}); err != nil {
		t.Fatalf("seed login: %v", err)
	}

	var buf bytes.Buffer
	if err := DockerLoginToWriter(DockerLoginOptions{
		ConfigPath: path, Server: "ghcr.io", Username: "alice", Password: "secret",
	}, &buf); err != nil {
		t.Fatalf("DockerLoginToWriter: %v", err)
	}

	var out struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if _, ok := out.Auths["quay.io"]; !ok {
		t.Errorf("existing quay.io entry was not preserved: %v", out.Auths)
	}
	if _, ok := out.Auths["ghcr.io"]; !ok {
		t.Errorf("new ghcr.io entry missing: %v", out.Auths)
	}
}

func TestDockerLoginToWriterValidation(t *testing.T) {
	var buf bytes.Buffer
	if err := DockerLoginToWriter(DockerLoginOptions{Username: "", Password: "x"}, &buf); err == nil {
		t.Errorf("expected error when username is empty")
	}
	if err := DockerLoginToWriter(DockerLoginOptions{Username: "x", Password: ""}, &buf); err == nil {
		t.Errorf("expected error when password is empty")
	}
}
