//go:build !nok8s

package svcruntime

import "github.com/BenjaminBenetti/fleet-man/internal/svcruntime/kubert"

// kubeRuntime is connection clusterID's runtime.
func kubeRuntime(clusterID string) Runtime { return kubert.For(clusterID) }

// presentClusters are the connections the daemon recorded fleetName's
// objects on.
func presentClusters(fleetName string) []string { return kubert.PresentClusters(fleetName) }
