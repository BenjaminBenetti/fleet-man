//go:build !nok8s

package svcruntime

import "github.com/BenjaminBenetti/fleet-man/internal/svcruntime/kubert"

// A connection's runtime reports its caches' usage, keeps them up, and
// issues each instance its shared Buildkit certificate.
var (
	_ UsageReporter    = (*kubert.Runtime)(nil)
	_ Maintainer       = (*kubert.Runtime)(nil)
	_ CredentialIssuer = (*kubert.Runtime)(nil)
)

// kubeRuntime is connection clusterID's runtime.
func kubeRuntime(clusterID string) Runtime { return kubert.For(clusterID) }

// presentClusters are the connections the daemon recorded fleetName's
// objects on.
func presentClusters(fleetName string) []string { return kubert.PresentClusters(fleetName) }
