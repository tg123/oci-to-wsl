package wsl

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// WslConfMode selects how user-supplied wsl.conf content is combined with any
// /etc/wsl.conf already present in the rootfs tar.
type WslConfMode string

const (
	// WslConfModeMerge merges the user-supplied content section-by-section
	// and key-by-key with any existing /etc/wsl.conf in the rootfs tar.
	// User keys override existing keys; new sections and keys are appended
	// in declaration order. This is the default when no mode is specified.
	WslConfModeMerge WslConfMode = "merge"

	// WslConfModeReplace discards any existing /etc/wsl.conf from the
	// rootfs tar and writes the user-supplied content verbatim.
	WslConfModeReplace WslConfMode = "replace"
)

// wslConfTarPath is the location of the wsl.conf file inside the rootfs tar
// (no leading slash, the same form returned by toTarPath).
const wslConfTarPath = "etc/wsl.conf"

// ApplyWslConf rewrites the rootfs tar at tarPath so that /etc/wsl.conf
// contains content (when mode is "replace") or the result of merging content
// with the existing /etc/wsl.conf (when mode is "merge" or empty).
//
// Behaviour:
//   - Any existing /etc/wsl.conf entry is removed from the output tar.
//   - In merge mode, its body is parsed and merged with content; user keys
//     override existing keys, new sections/keys are appended in declaration
//     order. In replace mode the existing body is discarded outright.
//   - A fresh /etc/wsl.conf regular-file entry is appended at the end of
//     the tar with mode 0644.
//   - If the rootfs tar has no /etc/ directory entry, one is emitted before
//     the new wsl.conf so extractors that require parent directories first
//     do not warn. /etc almost always already exists in a Linux rootfs.
//
// A no-op when content is empty.
func ApplyWslConf(tarPath string, content string, mode WslConfMode) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	switch mode {
	case "", WslConfModeMerge, WslConfModeReplace:
	default:
		return fmt.Errorf("invalid wsl_conf mode %q (expected %q or %q)", string(mode), WslConfModeMerge, WslConfModeReplace)
	}
	if mode == "" {
		mode = WslConfModeMerge
	}

	in, err := os.Open(tarPath) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open rootfs tar %q: %w", tarPath, err)
	}
	defer func() {
		if in != nil {
			_ = in.Close()
		}
	}()

	outPath := tarPath + ".wslconf"
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create %q: %w", outPath, err)
	}
	cleanup := func() { _ = os.Remove(outPath) }

	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)

	var existingBody []byte
	sawEtcDir := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = tw.Close()
			_ = out.Close()
			cleanup()
			return fmt.Errorf("reading tar entry: %w", err)
		}
		name := normalizeTarName(hdr.Name)
		if name == "etc" && hdr.Typeflag == tar.TypeDir {
			sawEtcDir = true
		}
		if name == wslConfTarPath {
			// Capture and skip the existing wsl.conf entry.
			if mode == WslConfModeMerge && hdr.Size > 0 {
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, tr); err != nil {
					_ = tw.Close()
					_ = out.Close()
					cleanup()
					return fmt.Errorf("reading existing wsl.conf: %w", err)
				}
				existingBody = buf.Bytes()
			} else if hdr.Size > 0 {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					_ = tw.Close()
					_ = out.Close()
					cleanup()
					return fmt.Errorf("discarding existing wsl.conf: %w", err)
				}
			}
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = out.Close()
			cleanup()
			return fmt.Errorf("writing tar header %q: %w", hdr.Name, err)
		}
		if hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				_ = tw.Close()
				_ = out.Close()
				cleanup()
				return fmt.Errorf("copying tar body %q: %w", hdr.Name, err)
			}
		}
	}

	final := content
	if mode == WslConfModeMerge && len(existingBody) > 0 {
		merged, mErr := mergeWslConf(string(existingBody), content)
		if mErr != nil {
			_ = tw.Close()
			_ = out.Close()
			cleanup()
			return fmt.Errorf("merging wsl.conf: %w", mErr)
		}
		final = merged
	}

	if !sawEtcDir {
		if err := tw.WriteHeader(&tar.Header{
			Name:     "etc/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
			Format:   tar.FormatPAX,
		}); err != nil {
			_ = tw.Close()
			_ = out.Close()
			cleanup()
			return fmt.Errorf("writing etc dir entry: %w", err)
		}
	}

	body := []byte(final)
	if err := tw.WriteHeader(&tar.Header{
		Name:     wslConfTarPath,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
		Format:   tar.FormatPAX,
	}); err != nil {
		_ = tw.Close()
		_ = out.Close()
		cleanup()
		return fmt.Errorf("writing wsl.conf header: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		_ = tw.Close()
		_ = out.Close()
		cleanup()
		return fmt.Errorf("writing wsl.conf body: %w", err)
	}

	if err := tw.Close(); err != nil {
		_ = out.Close()
		cleanup()
		return fmt.Errorf("closing rewritten tar: %w", err)
	}
	if err := out.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing rewritten tar file: %w", err)
	}
	if err := in.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing source tar: %w", err)
	}
	in = nil
	if err := os.Rename(outPath, tarPath); err != nil {
		cleanup()
		return fmt.Errorf("replacing rootfs tar: %w", err)
	}
	return nil
}

