// Package registry handles pulling OCI images and exporting them as rootfs tarballs.
package registry

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/schollz/progressbar/v3"
)

// PullOptions controls how an image is pulled.
type PullOptions struct {
	// Authenticator is used for registry authentication.
	// If nil, the default keychain (docker config, env vars) is used unless
	// the registry is detected as Azure Container Registry, in which case
	// the Azure SDK credential chain (az CLI + interactive browser) runs
	// automatically.
	Authenticator authn.Authenticator

	// Platform selects a specific OS/arch from a multi-arch manifest list.
	// Format is "os/arch" (e.g. "linux/amd64", "linux/arm64"). When empty
	// the host's runtime arch is used (with OS forced to linux).
	Platform string
}

// PullToTar pulls the OCI image identified by imageRef and writes the flattened
// rootfs tar to w.  The flattened tar is suitable for use with "wsl --import".
//
// If the image is already present in a supported local container engine (the
// local Docker daemon) it is loaded from there instead of being pulled from
// the remote registry. Set OCI_TO_WSL_NO_LOCAL=1 to disable the local-engine
// lookup and always go to the registry (the value is parsed with
// strconv.ParseBool, so any of its accepted truthy spellings work). The
// local-engine lookup is also skipped when opts.Platform is non-empty, since
// the local daemon only holds whatever platform was last pulled for the tag
// and would silently produce a tar for the wrong arch.
func PullToTar(imageRef string, w io.Writer, opts PullOptions) error {
	platform, err := resolvePlatform(opts.Platform)
	if err != nil {
		return err
	}

	// Skip the local-daemon probe when a specific platform was requested:
	// the daemon stores one image per tag and we cannot guarantee it
	// matches opts.Platform, so fall straight through to the registry pull
	// (which honours crane.WithPlatform).
	var (
		img   v1.Image
		found bool
	)
	if opts.Platform == "" {
		img, found, err = loadFromLocal(imageRef)
		if err != nil {
			return err
		}
	}
	if !found {
		ref, err := name.ParseReference(imageRef)
		if err != nil {
			return fmt.Errorf("parsing image reference %q: %w", imageRef, err)
		}

		pullOpts := buildCraneOptions(ref, platform, opts)

		fmt.Printf("Pulling image %s (%s/%s) ...\n", ref, platform.OS, platform.Architecture)
		img, err = crane.Pull(imageRef, pullOpts...)
		if err != nil {
			return fmt.Errorf("pulling image %q: %w", imageRef, err)
		}
	}

	// crane.Export writes uncompressed tar bytes to w. Image manifests only
	// contain compressed layer sizes, so use a 3x heuristic to estimate the
	// final tar size for the progress bar's total.
	const uncompressedRatio = 3
	compressed := compressedSize(img)
	estimatedTotal := compressed
	if compressed > 0 {
		estimatedTotal = compressed * uncompressedRatio
	}

	desc := "Exporting rootfs "
	if estimatedTotal > 0 {
		desc = fmt.Sprintf("Exporting rootfs (est %s)", humanBytes(estimatedTotal))
	}

	bar := progressbar.NewOptions64(estimatedTotal,
		progressbar.OptionSetDescription(desc),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(30),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionSetPredictTime(estimatedTotal > 0),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionOnCompletion(func() { fmt.Fprintln(os.Stderr) }),
	)
	defer func() { _ = bar.Close() }()

	// crane.Export (via go-containerregistry's mutate.Extract) emits
	// backslash-separated header names on Windows, which produces a tar that
	// `wsl --import` accepts but extracts as a flat pile of weirdly named
	// files at the root (the rootfs ends up effectively empty and unbootable).
	// Pipe the exported tar through normalizeTarPaths to rewrite header names
	// to use forward slashes before they reach the destination writer.
	pr, pw := io.Pipe()
	exportErr := make(chan error, 1)
	go func() {
		err := crane.Export(img, io.MultiWriter(pw, &safeBarWriter{bar: bar}))
		_ = pw.CloseWithError(err)
		exportErr <- err
	}()

	if err := normalizeTarPaths(pr, w); err != nil {
		_ = pr.CloseWithError(err)
		// Drain the export goroutine so it doesn't leak.
		<-exportErr
		return fmt.Errorf("normalizing rootfs tar: %w", err)
	}
	if err := <-exportErr; err != nil {
		return fmt.Errorf("exporting rootfs tar: %w", err)
	}
	return nil
}

