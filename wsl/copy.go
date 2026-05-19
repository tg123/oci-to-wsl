package wsl

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CopyEntry describes a single file or directory on the host to be staged
// inside the imported WSL distribution.
//
// Exactly one of Src or Data must be set. Src reads from the host
// filesystem (and may be a file, directory, or symlink). Data carries an
// inline file body, in which case a single regular file is materialised
// at Dst with that content — no host filesystem access is performed.
type CopyEntry struct {
	// Src is a host path to a file, directory, or symlink. It may be
	// absolute or relative; the caller is responsible for resolving any
	// relative paths (e.g. against the profile file directory) before
	// passing the entry to InjectCopies. Environment variables and a
	// leading ~ are NOT expanded here. Leave empty when supplying
	// inline bytes via Data.
	Src string

	// Data, when non-nil, is an inline file body written verbatim to Dst
	// as a single regular file. An empty but non-nil slice produces a
	// zero-byte file. Mutually exclusive with Src.
	Data []byte

	// Dst is the POSIX path inside the distribution where Src will be
	// placed. It must be absolute (start with "/"). For a directory Src,
	// the directory itself is created at Dst and its contents are written
	// underneath; for a file Src or inline Data, Dst is the resulting
	// file path.
	Dst string

	// Mode, when non-empty, is an octal string (e.g. "0755", "777") baked
	// into the tar header for the destination so that wsl.exe --import
	// materialises Src at that mode without any chmod step inside the
	// distribution. For a directory source it is applied to the directory
	// and every regular file written under Dst (i.e. effectively recursive).
	// For inline Data, defaults to 0644 when unset.
	Mode string
}

