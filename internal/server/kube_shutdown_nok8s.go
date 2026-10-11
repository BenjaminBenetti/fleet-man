//go:build nok8s

package server

// kubeShutdownSteps: a -tags nok8s build has no pods to close and no
// Kubernetes jobs to checkpoint.
func (*service) kubeShutdownSteps() []shutdownStep { return nil }

// renewKubePool: no pool in a -tags nok8s build.
func renewKubePool() {}
