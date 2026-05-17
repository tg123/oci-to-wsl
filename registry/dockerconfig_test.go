package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeDockerServer(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", DefaultDockerHubServer},
		{"  ", DefaultDockerHubServer},
		{"docker.io", DefaultDockerHubServer},
		{"index.docker.io", DefaultDockerHubServer},
		{"registry-1.docker.io", DefaultDockerHubServer},
		{"https://index.docker.io/v1/", DefaultDockerHubServer},
		{"https://index.docker.io/v2/", DefaultDockerHubServer},
		{"myacr.azurecr.io", "myacr.azurecr.io"},
		{"https://myacr.azurecr.io", "myacr.azurecr.io"},
		{"https://myacr.azurecr.io/v2/", "myacr.azurecr.io"},
		{"ghcr.io/some/path", "ghcr.io"},
		{"quay.io:5000/foo", "quay.io:5000"},
	}
	for _, tc := range cases {
		got := NormalizeDockerServer(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeDockerServer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeBasicAuth(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	u, p, err := decodeBasicAuth(enc)
	if err != nil {
		t.Fatalf("decodeBasicAuth: unexpected error: %v", err)
	}
	if u != "alice" || p != "s3cret" {
		t.Errorf("decodeBasicAuth: got (%q,%q), want (alice,s3cret)", u, p)
	}

	// Passwords with colons must survive the round-trip (we split on the
	// first colon only).
	enc2 := base64.StdEncoding.EncodeToString([]byte("alice:has:colons"))
	u, p, err = decodeBasicAuth(enc2)
	if err != nil {
		t.Fatalf("decodeBasicAuth (colon pw): unexpected error: %v", err)
	}
	if u != "alice" || p != "has:colons" {
		t.Errorf("decodeBasicAuth (colon pw): got (%q,%q), want (alice,has:colons)", u, p)
	}

	if _, _, err := decodeBasicAuth("not-base64!!!"); err == nil {
		t.Errorf("decodeBasicAuth: expected error for malformed input")
	}

	// A blob that decodes but contains no colon is also invalid.
	bad := base64.StdEncoding.EncodeToString([]byte("nocolonhere"))
	if _, _, err := decodeBasicAuth(bad); err == nil {
		t.Errorf("decodeBasicAuth: expected error for missing colon")
	}
}

func TestSetAndGetAuthRoundTrip(t *testing.T) {
	cfg := &DockerConfig{Auths: map[string]DockerAuth{}}
	cfg.SetAuth("myacr.azurecr.io", "user1", "p@ss/word")

	entry, ok := cfg.Auths["myacr.azurecr.io"]
	if !ok {
		t.Fatalf("expected entry under canonical key, got auths=%v", cfg.Auths)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		t.Fatalf("entry.Auth is not valid base64: %v", err)
	}
	if string(decoded) != "user1:p@ss/word" {
		t.Errorf("decoded auth = %q, want %q", string(decoded), "user1:p@ss/word")
	}

	u, p, ok := cfg.GetAuth("myacr.azurecr.io")
	if !ok || u != "user1" || p != "p@ss/word" {
		t.Errorf("GetAuth = (%q,%q,%v), want (user1, p@ss/word, true)", u, p, ok)
	}

	// Same registry, different scheme should resolve to the same entry.
	u, p, ok = cfg.GetAuth("https://myacr.azurecr.io/")
	if !ok || u != "user1" || p != "p@ss/word" {
		t.Errorf("GetAuth(scheme variant) = (%q,%q,%v), want (user1, p@ss/word, true)", u, p, ok)
	}
}

func TestGetAuthDockerHubAliases(t *testing.T) {
	cfg := &DockerConfig{Auths: map[string]DockerAuth{
		"docker.io": {Auth: base64.StdEncoding.EncodeToString([]byte("dhub:pw"))},
	}}
	u, p, ok := cfg.GetAuth("")
	if !ok || u != "dhub" || p != "pw" {
		t.Errorf("GetAuth(\"\") = (%q,%q,%v), want (dhub, pw, true)", u, p, ok)
	}
}

func TestGetAuthUsernamePasswordFallback(t *testing.T) {
	cfg := &DockerConfig{Auths: map[string]DockerAuth{
		"ghcr.io": {Username: "u", Password: "p"},
	}}
	u, p, ok := cfg.GetAuth("ghcr.io")
	if !ok || u != "u" || p != "p" {
		t.Errorf("GetAuth = (%q,%q,%v), want (u, p, true)", u, p, ok)
	}
}

func TestGetAuthMissing(t *testing.T) {
	cfg := &DockerConfig{Auths: map[string]DockerAuth{}}
	if _, _, ok := cfg.GetAuth("ghcr.io"); ok {
		t.Errorf("GetAuth on empty config should return ok=false")
	}
	var nilCfg *DockerConfig
	if _, _, ok := nilCfg.GetAuth("ghcr.io"); ok {
		t.Errorf("GetAuth on nil config should return ok=false")
	}
}

func TestLoadDockerConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "config.json")
	cfg, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatalf("LoadDockerConfig on missing file should not error: %v", err)
	}
	if cfg == nil || cfg.Auths == nil {
		t.Fatalf("expected empty initialised config, got %+v", cfg)
	}
	if len(cfg.Auths) != 0 {
		t.Errorf("expected empty auths, got %v", cfg.Auths)
	}
}

func TestLoadDockerConfigParsesClassicAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	contents := `{
  "auths": {
    "ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("alice:secret")) + `"},
    "myacr.azurecr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("bob:pw")) + `"}
  },
  "credsStore": "desktop",
  "psFormat": "table {{.ID}}"
}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatalf("LoadDockerConfig: %v", err)
	}
	u, p, ok := cfg.GetAuth("ghcr.io")
	if !ok || u != "alice" || p != "secret" {
		t.Errorf("ghcr GetAuth = (%q,%q,%v), want (alice, secret, true)", u, p, ok)
	}
	u, p, ok = cfg.GetAuth("myacr.azurecr.io")
	if !ok || u != "bob" || p != "pw" {
		t.Errorf("acr GetAuth = (%q,%q,%v), want (bob, pw, true)", u, p, ok)
	}

	// Unknown top-level keys should be preserved on save.
	if _, ok := cfg.extras["credsStore"]; !ok {
		t.Errorf("expected credsStore to be retained in extras, got %v", cfg.extras)
	}
}

func TestSaveDockerConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "config.json")

	cfg, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatalf("LoadDockerConfig: %v", err)
	}
	cfg.SetAuth("ghcr.io", "alice", "secret")
	cfg.extras = map[string]json.RawMessage{
		"credsStore": json.RawMessage(`"desktop"`),
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File should exist and have the right permissions (skip mode check on
	// Windows where the bits are reported differently).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Errorf("config file mode = %o, want 0600", mode)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("written file is not valid JSON: %v", err)
	}
	if _, ok := parsed["auths"]; !ok {
		t.Errorf("written config missing 'auths' key: %s", raw)
	}
	if got := string(parsed["credsStore"]); got != `"desktop"` {
		t.Errorf("extras not preserved: got %s", got)
	}

	// Reload and confirm the credential is intact.
	reloaded, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatalf("LoadDockerConfig (reload): %v", err)
	}
	u, p, ok := reloaded.GetAuth("ghcr.io")
	if !ok || u != "alice" || p != "secret" {
		t.Errorf("reload GetAuth = (%q,%q,%v), want (alice, secret, true)", u, p, ok)
	}
}

func TestDockerLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	written, err := DockerLogin(DockerLoginOptions{
		ConfigPath: path,
		Server:     "ghcr.io",
		Username:   "alice",
		Password:   "secret",
	})
	if err != nil {
		t.Fatalf("DockerLogin: %v", err)
	}
	if written != path {
		t.Errorf("DockerLogin returned %q, want %q", written, path)
	}

	cfg, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatalf("LoadDockerConfig: %v", err)
	}
	u, p, ok := cfg.GetAuth("ghcr.io")
	if !ok || u != "alice" || p != "secret" {
		t.Errorf("after DockerLogin GetAuth = (%q,%q,%v), want (alice, secret, true)", u, p, ok)
	}

	// Re-login with a new password should overwrite, not duplicate.
	if _, err := DockerLogin(DockerLoginOptions{
		ConfigPath: path,
		Server:     "ghcr.io",
		Username:   "alice",
		Password:   "newpw",
	}); err != nil {
		t.Fatalf("DockerLogin (overwrite): %v", err)
	}
	cfg, _ = LoadDockerConfig(path)
	if len(cfg.Auths) != 1 {
		t.Errorf("expected single auth entry after overwrite, got %v", cfg.Auths)
	}
	_, p, _ = cfg.GetAuth("ghcr.io")
	if p != "newpw" {
		t.Errorf("password not overwritten: got %q, want newpw", p)
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
	// File should not have been created on validation failure.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config file should not exist after validation error, stat err = %v", err)
	}
}

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
