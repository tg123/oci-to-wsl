package wsl_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tg123/oci-to-wsl/wsl"
)

// writeEmptyTar writes a syntactically-valid empty tar (two zero blocks)
// to path so InjectCopies has something to append to.
func writeEmptyTar(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readTar(t *testing.T, path string) map[string]tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	out := map[string]tarEntry{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar reader: %v", err)
		}
		var buf bytes.Buffer
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(&buf, tr); err != nil {
				t.Fatal(err)
			}
		}
		out[hdr.Name] = tarEntry{hdr: *hdr, body: buf.Bytes()}
	}
	return out
}

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func TestInjectCopies_FileWithMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Src: src, Dst: "/usr/local/bin/bootstrap.sh", Mode: "0755"},
	})
	if err != nil {
		t.Fatalf("InjectCopies: %v", err)
	}

	entries := readTar(t, tarPath)
	file, ok := entries["usr/local/bin/bootstrap.sh"]
	if !ok {
		t.Fatalf("expected file entry; got %v", keysOf(entries))
	}
	if file.hdr.Mode != 0755 {
		t.Errorf("file mode: got %o, want 0755", file.hdr.Mode)
	}
	if string(file.body) != "#!/bin/sh\necho hi\n" {
		t.Errorf("file body: got %q", string(file.body))
	}

	// Parent directories must be present so wsl --import can create the path.
	for _, want := range []string{"usr/", "usr/local/", "usr/local/bin/"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("missing parent dir entry %q; have %v", want, keysOf(entries))
		}
	}
}

func TestInjectCopies_DirectoryRecursiveMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "assets")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "inner.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Src: src, Dst: "/opt/assets", Mode: "0777"},
	})
	if err != nil {
		t.Fatalf("InjectCopies: %v", err)
	}

	entries := readTar(t, tarPath)
	for _, name := range []string{"opt/assets/", "opt/assets/nested/", "opt/assets/hello.txt", "opt/assets/nested/inner.txt"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("missing entry %q; have %v", name, keysOf(entries))
		}
	}
	if e := entries["opt/assets/hello.txt"]; e.hdr.Mode != 0777 {
		t.Errorf("hello.txt mode: got %o, want 0777", e.hdr.Mode)
	}
	if e := entries["opt/assets/nested/inner.txt"]; e.hdr.Mode != 0777 {
		t.Errorf("inner.txt mode: got %o, want 0777", e.hdr.Mode)
	}
	if e := entries["opt/assets/"]; e.hdr.Mode != 0777 {
		t.Errorf("opt/assets dir mode: got %o, want 0777", e.hdr.Mode)
	}
	if string(entries["opt/assets/hello.txt"].body) != "hello" {
		t.Errorf("hello.txt body: got %q", entries["opt/assets/hello.txt"].body)
	}
}

func TestInjectCopies_AppendsPreservingExisting(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")

	// Pre-populate a tar with an existing file so we can verify the
	// trailer-truncation + append logic keeps prior entries intact.
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	existing := []byte("original")
	if err := tw.WriteHeader(&tar.Header{Name: "etc/existing", Mode: 0644, Size: int64(len(existing))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(existing); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, "added.txt")
	if err := os.WriteFile(src, []byte("added"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Src: src, Dst: "/etc/added.txt", Mode: "0600"},
	}); err != nil {
		t.Fatalf("InjectCopies: %v", err)
	}

	entries := readTar(t, tarPath)
	if string(entries["etc/existing"].body) != "original" {
		t.Errorf("pre-existing entry corrupted: got %q", entries["etc/existing"].body)
	}
	added, ok := entries["etc/added.txt"]
	if !ok {
		t.Fatalf("appended entry missing; have %v", keysOf(entries))
	}
	if added.hdr.Mode != 0600 {
		t.Errorf("appended mode: got %o, want 0600", added.hdr.Mode)
	}
	if string(added.body) != "added" {
		t.Errorf("appended body: got %q", added.body)
	}
}

func TestInjectCopies_RejectsInvalidMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Src: src, Dst: "/etc/f", Mode: "not-octal"},
	})
	if err == nil {
		t.Fatal("expected error for invalid mode, got nil")
	}
}

