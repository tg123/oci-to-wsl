package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// CopyEntry describes a single file or directory to copy from the host into
// the new WSL distribution. Entries are staged by appending them to the
// rootfs tar before `wsl --import` runs, so the files are present on first
// boot and available to init_cmds.
type CopyEntry struct {
	// Src is the path on the host. May be a file, directory, or symlink.
	// Windows-native paths (e.g. C:\Users\me\file), %VAR% / $VAR / ${VAR}
	// environment variable references, and a leading ~ are expanded by
	// LoadProfile. Relative paths are resolved against the directory of
	// the profile file when the entry was loaded via LoadProfile;
	// otherwise against the current working directory.
	Src string `yaml:"src"`

	// Dst is the destination POSIX path inside the WSL distribution and
	// must be absolute (start with "/"). For a directory source, the
	// directory itself is created at Dst and its contents are placed
	// underneath. For a file source, Dst is the resulting file path.
	Dst string `yaml:"dst"`

	// Mode is an optional file mode for the destination, expressed as an
	// octal string (e.g. "0755", "755", "777"). It is baked directly into
	// the tar header so wsl.exe --import materialises Src at that mode —
	// no chmod step runs inside the distribution. For a directory source
	// the mode is applied to the directory and every regular file written
	// under Dst (i.e. effectively recursive).
	Mode string `yaml:"mode"`
}

// Profile describes a WSL instance to create from an OCI image.
type Profile struct {
	// Name is the WSL distribution name.
	Name string `yaml:"name"`

	// Image is the OCI image reference (e.g. "ubuntu:22.04" or "myacr.azurecr.io/myimage:latest").
	Image string `yaml:"image"`

	// InstallDir is the directory where the WSL vhd/ext4 disk will be stored.
	// Defaults to ".\<name>" relative to the current working directory.
	InstallDir string `yaml:"install_dir"`

	// Copies is a list of file/directory entries staged into the new WSL
	// distribution by appending them to the rootfs tar before
	// `wsl --import` runs, so they exist on first boot — and therefore
	// before InitCmds, which can rely on the copied content.
	Copies []CopyEntry `yaml:"copies"`

	// Deletes is a list of absolute POSIX paths inside the distribution
	// to remove from the rootfs tar before `wsl --import` runs. Each
	// path may name a file or a directory; directories are removed
	// recursively (every entry under that prefix is dropped). Missing
	// paths are silently ignored. Deletes are applied before Copies, so
	// a profile may delete an upstream directory and then stage its own
	// replacement at the same destination.
	Deletes []string `yaml:"deletes"`

	// InitCmds is a list of shell commands to run inside the new WSL
	// instance after it is created. Each entry may be either a plain
	// string (the command to run) or a mapping with `cmd` and optional
	// `env` keys; see InitCmd for details.
	InitCmds []InitCmd `yaml:"init_cmds"`
}

// EnvVar is a single environment variable to set when running an init
// command inside the WSL distribution. Value may reference host-side
// environment variables using Windows `%NAME%` and POSIX `$NAME` /
// `${NAME}` syntax — those references are expanded at profile-load time
// against the oci-to-wsl process's environment (i.e. the Windows side),
// so a profile can forward values like `%USERNAME%` or `$USER` from the
// host into the new distribution. Unknown variables are left untouched.
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// InitCmd describes a single command to run inside the new WSL instance
// after `wsl --import` finishes. It accepts two YAML shapes:
//
//	init_cmds:
//	  - echo hello                # plain string form
//	  - cmd: echo "$USER"         # object form
//	    run_as: alice             # optional – run as this in-distro user
//	    env:
//	      - name: USER
//	        value: $USER          # expanded from the host env at load time
//
// Env entries are exported in the in-distro shell before Cmd runs, so
// Cmd may reference them with normal shell substitution. RunAs, when
// set, switches to the named user inside the distribution using `su`
// (which must exist in the rootfs and accept the call without a password
// — the default for `wsl --import` which launches as root).
type InitCmd struct {
	Cmd   string   `yaml:"cmd"`
	RunAs string   `yaml:"run_as"`
	Env   []EnvVar `yaml:"env"`
}

