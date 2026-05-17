package wsl

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Copy copies a file or directory from the host into the given WSL
// distribution. If mode is non-empty, it is parsed as an octal string (e.g.
// "0755", "777") and applied to the destination after the copy (recursively
// for directory sources).
//
// The copy is performed by streaming a tar archive over stdin to a
// "tar -xf -" invocation inside the distro, so it works on any rootfs that
// ships a tar binary (including BusyBox tar on alpine).
func Copy(distro, src, dst, mode string) error {
	wslPath, err := findWSL()
	if err != nil {
		return err
	}
	if dst == "" {
		return fmt.Errorf("copy: dst is required")
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("copy: stat %q: %w", src, err)
	}

	// Validate mode early so we don't tar+ship data only to fail at chmod.
	if mode != "" {
		if _, perr := parseMode(mode); perr != nil {
			return fmt.Errorf("copy: invalid mode %q: %w", mode, perr)
		}
	}

	// Always use forward-slash POSIX paths inside the distro.
	dstPosix := toPosix(dst)

	// Figure out where to extract the tar stream and what name(s) it will
	// contain. For a file source we extract into the parent directory and
	// rename to the destination basename; for a directory source we extract
	// directly into the destination directory.
	var extractDir string
	if info.IsDir() {
		extractDir = dstPosix
	} else {
		extractDir = path.Dir(dstPosix)
		if extractDir == "" {
			extractDir = "."
		}
	}

	// Build the remote shell command: ensure target dir exists, untar from
	// stdin, then optionally chmod.
	shellCmd := fmt.Sprintf("set -e; mkdir -p %s; tar -xf - -C %s", shellQuote(extractDir), shellQuote(extractDir))
	if mode != "" {
		modeArg := normalizeMode(mode)
		if info.IsDir() {
			shellCmd += fmt.Sprintf("; chmod -R %s %s", modeArg, shellQuote(dstPosix))
		} else {
			shellCmd += fmt.Sprintf("; chmod %s %s", modeArg, shellQuote(dstPosix))
		}
	}

	args := []string{"--distribution", distro, "--user", "root", "--", "sh", "-c", shellCmd}
	fmt.Printf("[%s] copy %s -> %s\n", distro, src, dstPosix)
	cmd := exec.Command(wslPath, args...) //nolint:gosec

	pr, pw := io.Pipe()
	cmd.Stdin = pr

	// Stream the tar in a goroutine and forward any error to the pipe so
	// the remote tar sees EOF and we capture write failures.
	tarErrCh := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		var terr error
		if info.IsDir() {
			terr = writeDirTar(tw, src)
		} else {
			terr = writeFileTar(tw, src, path.Base(dstPosix), info)
		}
		if cerr := tw.Close(); terr == nil {
			terr = cerr
		}
		_ = pw.CloseWithError(terr)
		tarErrCh <- terr
	}()

	out, runErr := cmd.CombinedOutput()
	tarErr := <-tarErrCh

	if tarErr != nil {
		return fmt.Errorf("copy: building tar for %q: %w", src, tarErr)
	}
	if runErr != nil {
		return fmt.Errorf("copy %q -> %q failed: %w\n%s", src, dstPosix, runErr, strings.TrimSpace(string(out)))
	}
	if len(out) > 0 {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return nil
}

// writeFileTar emits a single file entry under tarName.
func writeFileTar(tw *tar.Writer, src, tarName string, info os.FileInfo) error {
	hdr := &tar.Header{
		Name:    tarName,
		Mode:    int64(info.Mode().Perm()),
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
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return nil
}

// writeDirTar walks src and emits entries with names relative to src so the
// archive extracts directly into the destination directory.
func writeDirTar(tw *tar.Writer, src string) error {
	src = filepath.Clean(src)
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			// Skip the root; extraction directory already exists.
			return nil
		}
		tarName := filepath.ToSlash(rel)

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr := &tar.Header{
				Name:     tarName,
				Mode:     int64(info.Mode().Perm()),
				Linkname: filepath.ToSlash(target),
				Typeflag: tar.TypeSymlink,
				ModTime:  info.ModTime(),
				Format:   tar.FormatPAX,
			}
			return tw.WriteHeader(hdr)
		case info.IsDir():
			hdr := &tar.Header{
				Name:     tarName + "/",
				Mode:     int64(info.Mode().Perm()),
				Typeflag: tar.TypeDir,
				ModTime:  info.ModTime(),
				Format:   tar.FormatPAX,
			}
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			hdr := &tar.Header{
				Name:    tarName,
				Mode:    int64(info.Mode().Perm()),
				Size:    info.Size(),
				ModTime: info.ModTime(),
				Format:  tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(p) //nolint:gosec
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		default:
			// Skip sockets, devices and other unsupported entry types.
			return nil
		}
	})
}

// parseMode accepts an octal string (optionally with a leading "0") and
// returns the value. It is used both to validate the user input and to
// produce a normalised form for chmod.
func parseMode(mode string) (uint32, error) {
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

// normalizeMode returns the mode in a form chmod accepts (plain octal
// digits, no "0o" prefix).
func normalizeMode(mode string) string {
	v, err := parseMode(mode)
	if err != nil {
		return mode
	}
	return strconv.FormatUint(uint64(v), 8)
}

// toPosix converts a host path to forward-slash form for use inside WSL.
func toPosix(p string) string {
	return filepath.ToSlash(p)
}

// shellQuote wraps s in single quotes so it can be safely embedded in a
// /bin/sh command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
