package fleet

import "time"

// KubeExecCredential is the complete transport fleetd itself uses to reach a
// Kubernetes connection's API server: where the server is, how to trust it,
// the proxy and impersonation fleetd's client applies, and a live
// credential (a bearer token, a client certificate and key, or a basic-auth
// login), from fleetd's own cache. IssueKubeExecCredential hands it to the
// `fleet kube-exec` shim on the daemon host, whose direct-mode TTY attaches
// then dial the API server exactly as fleetd would, without running a
// credential plugin per attach.
//
// It holds a secret: it is never logged, persisted, backed up or sent to a
// remote caller (the RPC is local-socket only).
type KubeExecCredential struct {
	// ClusterID is the connection (KubeCluster.ID) it is for.
	ClusterID string
	// Server is the API server's URL, path prefix included.
	Server string
	// CertificateAuthorityData is the PEM bundle that verifies the server;
	// empty means the system roots.
	CertificateAuthorityData []byte
	// TLSServerName overrides the name the server's certificate is checked
	// against; InsecureSkipTLSVerify skips the check (as the kubeconfig says).
	TLSServerName         string
	InsecureSkipTLSVerify bool
	// ProxyURL is the proxy fleetd reaches the server through (the
	// kubeconfig's proxy-url, else fleetd's proxy environment); empty means
	// none, whatever the caller's own environment says.
	ProxyURL string
	// Impersonate is the identity the kubeconfig asks the server to act as.
	Impersonate KubeImpersonation
	// Auth is how the credential authenticates (kubeexec.AuthKind).
	Auth string
	// BearerToken, or ClientCertificateData with ClientKeyData (PEM), or
	// Username with Password: the credential itself.
	BearerToken           string
	ClientCertificateData []byte
	ClientKeyData         []byte
	Username              string
	Password              string
	// ExpiresAt is when the credential stops working; zero when it has no
	// known expiry. Ask again after it (or when the server refuses it).
	ExpiresAt time.Time
}

// KubeImpersonation is a kubeconfig's impersonation (`as`, `as-uid`,
// `as-groups`, `as-user-extra`).
type KubeImpersonation struct {
	User   string
	UID    string
	Groups []string
	Extra  map[string][]string
}
