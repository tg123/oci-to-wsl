package wsl

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ApplyDeletes rewrites the rootfs tar at tarPath, omitting any entry whose
// normalized name equals one of paths or is contained inside a path that
// names a directory (i.e. has that path as a "/"-separated prefix). Each
// entry in paths must be an absolute POSIX path inside the distribution
// (e.g. "/var/cache/apt", "/etc/motd").
//
// The function is a no-op when paths is empty. Missing entries are not an
// error: deleting a path that doesn't exist in the rootfs just leaves the
// tar unchanged for that entry.
func ApplyDeletes(tarPath string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		norm, err := toTarPath(p)
		if err != nil {
			return fmt.Errorf("invalid delete path %q: %w", p, err)
		}
		prefixes = append(prefixes, norm)
	}

	in, err := os.Open(tarPath) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open rootfs tar %q: %w", tarPath, err)
	}
	// Close in via defer on any error path; on the success path we close it
	// explicitly before os.Rename (needed on Windows) and set the local to
	// nil so this defer becomes a no-op rather than a double Close.
	defer func() {
		if in != nil {
			_ = in.Close()
		}
	}()

	outPath := tarPath + ".filtered"
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create %q: %w", outPath, err)
	}
	cleanup := func() { _ = os.Remove(outPath) }

	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
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
		if matchesAnyDeletePrefix(name, prefixes) {
			// Skip body for this entry.
			if hdr.Size > 0 {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					_ = tw.Close()
					_ = out.Close()
					cleanup()
					return fmt.Errorf("discarding deleted entry %q: %w", hdr.Name, err)
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

// normalizeTarName turns a tar header name into the same forward-slash,
// no-leading-slash, no-"./" form that toTarPath produces, so prefix
// comparisons line up regardless of how the source tar wrote the entry.
func normalizeTarName(n string) string {
	s := filepath.ToSlash(n)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	s = path.Clean(s)
	if s == "." {
		return ""
	}
	return s
}

// matchesAnyDeletePrefix reports whether entry should be removed because it
// equals one of prefixes or sits underneath one of them.
func matchesAnyDeletePrefix(entry string, prefixes []string) bool {
	for _, p := range prefixes {
		if entry == p || strings.HasPrefix(entry, p+"/") {
			return true
		}
	}
	return false
}
