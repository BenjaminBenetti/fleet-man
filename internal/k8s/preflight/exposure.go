//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// exposure.go scans the namespace's RoleBindings for other subjects that can
// reach what fleet keeps there: whoever can exec into, create or patch the
// pods is inside the instances, and whoever can create pods, claims or
// snapshots can mount a fleet's share, which holds its agent logins. These
// are warnings, never a degraded state: a namespace shared with a team is a
// choice, and the warning makes it a visible one.

// exposureVerb is one grant that reaches into fleet's objects.
type exposureVerb struct {
	group, resource, verb string
	label                 string
}

var (
	insideVerbs = []exposureVerb{
		{"", "pods/exec", "create", "pods/exec"},
		{"", "pods", "create", "pod create"},
		{"apps", "statefulsets", "patch", "statefulset patch"},
	}
	loginVerbs = []exposureVerb{
		{"", "persistentvolumeclaims", "create", "PVC create"},
		{"snapshot.storage.k8s.io", "volumesnapshots", "create", "snapshot create"},
	}
)

// maxSubjects caps the subjects one warning names.
const maxSubjects = 5

func (r *run) exposure(ctx context.Context) {
	if !r.nsExists {
		r.skip(CheckExposure, "namespace missing")
		return
	}
	r.running(CheckExposure)
	ns := r.t.Cluster.Namespace
	cctx, cancel := call(ctx)
	defer cancel()
	bindings, err := r.t.Client.RbacV1().RoleBindings(ns).List(cctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			r.skip(CheckExposure, "RoleBindings are not readable (optional): other subjects with access are unknown")
			return
		}
		r.warn(CheckExposure, "listing RoleBindings: "+Message(err), "")
		return
	}
	inside := map[string][]string{} // subject -> grants
	logins := map[string][]string{}
	unreadable := 0
	for _, b := range bindings.Items {
		rules, ok := r.roleRules(ctx, ns, b.RoleRef)
		if !ok {
			unreadable++
			continue
		}
		for _, s := range b.Subjects {
			if r.isSelf(s) {
				continue
			}
			name := subjectName(s, ns)
			for _, v := range insideVerbs {
				if rulesAllow(rules, v.group, v.resource, v.verb) {
					inside[name] = appendOnce(inside[name], v.label)
				}
			}
			if rulesAllow(rules, "", "pods", "create") {
				logins[name] = appendOnce(logins[name], "pod create")
			}
			for _, v := range loginVerbs {
				if rulesAllow(rules, v.group, v.resource, v.verb) {
					logins[name] = appendOnce(logins[name], v.label)
				}
			}
		}
	}
	var lines []string
	if len(inside) > 0 {
		line := fmt.Sprintf("%d other subject%s can act inside your instances: %s", len(inside), plural(len(inside)), listSubjects(inside))
		r.warning(line)
		lines = append(lines, line)
	}
	if len(logins) > 0 {
		line := fmt.Sprintf("%d other subject%s can read your agent logins: %s", len(logins), plural(len(logins)), listSubjects(logins))
		r.warning(line)
		lines = append(lines, line)
	}
	note := ""
	if unreadable > 0 {
		note = fmt.Sprintf(" (%d binding%s to roles fleet cannot read)", unreadable, plural(unreadable))
	}
	if len(lines) == 0 {
		r.pass(CheckExposure, "no other subject holds pods/exec, pod create, statefulset patch, PVC or snapshot create in "+ns+note)
		return
	}
	r.warn(CheckExposure, strings.Join(lines, "; ")+note, "a namespace of your own, so no one else can reach your instances")
}

// roleRules are the rules a binding grants, as resource rules; ok is false
// when the role cannot be read.
func (r *run) roleRules(ctx context.Context, ns string, ref rbacv1.RoleRef) ([]authorizationv1.ResourceRule, bool) {
	cctx, cancel := call(ctx)
	defer cancel()
	var policy []rbacv1.PolicyRule
	switch ref.Kind {
	case "Role":
		role, err := r.t.Client.RbacV1().Roles(ns).Get(cctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, false
		}
		policy = role.Rules
	case "ClusterRole":
		role, err := r.t.Client.RbacV1().ClusterRoles().Get(cctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, false
		}
		policy = role.Rules
	default:
		return nil, false
	}
	rules := make([]authorizationv1.ResourceRule, 0, len(policy))
	for _, p := range policy {
		rules = append(rules, authorizationv1.ResourceRule{Verbs: p.Verbs, APIGroups: p.APIGroups, Resources: p.Resources, ResourceNames: p.ResourceNames})
	}
	return rules, true
}

// isSelf: the subject is the identity fleet acts as. Groups it belongs to
// are not excluded: their other members are exactly who the warning is for.
func (r *run) isSelf(s rbacv1.Subject) bool {
	if r.user == "" {
		return false
	}
	switch s.Kind {
	case rbacv1.UserKind:
		return s.Name == r.user
	case rbacv1.ServiceAccountKind:
		return "system:serviceaccount:"+s.Namespace+":"+s.Name == r.user
	}
	return false
}

func subjectName(s rbacv1.Subject, ns string) string {
	if s.Kind == rbacv1.ServiceAccountKind {
		sns := s.Namespace
		if sns == "" {
			sns = ns
		}
		return "ServiceAccount " + sns + "/" + s.Name
	}
	return s.Kind + " " + s.Name
}

func listSubjects(m map[string][]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for i, k := range keys {
		if i == maxSubjects {
			parts = append(parts, fmt.Sprintf("and %d more", len(keys)-maxSubjects))
			break
		}
		parts = append(parts, k+" ("+strings.Join(m[k], ", ")+")")
	}
	return strings.Join(parts, ", ")
}

func appendOnce(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}
