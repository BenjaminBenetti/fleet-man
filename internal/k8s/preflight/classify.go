//go:build !nok8s

package preflight

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// maxMessage bounds a message relayed from the cluster or a plugin.
const maxMessage = kubeexec.MaxPluginMessage

// Classify maps an error from loading a connection or from a request into
// the connection state it means, and a one-line message safe to show any
// client (sanitized, bounded, with no credential material: client-go and
// plugin errors never carry the token itself).
func Classify(err error) (fleetgrpc.KubeClusterState, string) {
	if err == nil {
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, ""
	}
	msg := Message(err)
	var perr *kubeexec.PluginError
	switch {
	case errors.Is(err, kubeexec.ErrKubeconfigMissing), errors.Is(err, kubeexec.ErrContextMissing):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING, msg
	case errors.Is(err, kubeexec.ErrPluginMissing):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING, msg
	case errors.Is(err, kubeexec.ErrMisconfigured):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg
	case errors.As(err, &perr):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, msg
	case apierrors.IsUnauthorized(err):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "the cluster refused the credentials: " + msg
	case apierrors.IsForbidden(err):
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN, msg
	case isCredentialFailure(err):
		// client-go ran the plugin itself (a request between full checks) and
		// it failed: its stderr went to the daemon's log, only the exit made it
		// here.
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "the credential plugin failed: " + msg
	}
	return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE, msg
}

// isCredentialFailure: client-go could not get credentials from the exec
// plugin ("getting credentials: exec: ...").
func isCredentialFailure(err error) bool {
	return strings.Contains(err.Error(), "getting credentials")
}

// IsUnreachable reports a transport failure: the API server could not be
// reached or did not answer (as opposed to answering with a refusal).
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	var urlErr *url.Error
	var certErr x509.UnknownAuthorityError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr), errors.As(err, &certErr):
		return true
	case errors.As(err, &urlErr):
		return !isCredentialFailure(err)
	}
	return apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err)
}

// Message is err's text made safe to relay: control characters and escape
// sequences dropped, at most maxMessage bytes.
func Message(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error())
}

func sanitize(s string) string {
	return kubeexec.SanitizeOutput([]byte(strings.ReplaceAll(s, "\n", " ")))
}

// statusMessage is the API status message of err without the
// "<resource> "<name>" is forbidden: " prefix the API server puts in front of
// an admission refusal.
func statusMessage(err error) string {
	msg := err.Error()
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Message != "" {
		msg = status.Status().Message
	}
	if i := strings.Index(msg, " is forbidden: "); i >= 0 {
		msg = msg[i+len(" is forbidden: "):]
	}
	return sanitize(msg)
}
