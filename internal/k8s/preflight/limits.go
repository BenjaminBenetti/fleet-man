//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// limits.go reads what the namespace's LimitRanges and ResourceQuotas do to
// an instance: a LimitRange maximum the memory limit is clamped to (or the
// memory request breaks), the default a LimitRange gives a container with
// memory_limit off, the quota headroom left for one more instance, and a
// quota on limits the instance sets none for.

// limitRanges is what the namespace's LimitRanges give a container that
// sets no limit (the lowest default of each), and the lowest maximum memory
// they allow a container or a pod.
type limitRanges struct {
	count                     int
	defaultMemory, defaultCPU *resource.Quantity
	maxMemory                 *resource.Quantity
}

// readLimitRanges lists the namespace's LimitRanges once: the canaries
// render the instance's limits with them, and the limits check reports
// them.
func (r *run) readLimitRanges(ctx context.Context) (*limitRanges, error) {
	if r.lrRead {
		return r.lr, r.lrErr
	}
	r.lrRead = true
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.CoreV1().LimitRanges(r.t.Cluster.Namespace).List(cctx, metav1.ListOptions{})
	if err != nil {
		r.lrErr = err
		return nil, err
	}
	lowest := func(have *resource.Quantity, l corev1.ResourceList, name corev1.ResourceName) *resource.Quantity {
		if q, ok := l[name]; ok && (have == nil || q.Cmp(*have) < 0) {
			return &q
		}
		return have
	}
	lr := &limitRanges{count: len(list.Items)}
	for _, item := range list.Items {
		for _, l := range item.Spec.Limits {
			switch l.Type {
			case corev1.LimitTypeContainer:
				lr.defaultMemory = lowest(lr.defaultMemory, l.Default, corev1.ResourceMemory)
				lr.defaultCPU = lowest(lr.defaultCPU, l.Default, corev1.ResourceCPU)
				lr.maxMemory = lowest(lr.maxMemory, l.Max, corev1.ResourceMemory)
			case corev1.LimitTypePod:
				lr.maxMemory = lowest(lr.maxMemory, l.Max, corev1.ResourceMemory)
			}
		}
	}
	r.lr = lr
	return lr, nil
}

// instanceLimits are the dev container's limits as the connection's
// settings give them, before a devcontainer's -m or --cpus (podspec's
// finishResources): memory_limit clamped to the LimitRange maximum and never
// below the memory request (with memory_limit off, the request where the
// LimitRange's default is below it), resources.cpu_limit never below the
// CPU request. nil is a limit fleet does not set; lr nil is LimitRanges not
// known.
func instanceLimits(c *fleet.KubeCluster, lr *limitRanges) (mem, cpu *resource.Quantity) {
	if lr == nil {
		lr = &limitRanges{}
	}
	memReq := quantity(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)
	if s := c.InstanceMemoryLimit(); s != "" {
		if q, err := resource.ParseQuantity(s); err == nil {
			if lr.maxMemory != nil && lr.maxMemory.Cmp(q) < 0 {
				q = lr.maxMemory.DeepCopy()
			}
			if memReq.Cmp(q) > 0 {
				q = memReq
			}
			mem = &q
		}
	} else if lr.defaultMemory != nil && lr.defaultMemory.Cmp(memReq) < 0 {
		mem = &memReq
	}
	if s := c.InstanceCPULimit(); s != "" {
		if q, err := resource.ParseQuantity(s); err == nil {
			if req := quantity(c.Resources.CPURequest, fleet.DefaultKubeCPURequest); req.Cmp(q) > 0 {
				q = req
			}
			cpu = &q
		}
	}
	return mem, cpu
}

// quantity reads a setting, or its default when unset; zero when it does
// not parse (the daemon validates them).
func quantity(s, def string) resource.Quantity {
	q, err := resource.ParseQuantity(orDefault(s, def))
	if err != nil {
		return resource.Quantity{}
	}
	return q
}

