//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// gateway_pull.go judges the failed pulls of a gateway-delivered image while
// fleet waits on an instance's pod (a create, a start), with the gateway's
// own knowledge of why (imageserve.Gateway.ClassifyPull): a not found that
// means the daemon was offline is told apart from a digest the daemon no
// longer serves. The kubelet retries a pull, so a failure that can pass (the
// daemon reconnecting, the gateway restarting, a credential being rotated)
// is waited out to the image pull deadline, which then says what it was; one
// that cannot pass on its own fails the wait at once, with its fix.

// GatewayPulls is the gateway image server as the pods pulling through it
// see it (imageserve.Gateway).
type GatewayPulls interface {
	// ClassifyPull says why a pod's pull of a gateway-delivered image
	// failed, from its waiting container's reason and message.
	ClassifyPull(clusterID, ref, reason, message string) (imageserve.PullFailure, bool)
	// Warm opens the registry connection ahead of a pull fleetd starts.
	Warm()
}

// gatewayPullsLast are the failures a waiting kubelet's retries do not fix.
var gatewayPullsLast = map[imageserve.PullFailureKind]bool{
	imageserve.PullCertificate:    true,
	imageserve.PullRegistryOff:    true,
	imageserve.PullStaleReference: true,
	imageserve.PullDigestUnknown:  true,
}

// gatewayPulls watches one wait's pulls of a gateway-delivered image.
type gatewayPulls struct {
	gw      GatewayPulls
	p       *Pool
	cluster string
	sts     string
	// last is the latest failure that may pass.
	last *imageserve.PullFailure
}

// newGatewayPulls watches the pulls of instance sts's pod on connection
// cluster when its image is gateway-delivered and the daemon serves the
// gateway; nil otherwise (its methods are then no-ops).
func newGatewayPulls(p *Pool, strategy kubeimage.Strategy, cluster, sts string) *gatewayPulls {
	gw := currentEnvironment().Gateway
	if strategy != kubeimage.StrategyGateway || gw == nil {
		return nil
	}
	return &gatewayPulls{gw: gw, p: p, cluster: cluster, sts: sts}
}

// check is a WaitOptions.Check step: the dev container's failed pull,
// classified. A failure that lasts ends the wait.
func (g *gatewayPulls) check(ctx context.Context) error {
	if g == nil {
		return nil
	}
	c, err := g.p.Cluster(g.cluster)
	if err != nil {
		return nil
	}
	objs, err := g.p.objects(c)
	if err != nil {
		return nil
	}
	pod, err := objs.Pod(ctx, c.Namespace(), names.Pod(g.sts))
	if err != nil {
		return nil
	}
	st := containerStatus(pod, DevContainer)
	if st == nil || st.State.Waiting == nil {
		return nil
	}
	ref := ""
	for _, ctr := range pod.Spec.Containers {
		if ctr.Name == DevContainer {
			ref = ctr.Image
		}
	}
	pf, ok := g.gw.ClassifyPull(g.cluster, ref, st.State.Waiting.Reason, st.State.Waiting.Message)
	if !ok {
		return nil
	}
	if gatewayPullsLast[pf.Kind] {
		return kubeerr.WithHint(kubeerr.ImagePull.Narrow(string(pf.Kind)), errors.New(pf.Message), pf.Hint)
	}
	g.last = &pf
	return nil
}

// explain gives an image pull deadline the failure the gateway saw; any
// other error is returned as it is.
func (g *gatewayPulls) explain(err error) error {
	var de *DeadlineError
	if g == nil || g.last == nil || !errors.As(err, &de) || de.Kind != CodeImagePull {
		return err
	}
	out := *de
	out.Detail = g.last.Message
	if g.last.Hint != "" {
		out.Detail += "; " + g.last.Hint
	}
	return &out
}

// warmGateway opens the gateway's registry connection ahead of a start's
// pull of a gateway-delivered image.
func warmGateway(strategy kubeimage.Strategy) {
	if gw := currentEnvironment().Gateway; gw != nil && strategy == kubeimage.StrategyGateway {
		gw.Warm()
	}
}
