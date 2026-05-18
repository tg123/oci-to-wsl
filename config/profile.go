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

	// InitCmds is a list of shell commands to run inside the new WSL instance after it is created.
	InitCmds []string `yaml:"init_cmds"`

	// WslConf, when set, writes /etc/wsl.conf into the rootfs tar before
	// `wsl --import` runs. It is syntactic sugar over `copies`/`deletes`
	// that additionally understands the wsl.conf INI format: with Mode
	// "merge" (the default) it merges the user-supplied content with any
	// existing /etc/wsl.conf in the image section- and key-wise; with Mode
	// "replace" it discards any existing /etc/wsl.conf and writes Content
	// verbatim.
	WslConf *WslConfEntry `yaml:"wsl_conf"`
}

// WslConfEntry describes how to materialise /etc/wsl.conf in the rootfs tar.
//
// Two YAML shapes are accepted; both populate Content (the YAML-native form
// is rendered to INI text at load time and appended after any explicit
// Content, so it overrides on merge):
//
//  1. Raw INI string under `content:` —
//
//     wsl_conf:
//     mode: merge
//     content: |
//     [boot]
//     systemd=true
//
//  2. YAML-native sections as nested maps (any top-level key other than
//     `mode`/`content` is treated as a wsl.conf section name) —
//
//     wsl_conf:
//     mode: merge
//     boot:
//     systemd: true
//     user:
//     default: alice
//
// wsl.conf is a flat INI (sections automount/network/interop/user/boot/time/
// gpu/experimental, each a bag of scalar key=value pairs per
// https://learn.microsoft.com/windows/wsl/wsl-config), so the YAML-native
// mapping is unambiguous.
type WslConfEntry struct {
	// Mode is either "merge" (default) or "replace". Merge combines
	// Content with any existing /etc/wsl.conf in the image, with user
	// keys overriding existing keys; replace overwrites it outright.
	Mode string `yaml:"mode"`

	// Content is the wsl.conf body in standard INI format.
	Content string `yaml:"content"`
}

// UnmarshalYAML accepts both the raw `content:` form and the YAML-native
// section-map form (see WslConfEntry docs).
func (w *WslConfEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("wsl_conf: expected a mapping, got %s", yamlKindName(node.Kind))
	}
	var sectionsINI strings.Builder
	for i := 0; i < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return fmt.Errorf("wsl_conf: non-scalar key at line %d", k.Line)
		}
		switch k.Value {
		case "mode":
			if err := v.Decode(&w.Mode); err != nil {
				return fmt.Errorf("wsl_conf.mode: %w", err)
			}
		case "content":
			if err := v.Decode(&w.Content); err != nil {
				return fmt.Errorf("wsl_conf.content: %w", err)
			}
		default:
			if v.Kind != yaml.MappingNode {
				return fmt.Errorf("wsl_conf.%s: expected a mapping of key/value pairs, got %s", k.Value, yamlKindName(v.Kind))
			}
			if sectionsINI.Len() > 0 {
				sectionsINI.WriteString("\n")
			}
			sectionsINI.WriteString("[")
			sectionsINI.WriteString(k.Value)
			sectionsINI.WriteString("]\n")
			for j := 0; j < len(v.Content); j += 2 {
				kk, vv := v.Content[j], v.Content[j+1]
				if kk.Kind != yaml.ScalarNode || vv.Kind != yaml.ScalarNode {
					return fmt.Errorf("wsl_conf.%s: expected scalar key/value at line %d", k.Value, kk.Line)
				}
				sectionsINI.WriteString(kk.Value)
				sectionsINI.WriteString(" = ")
				sectionsINI.WriteString(vv.Value)
				sectionsINI.WriteString("\n")
			}
		}
	}
	if sectionsINI.Len() > 0 {
		if w.Content != "" && !strings.HasSuffix(w.Content, "\n") {
			w.Content += "\n"
		}
		w.Content += sectionsINI.String()
	}
	return nil
}

func yamlKindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
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

	// Expand environment variables in wsl_conf content so users can write
	// e.g. `default=$USER` or `default=%USERNAME%` and have it resolved at
	// profile-load time on the host.
	if p.WslConf != nil && p.WslConf.Content != "" {
		p.WslConf.Content = ExpandEnvVars(p.WslConf.Content)
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

	p = ExpandEnvVars(p)

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

// ExpandEnvVars expands environment variable references in s without doing
// any path-specific processing. It supports:
//   - Windows %NAME% references (e.g. %USERPROFILE%, %APPDATA%)
//   - POSIX $NAME / ${NAME} references
//
// Unknown variables are left as-is (Windows tokens stay %FOO%; POSIX tokens
// are rewritten to ${FOO}) so a missing variable surfaces as an obvious
// unexpanded token rather than silently expanding to an empty string.
func ExpandEnvVars(s string) string {
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

	// Expand $NAME / ${NAME} (POSIX). os.Expand returns "" for missing vars;
	// preserve the original token instead.
	s = os.Expand(s, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return "${" + name + "}"
	})

	return s
}
