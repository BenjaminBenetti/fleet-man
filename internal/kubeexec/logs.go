//go:build !nok8s

package kubeexec

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// logs.go reads a pod container's log through the API server (pods/log), the
// live half of the Kubernetes backend's `kube-exec logs`: the container.log
// fleetd saved on disk is the earlier runs, this is the running pod's own
// output.

// PodLogsRequest names one container's log to read.
type PodLogsRequest struct {
	Namespace string
	Pod       string
	Container string
	// Follow keeps the stream open until ctx ends; Previous reads the
	// previous (crashed) container's log instead of the running one; TailLines
	// limits the start (0: from the beginning).
	Follow    bool
	Previous  bool
	TailLines *int64
}

// PodLogs streams the pod container's log to w. ctx bounds it (cancelling a
// Follow ends it). The caller built cfg from an issued credential
// (RESTConfigFor) or the kubeconfig.
func PodLogs(ctx context.Context, cfg *rest.Config, req PodLogsRequest, w io.Writer) error {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	return PodLogsClient(ctx, cs, req, w)
}

// PodLogsClient is PodLogs with a clientset of the caller's own.
func PodLogsClient(ctx context.Context, cs kubernetes.Interface, req PodLogsRequest, w io.Writer) error {
	if req.Namespace == "" || req.Pod == "" {
		return fmt.Errorf("pod logs: no namespace or pod")
	}
	opts := &corev1.PodLogOptions{Container: req.Container, Follow: req.Follow, Previous: req.Previous, TailLines: req.TailLines}
	stream, err := cs.CoreV1().Pods(req.Namespace).GetLogs(req.Pod, opts).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = io.Copy(w, stream)
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}
