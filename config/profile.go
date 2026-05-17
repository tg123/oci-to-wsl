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
// the new WSL distribution before init_cmds run.
type CopyEntry struct {
	// Src is the path on the host. May be a file or a directory. Relative
	// paths are resolved against the directory of the profile file when the
	// entry was loaded via LoadProfile; otherwise against the current
	// working directory.
	Src string `yaml:"src"`

	// Dst is the destination path inside the WSL distribution. For a
	// directory source, the directory contents are placed at Dst. For a
	// file source, Dst is the resulting file path.
	Dst string `yaml:"dst"`

	// Mode is an optional file mode to apply to the destination after the
	// copy, expressed as an octal string (e.g. "0755", "755", "777"). For a
	// directory source, the mode is applied recursively.
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

	// Copies is a list of file/directory copies performed inside the new
	// WSL distribution after import but before InitCmds, so init commands
	// can rely on the copied content.
	Copies []CopyEntry `yaml:"copies"`

	// InitCmds is a list of shell commands to run inside the new WSL instance after it is created.
	InitCmds []string `yaml:"init_cmds"`

	// Platform selects a specific OS/arch from a multi-arch manifest list.
	// Format is "os/arch" (e.g. "linux/amd64", "linux/arm64"). When empty
	// the host's runtime arch is used (with OS forced to linux).
	Platform string `yaml:"platform"`
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
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, src)
		}
		p.Copies[i].Src = src
	}
	return &p, nil
}

// winEnvVarRE matches Windows-style %NAME% environment variable references.
// NAME must be at least one non-% character to avoid matching a literal "%%".
var winEnvVarRE = regexp.MustCompile(`%([^%]+)%`)

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

	// Expand %NAME% (Windows). Leave unknown vars untouched.
	p = winEnvVarRE.ReplaceAllStringFunc(p, func(m string) string {
		name := m[1 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})

	// Expand $NAME / ${NAME} (POSIX). os.Expand returns "" for missing vars;
	// preserve the original token instead.
	p = os.Expand(p, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return "${" + name + "}"
	})

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
