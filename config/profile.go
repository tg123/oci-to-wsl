package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileEntry describes a single file or directory to stage from the host into
// the new WSL distribution. Entries are staged by appending them to the
// rootfs tar before `wsl --import` runs, so the files are present on first
// boot and available to init_cmds.
//
// Exactly one source of file data must be set: Src (read from the host
// filesystem), Content (inline UTF-8 string body) or ContentBase64 (inline
// base64-encoded body, for binary or otherwise awkward content). Content
// and ContentBase64 only produce a single regular file at Dst — they
// cannot describe a directory tree.
type FileEntry struct {
	// Src is the path on the host. May be a file, directory, or symlink.
	// Windows-native paths (e.g. C:\Users\me\file), %VAR% / $VAR / ${VAR}
	// environment variable references, and a leading ~ are expanded by
	// LoadProfile. Relative paths are resolved against the directory of
	// the profile file when the entry was loaded via LoadProfile;
	// otherwise against the current working directory.
	Src string `yaml:"src,omitempty"`

	// Content is an inline UTF-8 file body. When set, no host file is
	// read: the bytes are written verbatim to Dst as a single regular
	// file. Mutually exclusive with Src and ContentBase64. A pointer is
	// used so an absent value is distinguishable from an explicit empty
	// string, allowing `content: ""` to stage a zero-byte file.
	Content *string `yaml:"content,omitempty"`

	// ContentBase64 is an inline file body encoded with standard base64.
	// Use this for binary or otherwise awkward content (it round-trips
	// through YAML cleanly). Mutually exclusive with Src and Content. A
	// pointer is used so an absent value is distinguishable from an
	// explicit empty string, allowing `content_base64: ""` to stage a
	// zero-byte file.
	ContentBase64 *string `yaml:"content_base64,omitempty"`

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

	// Replace, when true (the default when omitted), causes any existing
	// entry at Dst in the upstream rootfs tar to be dropped before this
	// copy is staged — equivalent to listing Dst in the top-level
	// `deletes`. For a directory Dst the entire subtree is removed
	// recursively, so the copied tree fully replaces the upstream one
	// rather than overlaying onto it. Set to false to overlay instead
	// (i.e. keep upstream files that the copy does not itself overwrite).
	// Use a pointer so an absent value is distinguishable from explicit
	// `false` and can therefore default to true.
	Replace *bool `yaml:"replace,omitempty"`
}

// ReplaceEnabled reports whether this entry's Dst should replace (i.e. be
// deleted from the upstream rootfs tar before injection). The default when
// Replace is unset is true.
func (e FileEntry) ReplaceEnabled() bool {
	if e.Replace == nil {
		return true
	}
	return *e.Replace
}

// Validate checks that this FileEntry is well-formed: Dst must be set, and
// exactly one source of file data (Src, Content, or ContentBase64) must be
// provided. When ContentBase64 is set, it must also decode as standard
// base64. Returns a descriptive error referencing the offending Dst when
// possible.
func (e FileEntry) Validate() error {
	if e.Dst == "" {
		return fmt.Errorf("'dst' is required")
	}
	sources := 0
	if e.Src != "" {
		sources++
	}
	if e.Content != nil {
		sources++
	}
	if e.ContentBase64 != nil {
		sources++
	}
	if sources == 0 {
		return fmt.Errorf("%q: exactly one of 'src', 'content', or 'content_base64' is required", e.Dst)
	}
	if sources > 1 {
		return fmt.Errorf("%q: 'src', 'content', and 'content_base64' are mutually exclusive", e.Dst)
	}
	if e.ContentBase64 != nil {
		if _, err := base64.StdEncoding.DecodeString(*e.ContentBase64); err != nil {
			return fmt.Errorf("%q: decoding content_base64: %w", e.Dst, err)
		}
	}
	return nil
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

	// Files is a list of file/directory entries staged into the new WSL
	// distribution by appending them to the rootfs tar before
	// `wsl --import` runs, so they exist on first boot — and therefore
	// before InitCmds, which can rely on the staged content.
	Files []FileEntry `yaml:"files"`

	// Deletes is a list of absolute POSIX paths inside the distribution
	// to remove from the rootfs tar before `wsl --import` runs. Each
	// path may name a file or a directory; directories are removed
	// recursively (every entry under that prefix is dropped). Missing
	// paths are silently ignored. Deletes are applied before Files, so
	// a profile may delete an upstream directory and then stage its own
	// replacement at the same destination.
	Deletes []string `yaml:"deletes"`

	// InitCmds is a list of shell commands to run inside the new WSL instance after it is created.
	InitCmds []string `yaml:"init_cmds"`
}

// Validate checks the profile for internal consistency without performing
// any I/O (it does not check for the existence of Src files or the
// suitability of InstallDir). It is intended to be called before any
// expensive work (e.g. pulling the image) so user-facing errors surface
// fast. The Image field and each Files entry are validated; Name and
// InstallDir are intentionally not checked here because their
// requirements differ between WSL-import and --save-tar modes.
func (p *Profile) Validate() error {
	if p.Image == "" {
		return fmt.Errorf("'image' is required")
	}
	for i, f := range p.Files {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
	}
	return nil
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

	// Resolve file sources: expand Windows %VAR% / POSIX $VAR environment
	// references and a leading ~ for the user's home folder, then resolve
	// remaining relative paths against the profile file's directory so
	// profiles remain portable regardless of the caller's CWD.
	baseDir := filepath.Dir(path)
	for i := range p.Files {
		src := p.Files[i].Src
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
		p.Files[i].Src = src
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