func (r *run) limits(ctx context.Context) {
	if !r.nsExists {
		r.skip(CheckLimits, "namespace missing")
		return
	}
	r.running(CheckLimits)
	lr, err := r.readLimitRanges(ctx)
	if err != nil {
		r.warn(CheckLimits, "LimitRanges are not readable: "+Message(err), "")
		return
	}
	c := &r.t.Cluster
	setting := c.InstanceMemoryLimit()
	var want resource.Quantity
	if setting != "" {
		if want, err = resource.ParseQuantity(setting); err != nil {
			r.warn(CheckLimits, "memory limit "+c.MemoryLimit+" is not a quantity", "")
			return
		}
	}
	if lr.defaultMemory != nil {
		r.caps.LimitRangeMemoryLimit = lr.defaultMemory.String()
	}
	var note, fix string
	if ceiling := lr.maxMemory; ceiling != nil {
		r.caps.LimitRangeMemoryMax = ceiling.String()
		req := quantity(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)
		switch {
		case ceiling.Cmp(req) < 0:
			// No limit fits: the request is above what the LimitRange allows.
			note = fmt.Sprintf("LimitRange maximum memory %s is below the %s memory request", ceiling.String(), req.String())
			fix = "set the memory request to at most " + ceiling.String() + ", or raise the LimitRange"
			r.limit("memory request above the LimitRange maximum")
		case setting != "" && ceiling.Cmp(want) < 0:
			note = fmt.Sprintf("LimitRange maximum memory %s → memory limit clamped from %s", ceiling.String(), want.String())
			fix = "set memory_limit to at most " + ceiling.String()
		}
	}
	switch {
	case note != "":
		r.warn(CheckLimits, note, fix)
	case lr.count == 0:
		r.pass(CheckLimits, "no LimitRange")
	case setting == "" && lr.defaultMemory != nil:
		r.pass(CheckLimits, "memory_limit off → the LimitRange's default memory limit "+lr.defaultMemory.String()+" applies")
	default:
		r.pass(CheckLimits, "LimitRanges leave fleet's limits alone")
	}
}

// instanceNeeds is what one instance takes from a ResourceQuota, by quota
// resource name: its requests, its disk, and the limits it sets, which are
// the LimitRange's defaults where fleet sets none (a limit neither sets is
// left out: limitsUnset names it).
func (r *run) instanceNeeds() map[corev1.ResourceName]resource.Quantity {
	c := &r.t.Cluster
	cpu := quantity(c.Resources.CPURequest, fleet.DefaultKubeCPURequest)
	mem := quantity(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)
	disk := quantity(c.DiskSize, fleet.DefaultKubeDiskSize)
	needs := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourcePods:                   resource.MustParse("1"),
		corev1.ResourceCPU:                    cpu,
		corev1.ResourceRequestsCPU:            cpu,
		corev1.ResourceMemory:                 mem,
		corev1.ResourceRequestsMemory:         mem,
		corev1.ResourceRequestsStorage:        disk,
		corev1.ResourcePersistentVolumeClaims: resource.MustParse("1"),
		"count/statefulsets.apps":             resource.MustParse("1"),
		"count/persistentvolumeclaims":        resource.MustParse("1"),
		corev1.ResourceSecrets:                resource.MustParse("2"),
		"count/secrets":                       resource.MustParse("2"),
	}
	lr := r.lr
	if lr == nil {
		lr = &limitRanges{}
	}
	memLimit, cpuLimit := instanceLimits(c, lr)
	for name, limit := range map[corev1.ResourceName][2]*resource.Quantity{
		corev1.ResourceLimitsMemory: {memLimit, lr.defaultMemory},
		corev1.ResourceLimitsCPU:    {cpuLimit, lr.defaultCPU},
	} {
		switch {
		case limit[0] != nil:
			needs[name] = *limit[0]
		case limit[1] != nil:
			needs[name] = *limit[1]
		}
	}
	if r.rootClass != nil {
		needs[corev1.ResourceName(r.rootClass.Name+".storageclass.storage.k8s.io/requests.storage")] = disk
		needs[corev1.ResourceName(r.rootClass.Name+".storageclass.storage.k8s.io/persistentvolumeclaims")] = resource.MustParse("1")
	}
	return needs
}

