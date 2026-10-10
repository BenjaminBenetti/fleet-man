//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// apiCode is the kubernetes: class of an error reaching a cluster: its API
// server's answer, or why it gave none. "" for an error it says nothing
// about.
func apiCode(err error) kubeerr.Code {
	var netErr net.Error
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
	case errors.Is(err, context.DeadlineExceeded):
		return kubeerr.Timeout
	case errors.As(err, &netErr), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return kubeerr.Unreachable
	}
	return ""
}

// expect is what the record's pod must match (VerifyPod).
func (r InstanceRecord) expect() Expect {
	return Expect{
		ImageDigests:   append([]string(nil), r.ImageDigests...),
		SpecHash:       r.SpecHash,
		StatefulSetUID: types.UID(r.StatefulSetUID),
	}
}
