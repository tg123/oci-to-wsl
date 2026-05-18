package registry

import (
	"fmt"
	"os"
	"strconv"

	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/moby/moby/client"
)

// envDisableLocal, when set to a value parseable as true by strconv.ParseBool
// (e.g. "1", "t", "true", "True", "TRUE"), disables the automatic lookup of
// the image in supported local container engines and forces oci-to-wsl to
// pull from the remote registry instead.
const envDisableLocal = "OCI_TO_WSL_NO_LOCAL"

// loadFromLocal tries to resolve imageRef against supported local container
// engines (currently: the Docker daemon).
//
// It returns (img, true, nil) when the image is found locally and
// (nil, false, nil) when the caller should silently fall back to a registry
// pull — i.e. when local lookup is disabled via OCI_TO_WSL_NO_LOCAL, when
// the daemon is unreachable (e.g. Docker is not installed/running), or when
// the daemon simply doesn't have the image.
//
// Any other unexpected error from the daemon (corrupt image, permission
// denied, malformed reference, …) is returned as (nil, false, err) so the
// caller can surface it rather than masking it with a registry-pull failure.
func loadFromLocal(imageRef string) (v1.Image, bool, error) {
	if isLocalDisabled() {
		return nil, false, nil
	}
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, false, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}
	img, err := daemon.Image(ref)
	if err != nil {
		// "Image not present" and "daemon unreachable" are both expected
		// reasons to fall through to the registry path; everything else
		// is genuinely unexpected and should be surfaced.
		if errdefs.IsNotFound(err) || client.IsErrConnectionFailed(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("loading %q from local docker daemon: %w", imageRef, err)
	}
	fmt.Printf("Found image %s in local docker daemon, using it instead of pulling from registry.\n", ref)
	return img, true, nil
}

// isLocalDisabled reports whether the user has opted out of local-engine
// lookups via the OCI_TO_WSL_NO_LOCAL environment variable. The value is
// parsed with strconv.ParseBool, so any of 1/t/T/TRUE/true/True (and their
// false-y counterparts) are accepted.
func isLocalDisabled() bool {
	v, err := strconv.ParseBool(os.Getenv(envDisableLocal))
	if err != nil {
		return false
	}
	return v
}
