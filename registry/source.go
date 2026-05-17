package registry

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Source identifies where an image is loaded from.
type Source int

const (
	// SourceRegistry pulls the image from a remote OCI registry (the default).
	SourceRegistry Source = iota
	// SourceDockerDaemon loads the image from a local Docker daemon
	// using the docker engine API (via DOCKER_HOST / the default socket).
	SourceDockerDaemon
	// SourceContainerd loads the image from a local containerd daemon by
	// shelling out to the `ctr` CLI.
	SourceContainerd
)

const (
	dockerDaemonPrefix = "docker-daemon:"
	containerdPrefix   = "containerd:"
	// containerdNSPrefix lets the caller pick a containerd namespace as
	//   containerd://<namespace>/<image-ref>
	// When the prefix is just "containerd:" the "default" namespace is used.
	containerdNSPrefix = "containerd://"
)

// ParsedRef is the result of decoding an image reference's optional
// source-scheme prefix.
type ParsedRef struct {
	Source    Source
	Ref       string // the underlying image reference (no scheme prefix)
	Namespace string // only meaningful when Source == SourceContainerd
}

// ParseRef extracts a source scheme prefix (if any) from imageRef.
//
// Recognised forms:
//
//	docker-daemon:<image>                  → local docker daemon
//	containerd:<image>                     → local containerd, "default" namespace
//	containerd://<namespace>/<image>       → local containerd, explicit namespace
//	<image>                                → remote registry (default)
//
// The image reference itself may contain colons (for tags or digests) and is
// returned untouched after the scheme prefix is stripped.
func ParseRef(imageRef string) ParsedRef {
	switch {
	case strings.HasPrefix(imageRef, dockerDaemonPrefix):
		return ParsedRef{
			Source: SourceDockerDaemon,
			Ref:    strings.TrimPrefix(imageRef, dockerDaemonPrefix),
		}
	case strings.HasPrefix(imageRef, containerdNSPrefix):
		rest := strings.TrimPrefix(imageRef, containerdNSPrefix)
		ns, ref := "default", rest
		if i := strings.Index(rest, "/"); i > 0 {
			ns, ref = rest[:i], rest[i+1:]
		}
		return ParsedRef{
			Source:    SourceContainerd,
			Ref:       ref,
			Namespace: ns,
		}
	case strings.HasPrefix(imageRef, containerdPrefix):
		return ParsedRef{
			Source:    SourceContainerd,
			Ref:       strings.TrimPrefix(imageRef, containerdPrefix),
			Namespace: "default",
		}
	default:
		return ParsedRef{Source: SourceRegistry, Ref: imageRef}
	}
}

// loadImage resolves the parsed reference into a v1.Image by dispatching to
// the appropriate backend (remote registry, docker daemon, or containerd).
//
// The returned cleanup func is always non-nil and must be invoked after the
// caller is done reading from the image — some backends (notably containerd)
// stage a temporary tarball on disk that needs to be removed.
func loadImage(pr ParsedRef, platform *v1.Platform, opts PullOptions) (v1.Image, func(), error) {
	noop := func() {}
	switch pr.Source {
	case SourceDockerDaemon:
		img, err := loadFromDockerDaemon(pr.Ref)
		return img, noop, err
	case SourceContainerd:
		return loadFromContainerd(pr.Namespace, pr.Ref, platform)
	default:
		ref, err := name.ParseReference(pr.Ref)
		if err != nil {
			return nil, noop, fmt.Errorf("parsing image reference %q: %w", pr.Ref, err)
		}
		craneOpts := buildCraneOptions(ref, platform, opts)
		fmt.Printf("Pulling image %s (%s/%s) ...\n", ref, platform.OS, platform.Architecture)
		img, err := crane.Pull(pr.Ref, craneOpts...)
		if err != nil {
			return nil, noop, fmt.Errorf("pulling image %q: %w", pr.Ref, err)
		}
		return img, noop, nil
	}
}

// loadFromDockerDaemon fetches an image from the local Docker daemon using
// `docker save` semantics (streamed through go-containerregistry's daemon
// package). The daemon socket is discovered via the standard DOCKER_HOST
// environment variable.
func loadFromDockerDaemon(imageRef string) (v1.Image, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}
	fmt.Printf("Loading image %s from local docker daemon ...\n", ref)
	img, err := daemon.Image(ref)
	if err != nil {
		return nil, fmt.Errorf("loading %q from docker daemon: %w", imageRef, err)
	}
	return img, nil
}

