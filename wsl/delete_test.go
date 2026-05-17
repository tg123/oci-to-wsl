package wsl_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/tg123/oci-to-wsl/wsl"
)

// writeTar writes the given entries to a tar at path.
func writeTar(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDeletes_File(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/motd", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("welcome\n")},
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("alpine\n")},
	})

	if err := wsl.ApplyDeletes(tarPath, []string{"/etc/motd"}); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	if _, ok := got["etc/motd"]; ok {
		t.Fatal("etc/motd should have been deleted")
	}
	if _, ok := got["etc/hostname"]; !ok {
		t.Fatal("etc/hostname should still be present")
	}
}

func TestApplyDeletes_DirectoryRecursive(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "var/cache/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "var/cache/apt/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "var/cache/apt/archives/foo.deb", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("deb")},
		{hdr: tar.Header{Name: "var/log/messages", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("log")},
	})

	if err := wsl.ApplyDeletes(tarPath, []string{"/var/cache"}); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	for _, k := range []string{"var/cache/", "var/cache/apt/", "var/cache/apt/archives/foo.deb"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%q should have been deleted", k)
		}
	}
	if _, ok := got["var/log/messages"]; !ok {
		t.Fatal("var/log/messages should still be present (not under /var/cache)")
	}
}

func TestApplyDeletes_PrefixIsNotSubstring(t *testing.T) {
	// "/var/ca" must not match "var/cache/...": prefix matching is
	// path-component aware, not raw string-prefix.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "var/cache/apt/foo", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("x")},
	})

	if err := wsl.ApplyDeletes(tarPath, []string{"/var/ca"}); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	if _, ok := got["var/cache/apt/foo"]; !ok {
		t.Fatal("var/cache/apt/foo should not have been deleted by /var/ca")
	}
}

func TestApplyDeletes_MissingPathIsNoop(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("alpine\n")},
	})

	if err := wsl.ApplyDeletes(tarPath, []string{"/does/not/exist"}); err != nil {
		t.Fatal(err)
	}
	got := readTar(t, tarPath)
	if _, ok := got["etc/hostname"]; !ok {
		t.Fatal("unrelated entry should still be present")
	}
}

func TestApplyDeletes_RejectsRelativePath(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, nil)

	if err := wsl.ApplyDeletes(tarPath, []string{"etc/motd"}); err == nil {
		t.Fatal("expected error for non-absolute delete path")
	}
}

func TestApplyDeletes_EmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("alpine\n")},
	})

	info1, err := os.Stat(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsl.ApplyDeletes(tarPath, nil); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if info1.Size() != info2.Size() {
		t.Fatalf("nil deletes should be a no-op; size %d -> %d", info1.Size(), info2.Size())
	}
}