// UnmarshalYAML accepts either a scalar string ("echo 1") or a mapping
// with `cmd`, optional `env`, and optional `run_as` fields and populates
// the receiver accordingly. Any other YAML kind yields a typed error so
// a malformed profile surfaces a clear message rather than a silent
// empty entry.
func (c *InitCmd) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return fmt.Errorf("init_cmds: decoding scalar entry: %w", err)
		}
		c.Cmd = s
		c.RunAs = ""
		c.Env = nil
		return nil
	case yaml.MappingNode:
		// Use a side type to avoid recursing back into this method.
		type rawInitCmd struct {
			Cmd   string   `yaml:"cmd"`
			RunAs string   `yaml:"run_as"`
			Env   []EnvVar `yaml:"env"`
		}
		var r rawInitCmd
		if err := node.Decode(&r); err != nil {
			return fmt.Errorf("init_cmds: decoding mapping entry: %w", err)
		}
		c.Cmd = r.Cmd
		c.RunAs = r.RunAs
		c.Env = r.Env
		return nil
	default:
		return fmt.Errorf("init_cmds: entry at line %d must be a string or a mapping with 'cmd' and optional 'env'/'run_as'", node.Line)
	}
}

// LoadProfile reads a YAML profile from the given file path.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile %q: %w", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing profile %q: %w", path, err)
	}

	// Resolve copy sources: expand Windows %VAR% / POSIX $VAR environment
	// references and a leading ~ for the user's home folder, then resolve
	// remaining relative paths against the profile file's directory so
	// profiles remain portable regardless of the caller's CWD.
	baseDir := filepath.Dir(path)
	for i := range p.Copies {
		src := p.Copies[i].Src
		if src == "" {
			continue
		}
		src = ExpandHostPath(src)
		switch {
		case filepath.IsAbs(src):
			// Native-absolute (e.g. drive-letter path on Windows, "/..."
			// on POSIX). Normalize separators but otherwise leave alone.
			src = filepath.Clean(src)
		case strings.HasPrefix(src, "/") || strings.HasPrefix(src, `\`):
			// POSIX-absolute path on a Windows host: treat as already
			// absolute so behavior is consistent across platforms (do
			// not prefix with the profile dir).
		default:
			src = filepath.Join(baseDir, src)
		}
		p.Copies[i].Src = src
	}

	// Expand host-side env-var references in init_cmds env values so a
	// profile can forward Windows-side environment variables (e.g.
	// `$USER`, `%USERNAME%`) into the new distribution. The Cmd string
	// itself is *not* expanded — it runs in the WSL shell, where the
	// caller likely wants substitutions to happen in-distro.
	for i := range p.InitCmds {
		for j := range p.InitCmds[i].Env {
			p.InitCmds[i].Env[j].Value = ExpandHostEnv(p.InitCmds[i].Env[j].Value)
		}
	}
	return &p, nil
}

// winEnvVarRE matches Windows-style %NAME% environment variable references.
// NAME must be at least one non-% character to avoid matching a literal "%%".
var winEnvVarRE = regexp.MustCompile(`%([^%]+)%`)

// ExpandHostEnv expands Windows %NAME% and POSIX $NAME / ${NAME}
// environment variable references against the current process
// environment. Unknown variables are preserved verbatim (so an
// unresolved reference surfaces in the value rather than silently
// becoming empty). Unlike ExpandHostPath it does not perform any
// path-specific handling such as tilde expansion.
func ExpandHostEnv(s string) string {
	if s == "" {
		return s
	}
	// Expand %NAME% (Windows). Leave unknown vars untouched.
	s = winEnvVarRE.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
	// Expand $NAME / ${NAME} (POSIX). os.Expand returns "" for missing
	// vars; preserve the original token instead.
	s = os.Expand(s, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return "${" + name + "}"
	})
	return s
}

// ExpandHostPath expands environment variable references and a leading ~ in a
// host path. It supports:
//   - Windows %NAME% references (e.g. %USERPROFILE%, %APPDATA%)
//   - POSIX $NAME / ${NAME} references
//   - A leading ~ or ~/ replaced with the current user's home directory
//
// Unknown variables are left as-is so that an unresolved %FOO% surfaces as a
// "file not found" error rather than silently expanding to an empty string.
func ExpandHostPath(p string) string {
	if p == "" {
		return p
	}

	p = ExpandHostEnv(p)

	// Expand a leading ~ or ~/ (or ~\ on Windows) to the user's home dir.
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}

	return p
}