// limitsUnset are the quota resources on limits that refuse an instance
// pod outright ("must specify limits.cpu"): the limits neither the
// connection nor a LimitRange's default sets, with the setting that sets
// each. GKE Autopilot sets them all (limits equal requests).
func (r *run) limitsUnset() map[corev1.ResourceName]string {
	if r.caps.Autopilot {
		return nil
	}
	needs := r.instanceNeeds()
	out := map[corev1.ResourceName]string{}
	if _, ok := needs[corev1.ResourceLimitsCPU]; !ok {
		out[corev1.ResourceLimitsCPU] = "resources.cpu_limit"
	}
	if _, ok := needs[corev1.ResourceLimitsMemory]; !ok {
		out[corev1.ResourceLimitsMemory] = "memory_limit (it is 0, off)"
	}
	return out
}

func (r *run) quota(ctx context.Context) {
	if !r.nsExists {
		r.skip(CheckQuota, "namespace missing")
		return
	}
	r.running(CheckQuota)
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.CoreV1().ResourceQuotas(r.t.Cluster.Namespace).List(cctx, metav1.ListOptions{})
	if err != nil {
		r.warn(CheckQuota, "ResourceQuotas are not readable: "+Message(err), "")
		return
	}
	if len(list.Items) == 0 {
		r.pass(CheckQuota, "no ResourceQuota")
		return
	}
	needs, unset := r.instanceNeeds(), r.limitsUnset()
	room := math.MaxInt
	var full, refused, set []string
	for _, q := range list.Items {
		for _, name := range sortedKeys(resourceNames(q.Status.Hard)) {
			hard := q.Status.Hard[corev1.ResourceName(name)]
			used := q.Status.Used[corev1.ResourceName(name)]
			r.caps.Quota = append(r.caps.Quota, &fleetgrpc.KubeQuotaHeadroom{Resource: name, Used: used.String(), Hard: hard.String()})
			if setting, ok := unset[corev1.ResourceName(name)]; ok {
				if !slices.Contains(refused, name) {
					refused, set = append(refused, name), append(set, setting)
				}
				continue
			}
			need, ok := needs[corev1.ResourceName(name)]
			if !ok || need.IsZero() {
				continue
			}
			left := hard.DeepCopy()
			left.Sub(used)
			fits := 0
			if left.Sign() > 0 {
				fits = int(math.Floor(float64(left.MilliValue()) / float64(need.MilliValue())))
			}
			if fits < room {
				room = fits
			}
			if fits == 0 {
				full = append(full, fmt.Sprintf("%s %s used of %s", name, used.String(), hard.String()))
			}
		}
	}
	sort.Slice(r.caps.Quota, func(i, j int) bool { return r.caps.Quota[i].Resource < r.caps.Quota[j].Resource })
	switch {
	case len(refused) > 0:
		r.warn(CheckQuota, "a ResourceQuota on "+strings.Join(refused, " and ")+" refuses instance pods, which set no such limit", "set the connection's "+strings.Join(set, " and "))
		r.limit("quota needs limits")
	case len(full) > 0:
		r.warn(CheckQuota, "no room for another instance: "+strings.Join(full, ", "), "raise the namespace's ResourceQuota, or lower the instance's requests and limits")
		r.limit("quota full")
	case room == math.MaxInt:
		r.pass(CheckQuota, "ResourceQuota sets nothing an instance takes")
	default:
		r.pass(CheckQuota, fmt.Sprintf("room for %d more instance%s", room, plural(room)))
	}
}

func resourceNames(l corev1.ResourceList) map[string]bool {
	out := make(map[string]bool, len(l))
	for name := range l {
		out[string(name)] = true
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
