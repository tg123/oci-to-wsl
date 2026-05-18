package wsl_test

import (
	"archive/tar"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tg123/oci-to-wsl/wsl"
)

func TestApplyWslConf_ReplaceWithoutExisting(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("alpine\n")},
	})

	content := "[boot]\nsystemd=true\n"
	if err := wsl.ApplyWslConf(tarPath, content, wsl.WslConfModeReplace); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	e, ok := got["etc/wsl.conf"]
	if !ok {
		t.Fatalf("etc/wsl.conf missing, got %v", keysOf(got))
	}
	if string(e.body) != content {
		t.Fatalf("body = %q, want %q", e.body, content)
	}
	if e.hdr.Mode != 0o644 {
		t.Fatalf("mode = %o, want 0644", e.hdr.Mode)
	}
	if _, ok := got["etc/hostname"]; !ok {
		t.Fatal("etc/hostname must be preserved")
	}
}

func TestApplyWslConf_ReplaceOverridesExisting(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	old := "[boot]\nsystemd=false\n[user]\ndefault=root\n"
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/wsl.conf", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte(old)},
	})

	content := "[boot]\nsystemd=true\n"
	if err := wsl.ApplyWslConf(tarPath, content, wsl.WslConfModeReplace); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	e, ok := got["etc/wsl.conf"]
	if !ok {
		t.Fatal("etc/wsl.conf missing")
	}
	if string(e.body) != content {
		t.Fatalf("replace must drop existing keys: got %q, want %q", e.body, content)
	}
	// Ensure there is no second wsl.conf entry by scanning duplicates.
	if strings.Contains(string(e.body), "default") {
		t.Fatal("replace mode leaked old [user] section")
	}
}

func TestApplyWslConf_MergeOverridesAndAdds(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	old := "[boot]\nsystemd=false\ncommand=echo hi\n[user]\ndefault=root\n"
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/wsl.conf", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte(old)},
	})

	overlay := "[boot]\nsystemd=true\n[interop]\nenabled=false\n"
	if err := wsl.ApplyWslConf(tarPath, overlay, wsl.WslConfModeMerge); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	e, ok := got["etc/wsl.conf"]
	if !ok {
		t.Fatal("etc/wsl.conf missing")
	}
	merged := string(e.body)
	// Overridden key.
	if !strings.Contains(merged, "systemd = true") {
		t.Fatalf("expected systemd overridden to true, got:\n%s", merged)
	}
	if strings.Contains(merged, "systemd = false") {
		t.Fatalf("old systemd value leaked, got:\n%s", merged)
	}
	// Preserved key from base.
	if !strings.Contains(merged, "command = echo hi") {
		t.Fatalf("expected base 'command' preserved, got:\n%s", merged)
	}
	// Preserved section from base.
	if !strings.Contains(merged, "[user]") || !strings.Contains(merged, "default = root") {
		t.Fatalf("expected [user] section preserved, got:\n%s", merged)
	}
	// New section from overlay.
	if !strings.Contains(merged, "[interop]") || !strings.Contains(merged, "enabled = false") {
		t.Fatalf("expected [interop] added, got:\n%s", merged)
	}
}

func TestApplyWslConf_MergeFallsBackWhenBaseUnparseable(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	// Section header missing closing bracket — gopkg.in/ini.v1 rejects this.
	bad := "[boot\nsystemd=false\n"
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/wsl.conf", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte(bad)},
	})

	overlay := "[boot]\nsystemd=true\n"
	if err := wsl.ApplyWslConf(tarPath, overlay, wsl.WslConfModeMerge); err != nil {
		t.Fatalf("expected fallback to succeed when base is unparseable, got: %v", err)
	}

	got := readTar(t, tarPath)
	e, ok := got["etc/wsl.conf"]
	if !ok {
		t.Fatal("etc/wsl.conf missing")
	}
	body := string(e.body)
	if !strings.Contains(body, "systemd = true") {
		t.Fatalf("expected overlay to be written verbatim-style after fallback, got:\n%s", body)
	}
	if strings.Contains(body, "systemd=false") || strings.Contains(body, "systemd = false") {
		t.Fatalf("unparseable base content should be discarded on fallback, got:\n%s", body)
	}
}

func TestApplyWslConf_MergeRejectsUnparseableUserContent(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/wsl.conf", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("[boot]\nsystemd=false\n")},
	})

	// Unclosed section header in user-supplied content.
	overlay := "[boot\nsystemd=true\n"
	err := wsl.ApplyWslConf(tarPath, overlay, wsl.WslConfModeMerge)
	if err == nil {
		t.Fatal("expected error for unparseable user-supplied wsl_conf content")
	}
	if !strings.Contains(err.Error(), "user-supplied") {
		t.Fatalf("error should attribute the failure to user-supplied content, got: %v", err)
	}
}

func TestApplyWslConf_MergeWithoutExistingActsLikeReplace(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
	})

	content := "[boot]\nsystemd=true\n"
	if err := wsl.ApplyWslConf(tarPath, content, wsl.WslConfModeMerge); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	e, ok := got["etc/wsl.conf"]
	if !ok {
		t.Fatal("etc/wsl.conf missing")
	}
	if string(e.body) != content {
		t.Fatalf("body = %q, want %q (no merge transformation expected)", e.body, content)
	}
}

func TestApplyWslConf_DefaultModeIsMerge(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Mode: 0o755, Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "etc/wsl.conf", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("[user]\ndefault=root\n")},
	})

	if err := wsl.ApplyWslConf(tarPath, "[boot]\nsystemd=true\n", ""); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	merged := string(got["etc/wsl.conf"].body)
	if !strings.Contains(merged, "default = root") || !strings.Contains(merged, "systemd = true") {
		t.Fatalf("default mode should be merge, got:\n%s", merged)
	}
}

func TestApplyWslConf_InvalidMode(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, nil)

	err := wsl.ApplyWslConf(tarPath, "[boot]\nsystemd=true\n", "append")
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if !strings.Contains(err.Error(), "invalid wsl_conf mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyWslConf_EmptyContentNoOp(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "etc/hostname", Mode: 0o644, Typeflag: tar.TypeReg}, body: []byte("alpine\n")},
	})

	if err := wsl.ApplyWslConf(tarPath, "   \n  ", wsl.WslConfModeReplace); err != nil {
		t.Fatal(err)
	}
	got := readTar(t, tarPath)
	if _, ok := got["etc/wsl.conf"]; ok {
		t.Fatal("empty content should be a no-op")
	}
	if _, ok := got["etc/hostname"]; !ok {
		t.Fatal("hostname should be preserved")
	}
}

func TestApplyWslConf_EmitsEtcDirWhenMissing(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	// No etc/ directory entry.
	writeTar(t, tarPath, []tarEntry{
		{hdr: tar.Header{Name: "bin/sh", Mode: 0o755, Typeflag: tar.TypeReg}, body: []byte("#!/bin/sh\n")},
	})

	if err := wsl.ApplyWslConf(tarPath, "[boot]\nsystemd=true\n", wsl.WslConfModeReplace); err != nil {
		t.Fatal(err)
	}

	got := readTar(t, tarPath)
	if _, ok := got["etc/"]; !ok {
		t.Fatalf("expected synthesised etc/ dir entry, got keys %v", keysOf(got))
	}
	if _, ok := got["etc/wsl.conf"]; !ok {
		t.Fatal("etc/wsl.conf missing")
	}
}
