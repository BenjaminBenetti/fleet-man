package podinit

import (
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// reexecEnv marks a pod-init that is already the binary a first pod-init
// chose, so a mismatch never loops.
const reexecEnv = "FLEET_POD_INIT_REEXEC"

// binaryChoice is what verifyBinary decided.
type binaryChoice struct {
	// exec is the binary to re-exec as; "" runs on as this one.
	exec string
	// warning is printed when this binary is not the spec's and no better
	// one exists.
	warning string
}

// chooseBinary decides whether this pod-init (whose own sha256 is self) is the
// one the spec names, and if not, which fleet-<sha> in binDir to re-exec: the
// spec's binary, else its fallback (the one before it), each only if its
// content still has that sha. With neither on disk it runs on as itself,
// with a warning: a pod that boots beats one that crash-loops.
func chooseBinary(spec Spec, self, binDir string) binaryChoice {
	if spec.BinarySHA256 == "" || self == spec.BinarySHA256 {
		return binaryChoice{}
	}
	if os.Getenv(reexecEnv) != "" {
		return binaryChoice{warning: "running as fleet-" + self + ", the binary an earlier pod-init chose"}
	}
	for _, sha := range []string{spec.BinarySHA256, spec.FallbackSHA256} {
		if sha == "" || sha == self {
			continue
		}
		candidate := filepath.Join(binDir, podseed.BinaryName(sha))
		if got, err := podseed.FileSHA256(candidate); err == nil && got == sha {
			return binaryChoice{exec: candidate}
		}
	}
	if self == spec.FallbackSHA256 {
		return binaryChoice{warning: "running as the fallback binary fleet-" + self + "; fleet-" + spec.BinarySHA256 + " is missing"}
	}
	return binaryChoice{warning: "this binary (" + self + ") is not the spec's (" + spec.BinarySHA256 + ") and no fleet-<sha> of the spec's is on disk; running on"}
}