// wslConfSection is an ordered collection of key/value pairs under a single
// [section] header. Keys preserve insertion order; updating an existing key
// keeps its original position rather than moving it to the end.
type wslConfSection struct {
	name  string // "" represents the pre-section (top-of-file) area
	keys  []string
	pairs map[string]string
	// raw holds verbatim non-key lines (blank lines and comments) appended
	// after the section header. Only used for the merge result so that
	// user-supplied comments are preserved in the produced file.
	leading []string
}

// wslConfDoc is an ordered list of sections, indexable by section name.
type wslConfDoc struct {
	order    []string
	sections map[string]*wslConfSection
}

func newWslConfDoc() *wslConfDoc {
	return &wslConfDoc{sections: map[string]*wslConfSection{}}
}

func (d *wslConfDoc) section(name string) *wslConfSection {
	s, ok := d.sections[name]
	if ok {
		return s
	}
	s = &wslConfSection{name: name, pairs: map[string]string{}}
	d.sections[name] = s
	d.order = append(d.order, name)
	return s
}

func (s *wslConfSection) set(key, value string) {
	if _, ok := s.pairs[key]; !ok {
		s.keys = append(s.keys, key)
	}
	s.pairs[key] = value
}

// parseWslConf parses an INI-ish wsl.conf into an ordered document.
// Comments (lines starting with '#' or ';') and blank lines are preserved
// per-section so they survive a round-trip when no merge happens; once a
// merge occurs the formatting is canonicalised.
func parseWslConf(content string) (*wslConfDoc, error) {
	doc := newWslConfDoc()
	cur := doc.section("")
	lines := strings.Split(content, "\n")
	for i, raw := range lines {
		// Strip a trailing \r so CRLF input parses identically to LF.
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			cur.leading = append(cur.leading, line)
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if name == "" {
				return nil, fmt.Errorf("line %d: empty section header", i+1)
			}
			cur = doc.section(name)
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected 'key = value', got %q", i+1, trimmed)
		}
		key := strings.TrimSpace(trimmed[:eq])
		val := strings.TrimSpace(trimmed[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}
		cur.set(key, val)
	}
	return doc, nil
}

// mergeWslConf parses base and overlay as wsl.conf documents and returns
// the serialised merge: overlay sections/keys override base, but base
// sections/keys that overlay does not mention are preserved in their
// original order.
func mergeWslConf(base, overlay string) (string, error) {
	bDoc, err := parseWslConf(base)
	if err != nil {
		return "", fmt.Errorf("parse existing wsl.conf: %w", err)
	}
	oDoc, err := parseWslConf(overlay)
	if err != nil {
		return "", fmt.Errorf("parse user wsl.conf: %w", err)
	}
	for _, name := range oDoc.order {
		oSec := oDoc.sections[name]
		bSec := bDoc.section(name)
		for _, k := range oSec.keys {
			bSec.set(k, oSec.pairs[k])
		}
	}
	return serializeWslConf(bDoc), nil
}

// serializeWslConf renders a wslConfDoc in canonical form: the unnamed
// pre-section first (if it has any pairs), then each named section in
// declaration order, with a blank line between sections.
func serializeWslConf(d *wslConfDoc) string {
	var b strings.Builder
	first := true
	for _, name := range d.order {
		s := d.sections[name]
		// Skip the unnamed pre-section if it has no key/value pairs (we do
		// not round-trip top-of-file comments because they'd accumulate
		// across merges).
		if name == "" && len(s.keys) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		if name != "" {
			b.WriteString("[")
			b.WriteString(name)
			b.WriteString("]\n")
		}
		for _, k := range s.keys {
			b.WriteString(k)
			b.WriteString(" = ")
			b.WriteString(s.pairs[k])
			b.WriteString("\n")
		}
	}
	return b.String()
}