// loadFromContainerd exports an image from the local containerd daemon by
// shelling out to `ctr images export <tmp.tar> <image>`. `ctr` writes an OCI
// image layout packed into a tar; this function extracts that tar to a temp
// directory and resolves it via go-containerregistry's layout package.
//
// `ctr` is shipped as part of containerd itself, so no extra Go dependency on
// the heavy containerd client is required.
//
// The returned cleanup function deletes the temporary directory and must be
// invoked once the image has been fully consumed.
func loadFromContainerd(namespace, imageRef string, platform *v1.Platform) (v1.Image, func(), error) {
	noop := func() {}
	if namespace == "" {
		namespace = "default"
	}
	ctr, err := exec.LookPath("ctr")
	if err != nil {
		return nil, noop, fmt.Errorf("ctr CLI not found in PATH (required to load from containerd): %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "oci-to-wsl-ctr-")
	if err != nil {
		return nil, noop, fmt.Errorf("creating temp dir for ctr export: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	tarPath := filepath.Join(tmpDir, "image.tar")
	layoutDir := filepath.Join(tmpDir, "layout")
	if err := os.MkdirAll(layoutDir, 0o700); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("creating layout dir: %w", err)
	}

	fmt.Printf("Loading image %s from containerd (namespace=%s) ...\n", imageRef, namespace)
	cmd := exec.CommandContext(context.Background(), ctr, "-n", namespace, "images", "export", tarPath, imageRef)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("ctr export %q (namespace=%s): %w", imageRef, namespace, err)
	}

	if err := extractTar(tarPath, layoutDir); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("extracting ctr export tarball: %w", err)
	}

	img, err := imageFromOCILayout(layoutDir, platform)
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("reading OCI layout from ctr export: %w", err)
	}
	return img, cleanup, nil
}

// imageFromOCILayout opens an OCI image layout at dir and returns the v1.Image
// whose platform matches the requested one. When the layout's top-level index
// points directly at a single manifest (the common case for `ctr export` of a
// single-platform image), that manifest is returned regardless of the platform
// filter.
func imageFromOCILayout(dir string, platform *v1.Platform) (v1.Image, error) {
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, err
	}
	return findImageInIndex(idx, platform)
}

// findImageInIndex walks an image index recursively to find a manifest that
// matches the requested platform. If the index has exactly one image manifest
// (no platform fan-out) that manifest is returned directly.
func findImageInIndex(idx v1.ImageIndex, platform *v1.Platform) (v1.Image, error) {
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}

	// Collect direct image descriptors and recurse into nested indices.
	var imageDescs []v1.Descriptor
	for _, desc := range manifest.Manifests {
		switch desc.MediaType {
		case types.OCIImageIndex, types.DockerManifestList:
			sub, err := idx.ImageIndex(desc.Digest)
			if err != nil {
				return nil, err
			}
			if img, err := findImageInIndex(sub, platform); err == nil {
				return img, nil
			}
		case types.OCIManifestSchema1, types.DockerManifestSchema2:
			if platform != nil && desc.Platform != nil && !platformMatches(desc.Platform, platform) {
				continue
			}
			return idx.Image(desc.Digest)
		default:
			imageDescs = append(imageDescs, desc)
		}
	}

	// Fall back to the first image-shaped descriptor when nothing matched the
	// platform filter — this handles `ctr export` of a single-platform image
	// where the manifest entry has no Platform field at all.
	if len(imageDescs) > 0 {
		return idx.Image(imageDescs[0].Digest)
	}
	// Or the first plain image manifest if it was filtered out above.
	for _, desc := range manifest.Manifests {
		if desc.MediaType == types.OCIManifestSchema1 || desc.MediaType == types.DockerManifestSchema2 {
			return idx.Image(desc.Digest)
		}
	}
	return nil, fmt.Errorf("no image manifest found in OCI layout index")
}

func platformMatches(have, want *v1.Platform) bool {
	if want.OS != "" && have.OS != "" && want.OS != have.OS {
		return false
	}
	if want.Architecture != "" && have.Architecture != "" && want.Architecture != have.Architecture {
		return false
	}
	return true
}

// extractTar extracts a (uncompressed) tar archive at tarPath into dst.
// It only handles regular files and directories — sufficient for the OCI
// image layout written by `ctr images export`.
func extractTar(tarPath, dst string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Sanitize entry name to prevent path traversal outside dst.
		clean := filepath.Clean("/" + hdr.Name)
		target := filepath.Join(dst, clean)
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && target != filepath.Clean(dst) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks / special files — `ctr export` only writes
			// regular files and directories for OCI image layouts.
		}
	}
}
