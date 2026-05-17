package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DockerConfig is a minimal representation of the subset of
// ~/.docker/config.json that this tool reads and writes.
//
// Only the classic basic-auth form (an `auth` field containing the
// base64-encoded "username:password" string) is handled. This matches what
// `docker login` writes when no credential helper / store is configured and
// is what github.com/google/go-containerregistry's DefaultKeychain consumes
// when pulling images, so writing it here lets the rest of the pull flow
// authenticate without the docker CLI being installed.
//
// Unknown fields are preserved on a best-effort basis by round-tripping the
// raw JSON object through extras.
type DockerConfig struct {
	Auths  map[string]DockerAuth      `json:"auths,omitempty"`
	extras map[string]json.RawMessage `json:"-"`
}

// DockerAuth is a single entry under "auths" in docker's config.json.
type DockerAuth struct {
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Email    string `json:"email,omitempty"`
}

// DefaultDockerHubServer is the canonical server key docker uses for Docker
// Hub credentials. Matches the value `docker login` writes when called with
// no explicit server argument.
const DefaultDockerHubServer = "https://index.docker.io/v1/"

// DefaultDockerConfigPath returns the path to the docker config.json that
// docker (and go-containerregistry's DefaultKeychain) read from. The
// DOCKER_CONFIG environment variable takes precedence, matching docker's
// behaviour; otherwise the path is ~/.docker/config.json.
func DefaultDockerConfigPath() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating user home directory: %w", err)
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// LoadDockerConfig reads and parses the docker config.json at path. A missing
// file is not an error: an empty config is returned so callers can treat it
// as a starting point for writes.
func LoadDockerConfig(path string) (*DockerConfig, error) {
	cfg := &DockerConfig{
		Auths:  map[string]DockerAuth{},
		extras: map[string]json.RawMessage{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading docker config %q: %w", path, err)
	}
	if len(data) == 0 {
		return cfg, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing docker config %q: %w", path, err)
	}
	for k, v := range raw {
		if k == "auths" {
			if err := json.Unmarshal(v, &cfg.Auths); err != nil {
				return nil, fmt.Errorf("parsing 'auths' in docker config %q: %w", path, err)
			}
			continue
		}
		cfg.extras[k] = v
	}
	if cfg.Auths == nil {
		cfg.Auths = map[string]DockerAuth{}
	}
	return cfg, nil
}

// Save writes the config to path, preserving any unknown top-level keys that
// were present when the file was originally loaded. The parent directory is
// created with 0700 permissions if needed, and the file itself is written
// with 0600 permissions because it contains credentials.
func (c *DockerConfig) Save(path string) error {
	out := make(map[string]json.RawMessage, len(c.extras)+1)
	for k, v := range c.extras {
		out[k] = v
	}
	if len(c.Auths) > 0 {
		authsJSON, err := json.Marshal(c.Auths)
		if err != nil {
			return fmt.Errorf("encoding auths: %w", err)
		}
		out["auths"] = authsJSON
	} else {
		// Always emit an empty auths object so the file is recognisable as
		// a docker config and round-trips cleanly.
		out["auths"] = json.RawMessage(`{}`)
	}

	data, err := json.MarshalIndent(out, "", "\t")
	if err != nil {
		return fmt.Errorf("encoding docker config: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating docker config directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing docker config %q: %w", path, err)
	}
	return nil
}

// SetAuth records a basic-auth credential for the given server, replacing
// any existing entry. The username and password are joined and base64-encoded
// in the same format `docker login` produces, so the file is consumable by
// any tool that understands docker's classic credential format
// (including go-containerregistry's DefaultKeychain, which is what the
// `oci-to-wsl` pull flow already uses).
func (c *DockerConfig) SetAuth(server, username, password string) {
	if c.Auths == nil {
		c.Auths = map[string]DockerAuth{}
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	c.Auths[NormalizeDockerServer(server)] = DockerAuth{Auth: encoded}
}

// GetAuth returns the username and password recorded for server, decoding the
// base64 `auth` field when present and falling back to plain
// username/password fields otherwise. The boolean return is false when no
// entry exists for the server.
func (c *DockerConfig) GetAuth(server string) (username, password string, ok bool) {
	if c == nil || len(c.Auths) == 0 {
		return "", "", false
	}
	key := NormalizeDockerServer(server)
	entry, found := c.Auths[key]
	if !found {
		// docker historically stored docker hub credentials under several
		// equivalent keys; try a couple of common variants before giving up.
		for _, alt := range dockerHubAliases(server) {
			if entry, found = c.Auths[alt]; found {
				break
			}
		}
		if !found {
			return "", "", false
		}
	}

	if entry.Auth != "" {
		u, p, err := decodeBasicAuth(entry.Auth)
		if err == nil {
			return u, p, true
		}
		// Fall through to username/password fields if the auth blob is
		// malformed.
	}
	if entry.Username != "" || entry.Password != "" {
		return entry.Username, entry.Password, true
	}
	return "", "", false
}

// decodeBasicAuth decodes a base64-encoded "username:password" string as
// produced by `docker login`.
func decodeBasicAuth(encoded string) (string, string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// docker sometimes writes auth blobs without padding; try the
		// raw-encoding variant as a fallback.
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return "", "", fmt.Errorf("decoding base64 auth: %w", err)
		}
	}
	idx := strings.IndexByte(string(raw), ':')
	if idx < 0 {
		return "", "", fmt.Errorf("auth blob is not in username:password form")
	}
	return string(raw[:idx]), string(raw[idx+1:]), nil
}

