//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// permission is what fleet needs to do to one resource in its namespace.
type permission struct {
	group    string
	resource string // "pods", or "pods/exec" for a subresource
	verbs    []string
}

var (
	lifecycle = []string{"create", "get", "list", "watch", "patch", "delete"}
	objects   = []string{"create", "get", "list", "patch", "delete"}
	reads     = []string{"get", "list"}
)

// requiredPermissions is the namespace-scoped RBAC fleet needs (the security
// model's table). Without any of them a connection is FORBIDDEN.
var requiredPermissions = []permission{
	{"apps", "statefulsets", lifecycle},
	{"apps", "deployments", lifecycle},
	{"batch", "jobs", lifecycle},
	{"", "pods", []string{"create", "get", "list", "watch", "delete"}},
	{"", "pods/exec", []string{"create", "get"}},
	{"", "pods/portforward", []string{"create", "get"}},
	{"", "pods/log", []string{"get"}},
	{"", "events", reads},
	{"apps", "replicasets", reads},
	{"", "limitranges", reads},
	{"", "resourcequotas", reads},
	{"", "persistentvolumeclaims", objects},
	{"", "secrets", objects},
	{"", "configmaps", objects},
	{"", "services", objects},
	{"coordination.k8s.io", "leases", []string{"create", "get", "update"}},
	{"networking.k8s.io", "networkpolicies", objects},
}

// optionalPermissions improve what fleet can do or see; without them it
// degrades quietly (clone falls back to a copy, the exposure scan is
// skipped).
var optionalPermissions = []permission{
	{"snapshot.storage.k8s.io", "volumesnapshots", []string{"create", "get", "list", "delete"}},
	{"rbac.authorization.k8s.io", "rolebindings", []string{"list"}},
	{"metrics.k8s.io", "pods", []string{"list"}},
}

// pdbPermission backs protect_from_drain, which makes it required. The
// budget is applied server-side (patch) and pruned by label (list), like the
// other objects.
var pdbPermission = permission{"policy", "poddisruptionbudgets", objects}

func (r *run) rbac(ctx context.Context) {
	if !r.nsExists {
		r.skip(CheckRBAC, "namespace missing")
		return
	}
	r.running(CheckRBAC)
	required := append([]permission(nil), requiredPermissions...)
	optional := append([]permission(nil), optionalPermissions...)
	if r.t.Cluster.ProtectFromDrain {
		required = append(required, pdbPermission)
	} else {
		optional = append(optional, pdbPermission)
	}
	missing, err := r.missingPermissions(ctx, required)
	if err != nil {
		state, msg := Classify(err)
		if state == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN {
			r.warn(CheckRBAC, "cannot review its own permissions: "+msg, "")
			return
		}
		r.fail(CheckRBAC, msg, "")
		r.failState(state, msg)
		return
	}
	optMissing, _ := r.missingPermissions(ctx, optional)
	if len(missing) > 0 {
		msg := "missing " + strings.Join(missing, ", ")
		r.fail(CheckRBAC, msg, fmt.Sprintf("grant these in namespace %s to %s (the fleet Role in the docs)", r.t.Cluster.Namespace, orUnknown(r.user)))
		r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN, "RBAC: "+msg)
		return
	}
	detail := "sts deploy jobs pods pods/exec pods/portforward pvc secrets svc netpol leases"
	if len(optMissing) > 0 {
		r.warn(CheckRBAC, detail+" · optional missing: "+strings.Join(optMissing, ", "), "")
		return
	}
	r.pass(CheckRBAC, detail)
}

func orUnknown(s string) string {
	if s == "" {
		return "fleet's identity"
	}
	return s
}

// missingPermissions lists, as "verb resource", what perms grants that the
// connection's identity lacks in its namespace. It reads the rules review
// first and asks access reviews only for what the rules do not grant when the
// review is incomplete (a webhook or cloud IAM authorizer).
func (r *run) missingPermissions(ctx context.Context, perms []permission) ([]string, error) {
	ns := r.t.Cluster.Namespace
	cctx, cancel := call(ctx)
	defer cancel()
	review, err := r.t.Client.AuthorizationV1().SelfSubjectRulesReviews().Create(cctx, &authorizationv1.SelfSubjectRulesReview{
		Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: ns},
	}, metav1.CreateOptions{})
	var rules []authorizationv1.ResourceRule
	incomplete := true
	if err == nil {
		rules, incomplete = review.Status.ResourceRules, review.Status.Incomplete
	}
	var missing []string
	for _, p := range perms {
		for _, verb := range p.verbs {
			if rulesAllow(rules, p.group, p.resource, verb) {
				continue
			}
			if incomplete {
				ok, err := r.accessReview(ctx, p.group, p.resource, verb)
				if err != nil {
					return nil, err
				}
				if ok {
					continue
				}
			}
			missing = append(missing, verb+" "+p.resource)
		}
	}
	return missing, nil
}

func (r *run) accessReview(ctx context.Context, group, resource, verb string) (bool, error) {
	res, sub, _ := strings.Cut(resource, "/")
	cctx, cancel := call(ctx)
	defer cancel()
	review, err := r.t.Client.AuthorizationV1().SelfSubjectAccessReviews().Create(cctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: r.t.Cluster.Namespace, Verb: verb, Group: group, Resource: res, Subresource: sub,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

// rulesAllow reports whether rules grant verb on group/resource, as the RBAC
// authorizer matches them: "*" wildcards, "*/<subresource>", and no rule
// restricted to resource names (those never grant list or create).
func rulesAllow(rules []authorizationv1.ResourceRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if matches(rule.Verbs, verb) && matches(rule.APIGroups, group) && resourceMatches(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func matches(have []string, want string) bool {
	for _, h := range have {
		if h == "*" || h == want {
			return true
		}
	}
	return false
}

func resourceMatches(have []string, resource string) bool {
	_, sub, hasSub := strings.Cut(resource, "/")
	for _, h := range have {
		if h == "*" || h == resource || (hasSub && h == "*/"+sub) {
			return true
		}
	}
	return false
}
