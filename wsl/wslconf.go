package wsl

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/ini.v1"
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

// wsl.conf is an INI file (see https://learn.microsoft.com/windows/wsl/wsl-config),
// so parse/serialise via gopkg.in/ini.v1 rather than rolling our own.

// mergeWslConf parses base and overlay as wsl.conf documents and returns
// the serialised merge: overlay sections/keys override base, but base
// sections/keys that overlay does not mention are preserved in their
// original order.
//
// If the image-shipped base is unparseable, mergeWslConf logs a warning
// and falls back to overlay-only (replace-style) output so a malformed
// upstream /etc/wsl.conf does not abort the import. A parse failure on
// the user-supplied overlay is still returned as an error, since that
// indicates a problem the user can fix in their profile.
func mergeWslConf(base, overlay string) (string, error) {
	oDoc, err := ini.Load([]byte(overlay))
	if err != nil {
		return "", fmt.Errorf("parse user-supplied wsl_conf content: %w", err)
	}
	bDoc, err := ini.Load([]byte(base))
	if err != nil {
		slog.Warn("existing /etc/wsl.conf in rootfs image is not valid INI; falling back to replace mode for wsl_conf merge",
			"error", err)
		var buf bytes.Buffer
		if _, err := oDoc.WriteTo(&buf); err != nil {
			return "", fmt.Errorf("serialise wsl.conf: %w", err)
		}
		return buf.String(), nil
	}
	for _, name := range oDoc.SectionStrings() {
		oSec := oDoc.Section(name)
		bSec, err := bDoc.GetSection(name)
		if err != nil {
			bSec, err = bDoc.NewSection(name)
			if err != nil {
				return "", fmt.Errorf("create section %q: %w", name, err)
			}
		}
		for _, k := range oSec.KeyStrings() {
			bSec.Key(k).SetValue(oSec.Key(k).Value())
		}
	}
	var buf bytes.Buffer
	if _, err := bDoc.WriteTo(&buf); err != nil {
		return "", fmt.Errorf("serialise merged wsl.conf: %w", err)
	}
	return buf.String(), nil
}
