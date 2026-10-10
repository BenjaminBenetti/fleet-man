package imageserve

import (
	"strings"
)

// PullFailureKind is why a node could not pull a gateway-delivered image.
type PullFailureKind string

const (
	// PullDaemonOffline: the gateway could not reach the daemon. For 10
	// minutes after fleetd drops off it answers 502; after that it forgets
	// the daemon's session and answers 404, which reads like a missing
	// digest. Once fleetd is back the kubelet's retry pulls.
	PullDaemonOffline PullFailureKind = "daemon_offline"
	// PullGatewayUnreachable: the node could not reach the gateway at all
	// (it is down, or the node has no route to it).
	PullGatewayUnreachable PullFailureKind = "gateway_unreachable"
	// PullCertificate: the node does not trust the gateway's certificate.
	PullCertificate PullFailureKind = "certificate"
	// PullCredential: the pull credential was refused.
	PullCredential PullFailureKind = "credential"
	// PullStaleReference: the pod names a registry id or host the daemon
	// no longer has; the re-point gives it the current one.
	PullStaleReference PullFailureKind = "stale_reference"
	// PullDigestUnknown: the daemon does not serve the image to this
	// connection (any more); delivering it again can fix that.
	PullDigestUnknown PullFailureKind = "digest_unknown"
	// PullOther: anything else, with the kubelet's message.
	PullOther PullFailureKind = "other"
)

// PullFailure is a classified image pull failure.
type PullFailure struct {
	Kind PullFailureKind
	// Message says what happened, for the instance's error.
	Message string
	// Hint is the fix, when there is one.
	Hint string
	// Redeliver reports whether delivering the image again can fix it.
	Redeliver bool
}

// pull failure reasons a kubelet reports on a waiting container.
const (
	reasonErrImagePull     = "ErrImagePull"
	reasonImagePullBackOff = "ImagePullBackOff"
)

// ClassifyPull classifies a pod's failure to pull a gateway-delivered image
// from its waiting container's reason and message (ErrImagePull or
// ImagePullBackOff; false for anything else). ref is the image the pod
// names. A "not found" is told apart by what fleetd knows now: an image the
// connection still serves under the current registry prefix was not found
// because the gateway had forgotten the daemon (it was offline for more than
// 10 minutes), not because the digest is gone.
func (g *Gateway) ClassifyPull(clusterID, ref, reason, message string) (PullFailure, bool) {
	if reason != reasonErrImagePull && reason != reasonImagePullBackOff {
		return PullFailure{}, false
	}
	msg := strings.ToLower(message)
	refPrefix, digest, _ := strings.Cut(ref, "/"+repoName+"@")
	host := registryHost(refPrefix)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("x509", "certificate signed by unknown authority", "failed to verify certificate", "certificate is not valid"):
		return PullFailure{Kind: PullCertificate,
			Message: "the node does not trust the TLS certificate of the gateway at " + host,
			Hint:    "serve the gateway with a publicly trusted certificate, or install its CA on every node (containerd: /etc/containerd/certs.d/" + host + "/ca.crt)"}, true
	case has("401 unauthorized", "unauthorized", "failed to authorize", "authorization failed"):
		return PullFailure{Kind: PullCredential,
			Message: "the gateway pull credential was refused",
			Hint:    "fleet re-applies the connection's pull Secret at the next delivery; check that the pod lists it in imagePullSecrets"}, true
	case has("502 bad gateway", "503 service unavailable", "504 gateway timeout", "status 502", "status 503", "status 504"):
		return PullFailure{Kind: PullDaemonOffline,
			Message: "the image pull failed while the daemon was offline or reconnecting: the gateway at " + host + " could not reach fleetd",
			Hint:    "keep fleetd running while pods pull; the kubelet retries on its own"}, true
	case has("connection refused", "no such host", "i/o timeout", "network is unreachable", "no route to host", "connection reset", "tls handshake timeout", "server misbehaving", "dial tcp"):
		return PullFailure{Kind: PullGatewayUnreachable,
			Message: "the node could not reach the gateway at " + host,
			Hint:    "check that the gateway is running and that the cluster's nodes can reach " + host}, true
	case has("not found", "404", "manifest unknown", "name unknown", "blob unknown"):
		return g.classifyNotFound(clusterID, refPrefix, digest), true
	}
	return PullFailure{Kind: PullOther, Message: "pulling " + ref + " failed: " + message}, true
}

func (g *Gateway) classifyNotFound(clusterID, refPrefix, digest string) PullFailure {
	current := ""
	if g.Tunnel != nil {
		current, _ = g.Tunnel.Registry()
	}
	if current == "" && g.Store != nil {
		current, _ = g.Store.Prefix(clusterID)
	}
	if current != "" && refPrefix != current {
		return PullFailure{Kind: PullStaleReference,
			Message: "the pod names an old gateway registry (" + refPrefix + "); the daemon's is now " + current,
			Hint:    "fleet re-points the instance's StatefulSet; delete the pod to pull the current reference"}
	}
	if g.Store != nil && g.Store.Serves(clusterID, digest) {
		return PullFailure{Kind: PullDaemonOffline,
			Message: "the image pull failed while the daemon was offline: the gateway had dropped fleetd's session (offline for more than 10 minutes) and answered not found",
			Hint:    "keep fleetd running while pods pull; the kubelet retries on its own"}
	}
	return PullFailure{Kind: PullDigestUnknown,
		Message:   "the daemon no longer serves image " + short(digest) + " to this cluster",
		Hint:      "start the instance again to deliver it, or rebuild it",
		Redeliver: true}
}
