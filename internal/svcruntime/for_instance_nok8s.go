//go:build nok8s

package svcruntime

// kubeRuntime is nil: this build has no Kubernetes backend.
func kubeRuntime(string) Runtime { return nil }

// presentClusters is empty without the Kubernetes backend.
func presentClusters(string) []string { return nil }
