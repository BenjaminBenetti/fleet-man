//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
)

// ImageConnections is how the gateway image server reaches the connections
// whose instances it serves images to (imageserve.Connections): when the
// gateway's registry id or host changes, every gateway-delivered
// StatefulSet is re-pointed at the new reference at once. The StatefulSets
// are OnDelete, so running pods keep the image they pulled, and any pod
// Kubernetes makes later pulls the live reference.
type ImageConnections struct {
	// Pool is the pool the connections are reached through (nil: the
	// daemon's).
	Pool *Pool
}

func (ic ImageConnections) pool() *Pool {
	if ic.Pool != nil {
		return ic.Pool
	}
	return DefaultPool()
}

// Target is connection clusterID's delivery target, its managed pull
// Secrets bound to its namespace.
func (ic ImageConnections) Target(_ context.Context, clusterID string) (kubeimage.Target, error) {
	p := ic.pool()
	c, err := p.Cluster(clusterID)
	if err != nil {
		return kubeimage.Target{}, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return kubeimage.Target{}, err
	}
	id, err := owner.LoadIdentity()
	if err != nil {
		return kubeimage.Target{}, err
	}
	rec := c.Record
	rec.Namespace = c.Namespace()
	return kubeimage.Target{
		Cluster: &rec, DaemonID: id.DaemonID,
		Secrets: PullSecrets{Client: cs, Namespace: c.Namespace()}, Labels: PullSecretLabels(id.Owner(), rec.ID),
	}, nil
}

// RepointImages patches the dev container image of every instance
// StatefulSet of the connection whose image is an old reference in refs to
// its new one.
func (ic ImageConnections) RepointImages(ctx context.Context, t kubeimage.Target, refs map[string]string) error {
	if t.Cluster == nil {
		return errors.New("no connection to re-point")
	}
	p := ic.pool()
	c, err := p.Cluster(t.Cluster.ID)
	if err != nil {
		return err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return err
	}
	own, err := p.owner()
	if err != nil {
		return err
	}
	ns := c.Namespace()
	list, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(own, t.Cluster.ID)})
	if err != nil {
		return fmt.Errorf("listing the connection's instances: %w", err)
	}
	var errs []error
	for _, set := range list.Items {
		for _, ctr := range set.Spec.Template.Spec.Containers {
			to, ok := refs[ctr.Image]
			if ctr.Name != DevContainer || !ok {
				continue
			}
			patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []map[string]string{{"name": DevContainer, "image": to}},
			}}}})
			if _, err := cs.AppsV1().StatefulSets(ns).Patch(ctx, set.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil && !gone(err) {
				errs = append(errs, fmt.Errorf("re-pointing StatefulSet %s: %w", set.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}
