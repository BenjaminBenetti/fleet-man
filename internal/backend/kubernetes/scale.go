//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Scale sets the instance's StatefulSet to replicas (0 stops it, 1 starts
// it): a merge patch of spec.replicas under fleet's field manager, which
// needs only the patch verb fleet holds on StatefulSets. A later server-side
// apply of the whole StatefulSet that sets replicas takes the field back
// with force.
func (p *Pool) Scale(ctx context.Context, clusterID, sts string, replicas int32) error {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return err
	}
	body := []byte(`{"spec":{"replicas":` + strconv.Itoa(int(replicas)) + `}}`)
	if _, err := cs.AppsV1().StatefulSets(c.Namespace()).Patch(ctx, sts, types.MergePatchType, body, metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		return fmt.Errorf("scaling StatefulSet %s/%s to %d: %w", c.Namespace(), sts, replicas, err)
	}
	return nil
}

// Park stops a crash-looping instance: the run-end hook first (its runs'
// output is saved while the cluster still has it), then a scale to 0. The
// caller records the instance stopped, with the loop's message.
func (p *Pool) Park(ctx context.Context, clusterID, sts string) error {
	if hook := p.runEnded(); hook != nil {
		p.instMu.Lock()
		var end *RunEnd
		if t := p.tracks[trackKey{clusterID, sts}]; t != nil && t.uid != "" {
			end = &RunEnd{Cluster: clusterID, Namespace: t.ns, StatefulSet: sts, Pod: sts + "-0", PodUID: t.uid, RestartCount: t.count, Why: RunParked}
		}
		p.instMu.Unlock()
		if end != nil {
			hook(ctx, *end)
		}
	}
	return p.Scale(ctx, clusterID, sts, 0)
}
