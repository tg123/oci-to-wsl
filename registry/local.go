package registry

import (
	"fmt"
	"os"
	"strconv"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
)

// envDisableLocal, when set to a value parseable as true by strconv.ParseBool
// (e.g. "1", "t", "true", "True", "TRUE"), disables the automatic lookup of
// the image in supported local container engines and forces oci-to-wsl to
// pull from the remote registry instead.
const envDisableLocal = "OCI_TO_WSL_NO_LOCAL"

// loadFromLocal tries to resolve imageRef against supported local container
// engines (currently: the Docker daemon).
//
// It returns (img, true, nil) when the image is found locally, (nil, false,
// nil) when no local engine has it (or local lookup is disabled), and
// (nil, false, err) on an unexpected error.
//
// The caller should fall back to a remote registry pull when the second
// return value is false.
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
		// The daemon package returns an error both when the daemon is
		// unreachable and when the image simply isn't present. In either
		// case we silently fall back to the registry; the registry pull
		// path will surface a more useful error if that also fails.
		return nil, false, nil
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
