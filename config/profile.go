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

	// decodedBase64 caches the decoded bytes of ContentBase64 the first
	// time Validate() succeeds, so the happy path does not decode the
	// payload again at injection time.
	decodedBase64 []byte `yaml:"-"`

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
// base64; the decoded bytes are cached on the entry so DecodedContent()
// does not have to decode again on the happy path.
func (e *FileEntry) Validate() error {
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
		decoded, err := base64.StdEncoding.DecodeString(*e.ContentBase64)
		if err != nil {
			return fmt.Errorf("%q: decoding content_base64: %w", e.Dst, err)
		}
		e.decodedBase64 = decoded
	}
	return nil
}

// DecodedContentBase64 returns the bytes decoded from ContentBase64. It
// must only be called after Validate() has succeeded on this entry; the
// returned slice is the cached result of the validation-time decode.
func (e FileEntry) DecodedContentBase64() []byte {
	return e.decodedBase64
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
// `content` accepts two shapes, both of which produce the same INI body:
//
//  1. Raw INI string (a YAML scalar) —
//
//     wsl_conf:
//     mode: merge
//     content: |
//     [boot]
//     systemd=true
//     [user]
//     default=alice
//
//  2. YAML mapping of sections (each top-level key is a wsl.conf section
//     name, each value is a mapping of scalar key/value pairs) —
//
//     wsl_conf:
//     mode: merge
//     content:
//     boot:
//     systemd: true
//     user:
//     default: alice
//
// wsl.conf is a flat INI (sections automount/network/interop/user/boot/time/
// gpu/experimental, each a bag of scalar key=value pairs per
// https://learn.microsoft.com/windows/wsl/wsl-config), so the YAML-mapping
// form is unambiguous.
type WslConfEntry struct {
	// Mode is either "merge" (default) or "replace". Merge combines
	// Content with any existing /etc/wsl.conf in the image, with user
	// keys overriding existing keys; replace overwrites it outright.
	Mode string `yaml:"mode"`

	// Content is the wsl.conf body in standard INI format. When the
	// profile uses the YAML-mapping form of `content`, the mapping is
	// rendered to INI text at load time and stored here.
	Content string `yaml:"content"`
}

// UnmarshalYAML accepts both the raw `content:` string form and the
// YAML-mapping form (see WslConfEntry docs).
func (w *WslConfEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("wsl_conf: expected a mapping, got %s", yamlKindName(node.Kind))
	}
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
			rendered, err := renderWslConfContent(v)
			if err != nil {
				return err
			}
			w.Content = rendered
		default:
			return fmt.Errorf("wsl_conf: unknown key %q (expected one of: mode, content)", k.Value)
		}
	}
	return nil
}

// renderWslConfContent accepts either a YAML scalar (raw INI text) or a YAML
// mapping of sections-to-keys and returns the corresponding INI body.
func renderWslConfContent(v *yaml.Node) (string, error) {
	switch v.Kind {
	case yaml.ScalarNode:
		var s string
		if err := v.Decode(&s); err != nil {
			return "", fmt.Errorf("wsl_conf.content: %w", err)
		}
		return s, nil
	case yaml.MappingNode:
		var b strings.Builder
		for i := 0; i < len(v.Content); i += 2 {
			sk, sv := v.Content[i], v.Content[i+1]
			if sk.Kind != yaml.ScalarNode {
				return "", fmt.Errorf("wsl_conf.content: non-scalar section name at line %d", sk.Line)
			}
			if sv.Kind != yaml.MappingNode {
				return "", fmt.Errorf("wsl_conf.content.%s: expected a mapping of key/value pairs, got %s", sk.Value, yamlKindName(sv.Kind))
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[")
			b.WriteString(sk.Value)
			b.WriteString("]\n")
			for j := 0; j < len(sv.Content); j += 2 {
				kk, vv := sv.Content[j], sv.Content[j+1]
				if kk.Kind != yaml.ScalarNode || vv.Kind != yaml.ScalarNode {
					return "", fmt.Errorf("wsl_conf.content.%s: expected scalar key/value at line %d", sk.Value, kk.Line)
				}
				b.WriteString(kk.Value)
				b.WriteString(" = ")
				b.WriteString(vv.Value)
				b.WriteString("\n")
			}
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("wsl_conf.content: expected a string or a mapping of sections, got %s", yamlKindName(v.Kind))
	}
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
	for i := range p.Files {
		if err := p.Files[i].Validate(); err != nil {
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

	// Expand environment variables in wsl_conf content so users can write
	// e.g. `default=$USER` or `default=%USERNAME%` and have it resolved at
	// profile-load time on the host.
	if p.WslConf != nil && p.WslConf.Content != "" {
		p.WslConf.Content = ExpandEnvVars(p.WslConf.Content)
	}

	// Expand environment variables in wsl_conf content so users can write
	// e.g. `default=$USER` or `default=%USERNAME%` and have it resolved at
	// profile-load time on the host.
	if p.WslConf != nil && p.WslConf.Content != "" {
		p.WslConf.Content = ExpandEnvVars(p.WslConf.Content)
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
