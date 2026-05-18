//go:build e2e

// This file contains end-to-end tests for the local Docker daemon import
// path of oci-to-wsl. They are Linux-only and gated behind the `e2e`
// build tag, just like the WSL scenarios in e2e_test.go, so the default
// `go test ./...` run is unaffected.
//
// As with TestE2E, the test runner does NOT build the binary itself; the
// caller must build oci-to-wsl and export its absolute path via
// OCI_TO_WSL_BIN. This keeps the CI workflow a minimal "build + run"
// shell with no scenario-specific assertions.
package main_test

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestE2EDockerDaemon exercises the docker-daemon image source by:
//
//  1. Pulling alpine:latest from the public registry and tagging it under
//     an unreachable registry host so a forced registry fallback
//     deterministically fails.
//  2. Running `oci-to-wsl --image <local-only> --save-tar <file>` and
//     verifying the resulting rootfs tar looks like an Alpine rootfs
//     (contains a non-empty etc/alpine-release).
//  3. Asserting that OCI_TO_WSL_NO_LOCAL=1 disables the local-daemon
//     lookup: the local-only tag must fail, but pulling alpine:latest
//     from the registry must still succeed.
//
// The test is skipped on non-Linux platforms and when docker is not
// available on PATH, mirroring how TestE2E skips on non-Windows hosts.
func TestE2EDockerDaemon(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("docker-daemon e2e requires linux (GOOS=%s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not found on PATH: %v", err)
	}

	// Sanity-check the daemon is reachable before we do anything else
	// (including looking up the oci-to-wsl binary), so a host with the
	// docker CLI but no reachable daemon takes the skip path instead of
	// failing on a missing OCI_TO_WSL_BIN.
	if out, err := runOutput("docker", "version"); err != nil {
		t.Skipf("docker daemon not reachable: %v\n%s", err, out)
	}

	bin := ociToWSLBinary(t)

	const (
		baseImage = "alpine:latest"
		// Tag the local image under an unreachable registry host so the
		// forced-registry path (OCI_TO_WSL_NO_LOCAL=1) deterministically
		// fails instead of accidentally resolving against Docker Hub.
		// Port 1 is reserved/unassigned and refuses connections, so a
		// registry pull of this ref cannot succeed.
		localOnly = "localhost:1/oci-to-wsl-e2e/local-only:notpublished"
		alpineRel = "etc/alpine-release"
	)

	// Seed an image that only exists on the local daemon: pull alpine,
	// then tag it with a name no registry serves.
	if out, err := runOutput("docker", "pull", baseImage); err != nil {
		t.Fatalf("docker pull %s: %v\n%s", baseImage, err, out)
	}
	if out, err := runOutput("docker", "tag", baseImage, localOnly); err != nil {
		t.Fatalf("docker tag %s %s: %v\n%s", baseImage, localOnly, err, out)
	}
	t.Cleanup(func() {
		// Best-effort: drop the local-only tag so repeated local runs
		// don't accumulate dangling references. The base image is left
		// alone since CI tears the runner down anyway.
		_, _ = runOutput("docker", "rmi", "-f", localOnly)
	})
	if out, err := runOutput("docker", "image", "inspect", localOnly); err != nil {
		t.Fatalf("seeded image %s missing from daemon: %v\n%s", localOnly, err, out)
	}

	workDir := t.TempDir()

	t.Run("auto_detect_local_daemon", func(t *testing.T) {
		tarPath := filepath.Join(workDir, "alpine-docker.tar")
		// No OCI_TO_WSL_NO_LOCAL — the daemon path should be tried first
		// and succeed without ever contacting a registry.
		if out, err := runOutput(bin, "--image", localOnly, "--save-tar", tarPath); err != nil {
			t.Fatalf("oci-to-wsl --save-tar from local daemon failed: %v\n%s", err, out)
		}
		assertAlpineRootfsTar(t, tarPath, alpineRel)
	})

	t.Run("no_local_forces_registry_and_fails_for_local_only", func(t *testing.T) {
		tarPath := filepath.Join(workDir, "should-not-exist.tar")
		cmd := exec.Command(bin, "--image", localOnly, "--save-tar", tarPath)
		cmd.Env = append(os.Environ(), "OCI_TO_WSL_NO_LOCAL=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected failure when OCI_TO_WSL_NO_LOCAL=1 forces a registry pull of a local-only image; output:\n%s", out)
		}
		// The output tar should not have been left behind as a useful
		// artifact; we don't strictly require its absence, only that the
		// command exited non-zero, which it did.
	})

	t.Run("no_local_still_pulls_real_registry_image", func(t *testing.T) {
		tarPath := filepath.Join(workDir, "alpine-registry.tar")
		cmd := exec.Command(bin, "--image", baseImage, "--save-tar", tarPath)
		cmd.Env = append(os.Environ(), "OCI_TO_WSL_NO_LOCAL=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("oci-to-wsl --save-tar from registry with OCI_TO_WSL_NO_LOCAL=1 failed: %v\n%s", err, out)
		}
		info, err := os.Stat(tarPath)
		if err != nil {
			t.Fatalf("stat %s: %v", tarPath, err)
		}
		if info.Size() == 0 {
			t.Fatalf("registry-pulled tar %s is empty", tarPath)
		}
	})
}

// assertAlpineRootfsTar verifies path is a non-empty tar archive that
// contains marker (e.g. "etc/alpine-release") as a non-empty regular
// file. Entry names are normalized to forward slashes and any leading
// "./" is stripped, matching how the rest of the codebase compares tar
// header names.
func assertAlpineRootfsTar(t *testing.T, path, marker string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("rootfs tar %s is empty", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

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
		if name != marker {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			t.Fatalf("%s in %s is not a regular file (typeflag=%d)", marker, path, hdr.Typeflag)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s body from %s: %v", marker, path, err)
		}
		if len(body) == 0 {
			t.Fatalf("%s in %s is empty", marker, path)
		}
		t.Logf("%s: %s", marker, strings.TrimSpace(string(body)))
		return
	}
	t.Fatalf("%s not found in %s — does not look like an alpine rootfs", marker, path)
}
