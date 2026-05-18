package registry

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
)

// DefaultDockerHubServer is the canonical server key docker uses for Docker
// Hub credentials, matching the value `docker login` writes when called with
// no explicit server argument. It is exported because main.go uses it for
// display in interactive prompts.
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

// DockerLogin records basic-auth credentials in the docker config.json at
// opts.ConfigPath, the same way `docker login` would, but without requiring
// the docker CLI binary to be installed.
//
// The on-disk format is produced by docker's own
// github.com/docker/cli/cli/config/configfile package, so the resulting file
// is byte-for-byte equivalent to what `docker login` writes for the same
// inputs and is consumed transparently by anything that reads docker
// credentials (including go-containerregistry's DefaultKeychain, which the
// rest of this tool's pull flow already uses).
//
// It returns the path of the file that was written.
func DockerLogin(opts DockerLoginOptions) (string, error) {
	cf, path, err := buildDockerLoginConfig(opts)
	if err != nil {
		return "", err
	}
	if err := cf.Save(); err != nil {
		return "", fmt.Errorf("saving docker config %q: %w", path, err)
	}
	return path, nil
}

// DockerLoginToWriter is the same as DockerLogin but writes the resulting
// config.json to w instead of any file on disk. The existing config at
// opts.ConfigPath (or the default docker config location) is still read so
// that other auth entries and unknown top-level fields are preserved in the
// output.
func DockerLoginToWriter(opts DockerLoginOptions, w io.Writer) error {
	cf, _, err := buildDockerLoginConfig(opts)
	if err != nil {
		return err
	}
	if err := cf.SaveToWriter(w); err != nil {
		return fmt.Errorf("encoding docker config: %w", err)
	}
	return nil
}

// buildDockerLoginConfig validates opts, loads the existing config at the
// resolved path (or starts an empty one), applies the credential update, and
// returns the in-memory config plus the resolved source path. Both DockerLogin
// and DockerLoginToWriter share this so they produce identical bytes.
func buildDockerLoginConfig(opts DockerLoginOptions) (*configfile.ConfigFile, string, error) {
	if opts.Username == "" {
		return nil, "", fmt.Errorf("username is required")
	}
	if opts.Password == "" {
		return nil, "", fmt.Errorf("password is required")
	}

	path := opts.ConfigPath
	if path == "" {
		var err error
		path, err = DefaultDockerConfigPath()
		if err != nil {
			return nil, "", err
		}
	}

	cf, err := loadOrNewDockerConfig(path)
	if err != nil {
		return nil, "", err
	}

	server := normalizeLoginServer(opts.Server)
	cf.AuthConfigs[server] = types.AuthConfig{
		Username:      opts.Username,
		Password:      opts.Password,
		ServerAddress: server,
	}
	return cf, path, nil
}

// loadOrNewDockerConfig reads the docker config.json at path through docker's
// own configfile package so that all unknown top-level fields are preserved
// on save. A missing file produces an empty config that is ready for writes,
// matching `docker login`'s behaviour on a first-run system.
func loadOrNewDockerConfig(path string) (*configfile.ConfigFile, error) {
	cf := configfile.New(path)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cf, nil
		}
		return nil, fmt.Errorf("reading docker config %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := cf.LoadFromReader(f); err != nil {
		return nil, fmt.Errorf("parsing docker config %q: %w", path, err)
	}
	return cf, nil
}

// normalizeLoginServer mirrors what `docker login` does when assigning a
// server address: an empty or "docker.io" value is rewritten to the canonical
// "https://index.docker.io/v1/" index server key; anything else is kept
// verbatim, exactly as the docker CLI stores it.
func normalizeLoginServer(server string) string {
	server = strings.TrimSpace(server)
	if server == "" || server == "docker.io" {
		return DefaultDockerHubServer
	}
	return server
}
