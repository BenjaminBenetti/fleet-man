//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// limits.go reads what the namespace's LimitRanges and ResourceQuotas do to
// an instance: a LimitRange that would inject a memory limit below
// memory_limit (fleet then sets its own explicitly), a maximum fleet's limit
// breaks, and the quota headroom left for one more instance.

func (r *run) limits(ctx context.Context) {
	if !r.nsExists {
		r.skip(CheckLimits, "namespace missing")
		return
	}
	r.running(CheckLimits)
	cctx, cancel := call(ctx)
	defer cancel()
	list, err := r.t.Client.CoreV1().LimitRanges(r.t.Cluster.Namespace).List(cctx, metav1.ListOptions{})
	if err != nil {
		r.warn(CheckLimits, "LimitRanges are not readable: "+Message(err), "")
		return
	}
	want, err := resource.ParseQuantity(orDefault(r.t.Cluster.MemoryLimit, fleet.DefaultKubeMemoryLimit))
	if err != nil {
		r.warn(CheckLimits, "memory limit "+r.t.Cluster.MemoryLimit+" is not a quantity", "")
		return
	}
	var injected, max *resource.Quantity
	for _, lr := range list.Items {
		for _, item := range lr.Spec.Limits {
			if item.Type != corev1.LimitTypeContainer && item.Type != corev1.LimitTypePod {
				continue
			}
			if q, ok := item.Default[corev1.ResourceMemory]; ok && item.Type == corev1.LimitTypeContainer && (injected == nil || q.Cmp(*injected) < 0) {
				q := q
				injected = &q
			}
			if q, ok := item.Max[corev1.ResourceMemory]; ok && (max == nil || q.Cmp(*max) < 0) {
				q := q
				max = &q
			}
		}
	}
	if injected != nil {
		r.caps.LimitRangeMemoryLimit = injected.String()
	}
	var notes []string
	if injected != nil && injected.Cmp(want) < 0 {
		notes = append(notes, fmt.Sprintf("LimitRange default memory %s → explicit %s limit", injected.String(), want.String()))
	}
	if max != nil && max.Cmp(want) < 0 {
		notes = append(notes, fmt.Sprintf("LimitRange maximum memory %s is below the %s limit fleet sets", max.String(), want.String()))
		r.limit("memory limit above the LimitRange maximum")
	}
	switch {
	case len(notes) > 0:
		fix := ""
		if max != nil && max.Cmp(want) < 0 {
			fix = "set the memory limit to at most " + max.String()
		}
		r.warn(CheckLimits, strings.Join(notes, "; "), fix)
	case len(list.Items) == 0:
		r.pass(CheckLimits, "no LimitRange")
	default:
		r.pass(CheckLimits, "LimitRanges leave fleet's limits alone")
	}
}

// instanceNeeds is what one instance takes from a ResourceQuota, by quota
// resource name.
func (r *run) instanceNeeds() map[corev1.ResourceName]resource.Quantity {
	c := r.t.Cluster
	parse := func(s, def string) resource.Quantity {
		q, err := resource.ParseQuantity(orDefault(s, def))
		if err != nil {
			return resource.Quantity{}
		}
		return q
	}
	cpu := parse(c.Resources.CPURequest, fleet.DefaultKubeCPURequest)
	mem := parse(c.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest)
	limit := parse(c.MemoryLimit, fleet.DefaultKubeMemoryLimit)
	disk := parse(c.DiskSize, fleet.DefaultKubeDiskSize)
	needs := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourcePods:                   resource.MustParse("1"),
		corev1.ResourceCPU:                    cpu,
		corev1.ResourceRequestsCPU:            cpu,
		corev1.ResourceMemory:                 mem,
		corev1.ResourceRequestsMemory:         mem,
		corev1.ResourceLimitsMemory:           limit,
		corev1.ResourceRequestsStorage:        disk,
		corev1.ResourcePersistentVolumeClaims: resource.MustParse("1"),
		"count/statefulsets.apps":             resource.MustParse("1"),
		"count/persistentvolumeclaims":        resource.MustParse("1"),
		corev1.ResourceSecrets:                resource.MustParse("2"),
		"count/secrets":                       resource.MustParse("2"),
	}
	if r.rootClass != nil {
		needs[corev1.ResourceName(r.rootClass.Name+".storageclass.storage.k8s.io/requests.storage")] = disk
		needs[corev1.ResourceName(r.rootClass.Name+".storageclass.storage.k8s.io/persistentvolumeclaims")] = resource.MustParse("1")
	}
	return needs
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
	needs := r.instanceNeeds()
	room := math.MaxInt
	var full []string
	for _, q := range list.Items {
		for _, name := range sortedKeys(resourceNames(q.Status.Hard)) {
			hard := q.Status.Hard[corev1.ResourceName(name)]
			used := q.Status.Used[corev1.ResourceName(name)]
			r.caps.Quota = append(r.caps.Quota, &fleetgrpc.KubeQuotaHeadroom{Resource: name, Used: used.String(), Hard: hard.String()})
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
	case len(full) > 0:
		r.warn(CheckQuota, "no room for another instance: "+strings.Join(full, ", "), "raise the namespace's ResourceQuota, or lower the instance's requests")
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
