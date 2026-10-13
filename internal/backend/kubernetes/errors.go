//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// apiCode is the kubernetes: class of an error reaching a cluster: its API
// server's answer, or why it gave none. "" for an error it says nothing
// about.
func apiCode(err error) kubeerr.Code {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrOwnerConflict):
		return kubeerr.OwnerConflict
	case errors.Is(err, ErrClusterNotFound):
		return kubeerr.ClusterMissing
	case errors.Is(err, kubeexec.ErrKubeconfigMissing), errors.Is(err, kubeexec.ErrContextMissing),
		errors.Is(err, kubeexec.ErrPluginMissing), errors.Is(err, kubeexec.ErrMisconfigured):
		return kubeerr.Misconfigured
	case apierrors.IsUnauthorized(err):
		return kubeerr.Unauthorized
	case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota"):
		return kubeerr.Quota
	case apierrors.IsForbidden(err):
		return kubeerr.Forbidden
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err),
		apierrors.IsTooManyRequests(err), apierrors.IsInternalError(err):
		return kubeerr.Unreachable
	case errors.Is(err, context.Canceled):
		return ""
	case kubeerr.Transport(err) != "":
		// Before the deadline: a dial that timed out is the cluster's. A
		// server that was reached but not trusted, or that refused the
		// login, is the connection's (Misconfigured, Unauthorized).
		return kubeerr.Transport(err)
	case errors.Is(err, context.DeadlineExceeded):
		return kubeerr.Timeout
	}
	return ""
}

// expect is what the record's pod must match (VerifyPod).
func (r InstanceRecord) expect() Expect {
	return Expect{
		ImageDigests:   append([]string(nil), r.ImageDigests...),
		ImageConfig:    r.ImageConfig,
		SameContent:    sameContentOn(r.Cluster),
		SpecHash:       r.SpecHash,
		StatefulSetUID: types.UID(r.StatefulSetUID),
		DockerImage:    r.DockerImage,
	}
}

// deliveredExpect is what a pod running del's image on connection clusterID
// must show of it (Expect's image fields); more are digests learned besides.
func deliveredExpect(clusterID string, del *kubeimage.Delivery, more ...string) Expect {
	return Expect{
		ImageDigests: append([]string{del.Digest, del.ImageID, del.Manifest}, more...),
		ImageConfig:  del.Config,
		SameContent:  sameContentOn(clusterID),
	}
}

// sameContentOn is Expect.SameContent from connection clusterID's image
// index: a read of a local file, made only for a container whose imageID is
// none the record names. An index that cannot be read knows nothing.
func sameContentOn(clusterID string) func(string, []string) ([]string, []string) {
	return func(config string, digests []string) ([]string, []string) {
		bare := make([]string, 0, len(digests))
		for _, d := range digests {
			if d = imageDigest(d); d != "" {
				bare = append(bare, d)
			}
		}
		configs, delivered, err := currentEnvironment().images().SameContent(clusterID, config, bare)
		if err != nil {
			flog.Warn("kube verify: reading the connection's image index", "cluster", clusterID, "err", err)
			return nil, nil
		}
		return configs, delivered
	}
}