// safeBarWriter forwards bytes to a progressbar but never errors, even if the
// running count exceeds the bar's configured total (which can happen when the
// estimated uncompressed size is too low).
type safeBarWriter struct {
	bar *progressbar.ProgressBar
}

func (w *safeBarWriter) Write(p []byte) (int, error) {
	_ = w.bar.Add(len(p))
	return len(p), nil
}

// normalizeTarPaths copies a tar stream from r to w, replacing Windows-style
// backslash separators in each header's Name and Linkname with forward slashes.
//
// This works around an upstream bug in github.com/google/go-containerregistry's
// mutate.Extract (used by crane.Export): it calls filepath.Clean / filepath.Join
// on tar entry names, which on Windows produces paths like "bin\busybox"
// instead of "bin/busybox". Such a tar is technically valid but POSIX tar
// extractors (including the one inside `wsl --import`) treat each entry as a
// single oddly-named file at the root, leaving the resulting rootfs effectively
// empty and unbootable.
//
// On non-Windows hosts the input never contains backslashes, so this pass is
// a no-op copy and remains safe to run unconditionally.
func normalizeTarPaths(r io.Reader, w io.Writer) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		hdr.Name = strings.ReplaceAll(hdr.Name, `\`, "/")
		if hdr.Linkname != "" {
			hdr.Linkname = strings.ReplaceAll(hdr.Linkname, `\`, "/")
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing tar header %q: %w", hdr.Name, err)
		}
		if hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("copying tar body %q: %w", hdr.Name, err)
			}
		}
	}
	return tw.Close()
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// compressedSize returns the sum of compressed layer sizes from the image
// manifest. Returns -1 (indeterminate) when the manifest is unavailable.
func compressedSize(img v1.Image) int64 {
	m, err := img.Manifest()
	if err != nil || m == nil {
		return -1
	}
	var total int64
	for _, l := range m.Layers {
		total += l.Size
	}
	if total <= 0 {
		return -1
	}
	return total
}

// buildCraneOptions constructs the crane.Option slice, wiring in the right
// authenticator (ACR browser flow, explicit creds, or the default keychain)
// and the requested platform.
func buildCraneOptions(ref name.Reference, platform *v1.Platform, opts PullOptions) []crane.Option {
	craneOpts := []crane.Option{crane.WithPlatform(platform)}

	if opts.Authenticator != nil {
		craneOpts = append(craneOpts, crane.WithAuth(opts.Authenticator))
		return craneOpts
	}

	// Auto-detect ACR registries and use browser-based auth.
	registry := ref.Context().RegistryStr()
	if isACR(registry) {
		fmt.Printf("Detected ACR registry %s – authenticating via Azure SDK ...\n", registry)
		auth, err := NewACRAuthenticator(registry)
		if err != nil {
			// Fall through to default keychain; the error will surface during pull.
			fmt.Printf("Warning: ACR browser auth failed: %v – falling back to keychain\n", err)
		} else {
			craneOpts = append(craneOpts, crane.WithAuth(auth))
			return craneOpts
		}
	}

	// Default: use the Docker credential keychain (config.json / env vars).
	craneOpts = append(craneOpts, crane.WithAuthFromKeychain(authn.DefaultKeychain))
	return craneOpts
}

// resolvePlatform converts a "os/arch" string into a *v1.Platform, falling
// back to the host runtime arch when the input is empty.
func resolvePlatform(spec string) (*v1.Platform, error) {
	if spec == "" {
		return &v1.Platform{OS: "linux", Architecture: hostArch()}, nil
	}
	parts := strings.SplitN(spec, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid platform %q (expected os/arch, e.g. linux/amd64)", spec)
	}
	return &v1.Platform{OS: parts[0], Architecture: normalizeArch(parts[1])}, nil
}

func hostArch() string {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		return "amd64"
	}
}

func normalizeArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return a
	}
}

// isACR returns true when the registry host looks like an Azure Container Registry.
func isACR(registry string) bool {
	lower := strings.ToLower(registry)
	return strings.HasSuffix(lower, ".azurecr.io") ||
		strings.HasSuffix(lower, ".azurecr.cn") ||
		strings.HasSuffix(lower, ".azurecr.us")
}
