package main

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tg123/oci-to-wsl/config"
)

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func TestVerifySha1(t *testing.T) {
	data := []byte("hello world")
	good := sha1Hex(data)

	if err := verifySha1(data, "", "src"); err != nil {
		t.Errorf("empty want should pass: %v", err)
	}
	if err := verifySha1(data, good, "src"); err != nil {
		t.Errorf("matching digest should pass: %v", err)
	}
	// Case-insensitive comparison.
	if err := verifySha1(data, strings.ToUpper(good), "src"); err != nil {
		t.Errorf("uppercase digest should pass: %v", err)
	}
	if err := verifySha1(data, "da39a3ee5e6b4b0d3255bfef95601890afd80709", "src"); err == nil {
		t.Error("mismatching digest should fail")
	} else if !strings.Contains(err.Error(), "sha1 mismatch") {
		t.Errorf("error = %v, want sha1 mismatch", err)
	}
}

func TestFileEntryToCopy_RemoteSrcWithSha1(t *testing.T) {
	body := []byte("#!/bin/sh\r\necho hi\r\n")
	normalized := []byte("#!/bin/sh\necho hi\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// With ForceLF, the digest applies to the normalized staged bytes.
	ce, err := fileEntryToCopy(config.FileEntry{
		Src: srv.URL, Dst: "/usr/local/bin/x", Sha1: sha1Hex(normalized), ForceLF: true,
	})
	if err != nil {
		t.Fatalf("fileEntryToCopy: %v", err)
	}
	if ce.Src != "" {
		t.Errorf("remote src should be staged as inline data, got Src=%q", ce.Src)
	}
	if string(ce.Data) != string(normalized) {
		t.Errorf("Data = %q, want %q", ce.Data, normalized)
	}
	if ce.ForceLF {
		t.Error("remote data should be normalized before staging")
	}

	// A digest of the pre-normalization source bytes does not match.
	if _, err := fileEntryToCopy(config.FileEntry{
		Src: srv.URL, Dst: "/x", Sha1: sha1Hex(body), ForceLF: true,
	}); err == nil {
		t.Error("expected sha1 mismatch error")
	} else if !strings.Contains(err.Error(), "sha1 mismatch") {
		t.Errorf("error = %v, want sha1 mismatch", err)
	}
}

func TestFileEntryToCopy_RemoteSrcHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fileEntryToCopy(config.FileEntry{Src: srv.URL, Dst: "/x"}); err == nil {
		t.Error("expected error for non-2xx response")
	} else if !strings.Contains(err.Error(), "downloading src") {
		t.Errorf("error = %v, want downloading src", err)
	}
}

func TestFileEntryToCopy_LocalSrc(t *testing.T) {
	dir := t.TempDir()
	body := []byte("local file content\n")
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// A local src keeps Src so directory/symlink/mode semantics in
	// wsl.InjectCopies are preserved.
	ce, err := fileEntryToCopy(config.FileEntry{Src: p, Dst: "/etc/f"})
	if err != nil {
		t.Fatalf("fileEntryToCopy: %v", err)
	}
	if ce.Src != p {
		t.Errorf("local src should stay as Src, got %q", ce.Src)
	}
	if ce.Data != nil {
		t.Errorf("local src should not be inlined, got Data=%q", ce.Data)
	}
}

func TestFileEntryToCopy_LocalSrcWithSha1(t *testing.T) {
	dir := t.TempDir()
	body := []byte("local\r\nfile\r\ncontent\r\n")
	normalized := []byte("local\nfile\ncontent\n")
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Matching digest: the local file is verified but still staged via Src
	// so its mode/symlink/directory semantics are preserved.
	ce, err := fileEntryToCopy(config.FileEntry{
		Src: p, Dst: "/etc/f", Sha1: sha1Hex(normalized), ForceLF: true,
	})
	if err != nil {
		t.Fatalf("fileEntryToCopy: %v", err)
	}
	if ce.Src != p {
		t.Errorf("verified local src should stay as Src, got %q", ce.Src)
	}
	if ce.Data != nil {
		t.Errorf("verified local src should not be inlined, got Data=%q", ce.Data)
	}
	if !ce.ForceLF {
		t.Error("verified local src should retain ForceLF for streamed staging")
	}

	// A digest of the pre-normalization source bytes does not match.
	if _, err := fileEntryToCopy(config.FileEntry{
		Src: p, Dst: "/etc/f", Sha1: sha1Hex(body), ForceLF: true,
	}); err == nil {
		t.Error("expected sha1 mismatch error")
	} else if !strings.Contains(err.Error(), "sha1 mismatch") {
		t.Errorf("error = %v, want sha1 mismatch", err)
	}
}
