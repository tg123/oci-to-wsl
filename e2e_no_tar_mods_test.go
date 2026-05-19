//go:build e2e

// This file contains end-to-end coverage for the OCI_TO_WSL_NO_TAR_MODS
// opt-out in --save-tar mode. It is Linux-only (the WSL import path is
// already covered by TestE2E on Windows) and is gated behind the `e2e`
// build tag so the default `go test ./...` run is unaffected.
//
// As with the other e2e tests, the runner does NOT build the binary
// itself; the caller must build oci-to-wsl and export its absolute path
// via OCI_TO_WSL_BIN.
package main_test

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestE2ESaveTarNoTarMods verifies that profile `copies` and `deletes`
// are applied to the saved rootfs tar by default in --save-tar mode and
// can be opted out of with OCI_TO_WSL_NO_TAR_MODS=1, and that the
// stderr skip notice bypasses --loglevel filtering.
func TestE2ESaveTarNoTarMods(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("save-tar no-tar-mods e2e is linux-only (GOOS=%s)", runtime.GOOS)
	}

	bin := ociToWSLBinary(t)
	workDir := t.TempDir()

	// Lay out a profile that copies a host file into /opt/marker.txt
	// and deletes /etc/issue from the rootfs.
	assetsDir := filepath.Join(workDir, "assets")
	mustMkdir(t, assetsDir)
	mustWrite(t, filepath.Join(assetsDir, "marker.txt"), "hello-from-host")

	profile := `name: e2e-no-tar-mods
image: alpine:latest
files:
  - src: ./assets/marker.txt
    dst: /opt/marker.txt
    mode: "0644"
deletes:
  - /etc/alpine-release
`
	profilePath := filepath.Join(workDir, "profile.yaml")
	mustWrite(t, profilePath, profile)

	t.Run("default_applies_copies_and_deletes", func(t *testing.T) {
		tarPath := filepath.Join(workDir, "default.tar")
		cmd := exec.Command(bin, "--profile", profilePath, "--save-tar", tarPath)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("oci-to-wsl --save-tar (default) failed: %v\n%s", err, out)
		}

		entries := readTarEntries(t, tarPath)
		if body, ok := entries["opt/marker.txt"]; !ok {
			t.Fatalf("opt/marker.txt missing from default tar (copies were not applied); entries:\n%s", strings.Join(sortedKeys(entries), "\n"))
		} else if !strings.Contains(string(body), "hello-from-host") {
			t.Fatalf("opt/marker.txt content unexpected: %q", string(body))
		}
		if _, ok := entries["etc/alpine-release"]; ok {
			t.Fatalf("etc/alpine-release still present in default tar (deletes were not applied)")
		}
	})

	t.Run("no_tar_mods_skips_copies_and_deletes", func(t *testing.T) {
		tarPath := filepath.Join(workDir, "raw.tar")
		// Run with --loglevel error so the stderr skip notice assertion
		// actually proves the message bypasses slog level filtering
		// (slog warn/info would be suppressed at this level).
		cmd := exec.Command(bin, "--loglevel", "error", "--profile", profilePath, "--save-tar", tarPath)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "OCI_TO_WSL_NO_TAR_MODS=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oci-to-wsl --save-tar (NO_TAR_MODS=1) failed: %v\n%s", err, out)
		}
		// The skip notice must always be visible on stderr (it bypasses
		// slog filtering on purpose).
		if !strings.Contains(string(out), "OCI_TO_WSL_NO_TAR_MODS is set") {
			t.Fatalf("expected skip notice on stderr; got:\n%s", out)
		}

		entries := readTarEntries(t, tarPath)
		if _, ok := entries["opt/marker.txt"]; ok {
			t.Fatalf("opt/marker.txt unexpectedly present in raw tar (copies should be skipped)")
		}
		if _, ok := entries["etc/alpine-release"]; !ok {
			t.Fatalf("etc/alpine-release missing from raw tar (deletes should be skipped); entries:\n%s", strings.Join(sortedKeys(entries), "\n"))
		}
	})
}

// readTarEntries reads path as a tar archive and returns a map of
// normalized entry names (forward slashes, no leading "./") to their
// content. Non-regular entries are recorded with a nil body so callers
// can still assert presence/absence.
func readTarEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("tar %s is empty", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	entries := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		name := strings.TrimPrefix(strings.ReplaceAll(hdr.Name, "\\", "/"), "./")
		name = strings.TrimSuffix(name, "/")
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("reading %s body from %s: %v", name, path, err)
			}
			entries[name] = body
		} else {
			entries[name] = nil
		}
	}
	return entries
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