// NormalizeDockerServer canonicalises a server string the way `docker login`
// does, so credentials written by this tool live under the same key the
// docker CLI (and go-containerregistry) would look them up by.
//
// In particular:
//   - An empty server, or any docker-hub variant, becomes the canonical
//     "https://index.docker.io/v1/" key.
//   - Otherwise, any leading scheme is stripped and any trailing path /
//     slashes are removed, leaving the bare hostname[:port].
func NormalizeDockerServer(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return DefaultDockerHubServer
	}
	lower := strings.ToLower(server)
	if lower == "docker.io" ||
		lower == "index.docker.io" ||
		lower == "registry-1.docker.io" ||
		lower == "https://index.docker.io/v1/" ||
		lower == "https://index.docker.io/v2/" ||
		lower == "https://registry-1.docker.io" {
		return DefaultDockerHubServer
	}
	// Strip scheme.
	if i := strings.Index(server, "://"); i >= 0 {
		server = server[i+3:]
	}
	// Strip trailing path / slashes.
	if i := strings.IndexByte(server, '/'); i >= 0 {
		server = server[:i]
	}
	return server
}

// dockerHubAliases returns alternative keys that may have been used to store
// docker hub credentials by other tooling.
func dockerHubAliases(server string) []string {
	if NormalizeDockerServer(server) != DefaultDockerHubServer {
		return nil
	}
	return []string{
		"docker.io",
		"index.docker.io",
		"registry-1.docker.io",
		"https://index.docker.io/v2/",
		"https://registry-1.docker.io",
	}
}

// DockerLoginOptions controls the dockerlogin subcommand.
type DockerLoginOptions struct {
	// ConfigPath is the path to the docker config.json to update. When
	// empty, DefaultDockerConfigPath is used.
	ConfigPath string
	// Server is the registry server. Empty defaults to Docker Hub.
	Server string
	// Username for the registry.
	Username string
	// Password for the registry.
	Password string
}

// DockerLogin records the given basic-auth credentials in the docker
// config.json at opts.ConfigPath, the same way `docker login` would, but
// without requiring the docker CLI binary to be installed.
//
// It returns the path of the file that was written.
func DockerLogin(opts DockerLoginOptions) (string, error) {
	if opts.Username == "" {
		return "", fmt.Errorf("username is required")
	}
	if opts.Password == "" {
		return "", fmt.Errorf("password is required")
	}

	path := opts.ConfigPath
	if path == "" {
		var err error
		path, err = DefaultDockerConfigPath()
		if err != nil {
			return "", err
		}
	}

	cfg, err := LoadDockerConfig(path)
	if err != nil {
		return "", err
	}
	cfg.SetAuth(opts.Server, opts.Username, opts.Password)
	if err := cfg.Save(path); err != nil {
		return "", err
	}
	return path, nil
}