// InjectCopies appends entries to an existing rootfs tar archive so that
// when WSL imports the tar the files are already present in the
// distribution. This avoids any dependency on a tar binary inside the
// container.
//
// The function expects tarPath to point at a well-formed tar archive
// produced by Go's archive/tar (ending in two 512-byte zero blocks). The
// trailing zero blocks are truncated, the new entries appended, and a
// fresh trailer is written by Close.
func InjectCopies(tarPath string, entries []CopyEntry) error {
	if len(entries) == 0 {
		return nil
	}

	f, err := os.OpenFile(tarPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open rootfs tar %q: %w", tarPath, err)
	}
	defer func() { _ = f.Close() }()

	// Strip the trailing two 512-byte zero blocks written by tar.Writer.Close
	// so the new entries are appended in-stream rather than after EOF.
	if err := truncateTarTrailer(f); err != nil {
		return fmt.Errorf("truncate tar trailer in %q: %w", tarPath, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek end of %q: %w", tarPath, err)
	}

	// Deduplicate parent directory entries we add across calls to avoid
	// emitting the same intermediate directory twice (some extractors warn).
	addedDirs := make(map[string]struct{})

	tw := tar.NewWriter(f)
	for _, e := range entries {
		if e.Dst == "" {
			_ = tw.Close()
			return fmt.Errorf("inject copies: 'dst' is required")
		}
		if (e.Src == "") == (e.Data == nil) {
			_ = tw.Close()
			return fmt.Errorf("inject copies: exactly one of 'src' or inline data is required for %q", e.Dst)
		}
		if err := injectOne(tw, e, addedDirs); err != nil {
			_ = tw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finalize rootfs tar %q: %w", tarPath, err)
	}
	return nil
}

// injectOne appends a single CopyEntry (a file, directory, or symlink) to
// the tar writer, along with any missing parent directory entries.
func injectOne(tw *tar.Writer, e CopyEntry, addedDirs map[string]struct{}) error {
	var modeOverride int64
	var hasMode bool
	if e.Mode != "" {
		v, perr := parseOctalMode(e.Mode)
		if perr != nil {
			return fmt.Errorf("invalid mode %q for %q: %w", e.Mode, displaySource(e), perr)
		}
		modeOverride = int64(v)
		hasMode = true
	}

	dst, err := toTarPath(e.Dst)
	if err != nil {
		return fmt.Errorf("invalid dst %q: %w", e.Dst, err)
	}

	if err := writeParentDirs(tw, dst, addedDirs); err != nil {
		return err
	}

	// Inline data path: write a single regular file at dst with the
	// provided bytes. Defaults to 0644 when no mode was supplied.
	if e.Data != nil {
		mode := int64(0o644)
		if hasMode {
			mode = modeOverride
		}
		return writeInlineFile(tw, dst, e.Data, mode)
	}

	// Use Lstat so a top-level symlink is preserved as a symlink rather
	// than silently dereferenced into its target's contents.
	info, err := os.Lstat(e.Src)
	if err != nil {
		return fmt.Errorf("stat %q: %w", e.Src, err)
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, lerr := os.Readlink(e.Src)
		if lerr != nil {
			return fmt.Errorf("readlink %q: %w", e.Src, lerr)
		}
		mode := int64(info.Mode().Perm())
		if hasMode {
			mode = modeOverride
		}
		return tw.WriteHeader(&tar.Header{
			Name:     dst,
			Mode:     mode,
			Linkname: filepath.ToSlash(target),
			Typeflag: tar.TypeSymlink,
			ModTime:  info.ModTime(),
			Format:   tar.FormatPAX,
		})
	case info.IsDir():
		return writeDirTree(tw, e.Src, dst, hasMode, modeOverride, addedDirs)
	default:
		return writeRegularFile(tw, e.Src, dst, info, hasMode, modeOverride)
	}
}

// writeInlineFile writes a single regular-file entry at tarName whose body
// is the supplied byte slice. The tar header's ModTime is set to the Unix
// epoch so that identical inline content produces an identical tar entry on
// every run (inline data has no natural source timestamp).
func writeInlineFile(tw *tar.Writer, tarName string, data []byte, mode int64) error {
	hdr := &tar.Header{
		Name:    tarName,
		Mode:    mode,
		Size:    int64(len(data)),
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	return nil
}

// displaySource returns a short identifier for an entry used in error
// messages — either the host source path or "<inline data>" if the entry
// supplies inline bytes via Data.
func displaySource(e CopyEntry) string {
	if e.Src != "" {
		return e.Src
	}
	return "<inline data>"
}

// writeParentDirs emits tar entries for every missing parent directory of
// dst (relative tar name, no leading slash). Directories are created with
// 0755 because we don't have a concrete source mode for them.
func writeParentDirs(tw *tar.Writer, dst string, addedDirs map[string]struct{}) error {
	parts := strings.Split(path.Dir(dst), "/")
	var cur string
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		if _, ok := addedDirs[cur]; ok {
			continue
		}
		addedDirs[cur] = struct{}{}
		hdr := &tar.Header{
			Name:     cur + "/",
			Mode:     0755,
			Typeflag: tar.TypeDir,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
	}
	return nil
}

// writeRegularFile emits a single regular-file entry at tarName.
func writeRegularFile(tw *tar.Writer, src, tarName string, info os.FileInfo, hasMode bool, modeOverride int64) error {
	mode := int64(info.Mode().Perm())
	if hasMode {
		mode = modeOverride
	}
	hdr := &tar.Header{
		Name:    tarName,
		Mode:    mode,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return nil
}

// writeDirTree walks src and emits the directory plus all children under
// dstBase (relative tar path).
func writeDirTree(tw *tar.Writer, src, dstBase string, hasMode bool, modeOverride int64, addedDirs map[string]struct{}) error {
	src = filepath.Clean(src)

	// Emit the destination directory itself first.
	rootInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	dirMode := int64(rootInfo.Mode().Perm())
	if hasMode {
		dirMode = modeOverride
	}
	if _, ok := addedDirs[dstBase]; !ok {
		addedDirs[dstBase] = struct{}{}
		if err := tw.WriteHeader(&tar.Header{
			Name:     dstBase + "/",
			Mode:     dirMode,
			Typeflag: tar.TypeDir,
			ModTime:  rootInfo.ModTime(),
			Format:   tar.FormatPAX,
		}); err != nil {
			return err
		}
	}

	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		tarName := dstBase + "/" + filepath.ToSlash(rel)

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			return tw.WriteHeader(&tar.Header{
				Name:     tarName,
				Mode:     int64(info.Mode().Perm()),
				Linkname: filepath.ToSlash(target),
				Typeflag: tar.TypeSymlink,
				ModTime:  info.ModTime(),
				Format:   tar.FormatPAX,
			})
		case info.IsDir():
			subMode := int64(info.Mode().Perm())
			if hasMode {
				subMode = modeOverride
			}
			if _, ok := addedDirs[tarName]; ok {
				return nil
			}
			addedDirs[tarName] = struct{}{}
			return tw.WriteHeader(&tar.Header{
				Name:     tarName + "/",
				Mode:     subMode,
				Typeflag: tar.TypeDir,
				ModTime:  info.ModTime(),
				Format:   tar.FormatPAX,
			})
		case info.Mode().IsRegular():
			return writeRegularFile(tw, p, tarName, info, hasMode, modeOverride)
		default:
			// Skip sockets, devices and other unsupported entry types.
			return nil
		}
	})
}

// truncateTarTrailer removes the two trailing 512-byte zero blocks written
// by tar.Writer.Close so that subsequent entries can be appended.
func truncateTarTrailer(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	const trailer = 1024
	if fi.Size() < trailer {
		// Empty or malformed tar - nothing to trim.
		return nil
	}
	buf := make([]byte, trailer)
	if _, err := f.ReadAt(buf, fi.Size()-trailer); err != nil {
		return err
	}
	for _, b := range buf {
		if b != 0 {
			// Not a standard trailer; leave the file alone rather than
			// corrupt a tar archive we don't recognise.
			return nil
		}
	}
	return f.Truncate(fi.Size() - trailer)
}

// parseOctalMode parses a mode string in octal (with optional "0" or "0o"
// prefix) and returns its numeric value.
func parseOctalMode(mode string) (uint32, error) {
	s := strings.TrimSpace(mode)
	if strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O") {
		s = s[2:]
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// toTarPath converts an absolute POSIX destination path to a clean
// tar-relative name (no leading slash, forward slashes, no "." or "..").
// It rejects empty or relative destinations so that profile entries which
// would otherwise place files at unexpected locations surface as errors.
func toTarPath(dst string) (string, error) {
	if dst == "" {
		return "", fmt.Errorf("destination path is empty")
	}
	d := filepath.ToSlash(dst)
	if !strings.HasPrefix(d, "/") {
		return "", fmt.Errorf("destination path must be absolute (start with '/')")
	}
	d = path.Clean(d)
	d = strings.TrimPrefix(d, "/")
	if d == "" || d == "." || strings.HasPrefix(d, "..") {
		return "", fmt.Errorf("destination path resolves outside the rootfs")
	}
	return d, nil
}