func TestInjectCopies_InlineData(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	body := []byte("inline-bytes\n")
	if err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Data: body, Dst: "/etc/motd"},
		{Data: []byte{0, 1, 2, 3}, Dst: "/opt/binary.bin", Mode: "0600"},
		{Data: []byte{}, Dst: "/var/empty.dat"},
	}); err != nil {
		t.Fatalf("InjectCopies: %v", err)
	}

	entries := readTar(t, tarPath)

	motd, ok := entries["etc/motd"]
	if !ok {
		t.Fatalf("missing etc/motd; have %v", keysOf(entries))
	}
	if string(motd.body) != string(body) {
		t.Errorf("motd body: got %q, want %q", motd.body, body)
	}
	if motd.hdr.Mode != 0644 {
		t.Errorf("motd default mode: got %o, want 0644", motd.hdr.Mode)
	}

	bin, ok := entries["opt/binary.bin"]
	if !ok {
		t.Fatalf("missing opt/binary.bin")
	}
	if string(bin.body) != string([]byte{0, 1, 2, 3}) {
		t.Errorf("binary body: got %x", bin.body)
	}
	if bin.hdr.Mode != 0600 {
		t.Errorf("binary mode: got %o, want 0600", bin.hdr.Mode)
	}

	empty, ok := entries["var/empty.dat"]
	if !ok {
		t.Fatalf("missing var/empty.dat")
	}
	if len(empty.body) != 0 || empty.hdr.Size != 0 {
		t.Errorf("empty file: got body %d bytes, hdr size %d", len(empty.body), empty.hdr.Size)
	}
}

func TestInjectCopies_PreservesExistingDirOwnership(t *testing.T) {
	// Regression: when the source tar already contains an explicit
	// directory entry (e.g. /home/bob/ written by ApplyUsers with the
	// new user's uid/gid and mode 0700), InjectCopies must NOT emit a
	// fresh root:root 0755 header for the same path while writing
	// parent directories of a staged file. Re-emission would cause
	// wsl --import to apply the later (root-owned) header and leave the
	// user unable to write to their own home — the original bug.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, d := range []struct {
		name     string
		mode     int64
		uid, gid int
	}{
		{"home/", 0755, 0, 0},
		{"home/bob/", 0700, 1000, 1000},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name:     d.name,
			Mode:     d.mode,
			Uid:      d.uid,
			Gid:      d.gid,
			Typeflag: tar.TypeDir,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Data: []byte("token"), Dst: "/home/bob/.azure/config"},
	}); err != nil {
		t.Fatalf("InjectCopies: %v", err)
	}

	// Walk the tar sequentially: the existing dir entries must appear
	// exactly once; the only newly-emitted directory should be the
	// previously-absent .azure subdirectory.
	fr, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fr.Close() }()
	tr := tar.NewReader(fr)
	dirCounts := map[string]int{}
	dirHeaders := map[string]tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar reader: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			dirCounts[hdr.Name]++
			dirHeaders[hdr.Name] = *hdr
		}
	}

	for _, name := range []string{"home/", "home/bob/"} {
		if got := dirCounts[name]; got != 1 {
			t.Errorf("dir %q: got %d entries, want 1 (duplicate header would clobber ownership)", name, got)
		}
	}
	if h := dirHeaders["home/bob/"]; h.Uid != 1000 || h.Gid != 1000 || h.Mode != 0700 {
		t.Errorf("home/bob/ ownership clobbered: got uid=%d gid=%d mode=%o, want uid=1000 gid=1000 mode=0700",
			h.Uid, h.Gid, h.Mode)
	}

	// The freshly-added subdirectory is expected to be present.
	if dirCounts["home/bob/.azure/"] != 1 {
		t.Errorf("expected exactly one home/bob/.azure/ entry, got %d", dirCounts["home/bob/.azure/"])
	}
}

func TestInjectCopies_RejectsBothSrcAndData(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Src: src, Data: []byte("y"), Dst: "/etc/f"},
	})
	if err == nil {
		t.Fatal("expected error when both Src and Data are set, got nil")
	}
}

func TestInjectCopies_RejectsNeitherSrcNorData(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeEmptyTar(t, tarPath)

	err := wsl.InjectCopies(tarPath, []wsl.CopyEntry{
		{Dst: "/etc/f"},
	})
	if err == nil {
		t.Fatal("expected error when neither Src nor Data is set, got nil")
	}
}

func keysOf(m map[string]tarEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
