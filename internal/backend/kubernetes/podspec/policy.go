package podspec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// The privilege policy and the debug-capabilities policy.
//
// Privilege: a run's --privileged, devices, host namespaces and docker
// engine socket need privileged pods allowed on the connection
// (fleet.KubeCluster.AllowsPrivileged: auto on a local cluster, the
// connection's "Allow privileged pods" toggle elsewhere, because a
// privileged pod makes the node the isolation boundary). When the cluster
// offers a sysbox runtime class, --privileged and the docker engine's
// sidecar run under it instead: sysbox gives a container what
// docker-in-docker needs without privilege on the node. Nothing is granted
// that the run did not ask for, except the privileged flag --device needs to
// reach its device, and the privileged sidecar a docker.sock bind gets (its
// engine is the pod's own; see docker.go).
//
// Debug grants: SYS_PTRACE and unconfined seccomp or AppArmor (the Go, Rust
// and C++ features add them for debuggers) are requested like any other
// grant. When admission denies a render, Denied decides: if the render
// carried debug grants, the create re-renders without them
// (Input.DropDebugGrants) and warns, unless the connection's
// debug_capabilities is require; any other denial fails the create, since
// functional grants are never dropped.

// sysboxUsernsAnnotation is the user-namespace mode a sysbox pod asks
// CRI-O for (sysbox runs on CRI-O nodes).
const sysboxUsernsAnnotation = "io.kubernetes.cri-o.userns-mode"

// needsPrivilegeAllowed records that what the run asked for (as written)
// needs privileged pods allowed on the connection, though not a privileged
// dev container: host namespaces.
func (t *translation) needsPrivilegeAllowed(what string) {
	t.needsAllowed = append(t.needsAllowed, what)
}

// finishPrivilege applies the privilege policy, and the connection's
// runtime class.
func (t *translation) finishPrivilege() {
	if rc := t.cluster.RuntimeClass; rc != "" && t.spec.RuntimeClassName == "" {
		t.spec.RuntimeClassName = rc
	}
	privileged := t.privileged
	dood := len(t.dood) > 0
	if (privileged || dood) && len(t.needsPrivileged) == 0 && len(t.needsAllowed) == 0 {
		if class := t.sysboxClass(); class != "" {
			t.sysbox = true
			privileged = false
			t.spec.RuntimeClassName = class
			if t.spec.Annotations == nil {
				t.spec.Annotations = map[string]string{}
			}
			t.spec.Annotations[sysboxUsernsAnnotation] = "auto:size=65536"
			what := "--privileged"
			if !t.privileged {
				what = "the docker engine's sidecar"
			} else if dood {
				what = "--privileged and the docker engine's sidecar"
			}
			t.warn("%s: the pod runs under the %s runtime class instead of privileged, which gives docker-in-docker what it needs without privilege on the node", what, class)
		}
	}
	allowed := t.cluster.AllowsPrivileged(t.in.Facts.Local)
	if dood && !t.sysbox {
		if allowed {
			t.doodPrivileged = true
		} else {
			// After the dev container's own refusal, which leads.
			defer t.refuseDooD()
		}
	}
	var asks []string
	if privileged {
		asks = append(asks, "--privileged")
	}
	asks = append(asks, t.needsPrivileged...)
	asks = append(asks, t.needsAllowed...)
	if len(asks) == 0 {
		return
	}
	if !allowed {
		t.refusePrivileged(asks, privileged)
		return
	}
	if privileged || len(t.needsPrivileged) > 0 {
		t.spec.Security.Privileged = true
		t.spec.PID1.Privileged = true
	}
	if !privileged && len(t.needsPrivileged) > 0 {
		t.warn("%s: the pod runs privileged to reach the device (the node is the isolation boundary)", joinNames(t.needsPrivileged))
	}
}

func (t *translation) refusePrivileged(asks []string, privileged bool) {
	where := "Allow privileged pods is off for it"
	if t.cluster.AllowPrivileged == nil {
		where = "it is not a local cluster, where they are allowed by default"
	}
	hint := ""
	if privileged {
		hint = ", or use a cluster with a sysbox runtime class for --privileged"
	}
	verb := "needs"
	if len(asks) > 1 {
		verb = "need"
	}
	t.refuse(&Refusal{Code: CodePrivileged, Reason: fmt.Sprintf(
		"%s %s privileged pods, which %s does not allow (%s). Turn on Allow privileged pods for %s in Settings → Kubernetes (the node becomes the isolation boundary: a privileged pod can reach every pod on its node)%s",
		joinNames(asks), verb, t.clusterName(), where, t.clusterName(), hint)})
}

// refuseDooD refuses a run that reaches the docker engine on a connection
// that allows neither privileged pods nor offers a sysbox runtime class.
func (t *translation) refuseDooD() {
	where := "Allow privileged pods is off for it"
	if t.cluster.AllowPrivileged == nil {
		where = "it is not a local cluster, where they are allowed by default"
	}
	t.refuse(&Refusal{Code: CodeDooDNeedsPrivileged, Reason: fmt.Sprintf(
		"%s binds the docker engine's socket; on a cluster that engine is the pod's own, a sidecar that needs privileged pods, which %s does not allow (%s). Turn on Allow privileged pods for %s in Settings → Kubernetes (the node becomes the isolation boundary: a privileged pod can reach every pod on its node), or use a cluster with a sysbox runtime class",
		joinNames(t.dood), t.clusterName(), where, t.clusterName())})
}

// sysboxClass is the sysbox runtime class --privileged can run under: the
// connection's runtime class or --runtime when it is one, else the
// cluster's own sysbox class unless the connection pins another runtime.
func (t *translation) sysboxClass() string {
	if rc := t.spec.RuntimeClassName; rc != "" {
		if strings.Contains(rc, "sysbox") {
			return rc
		}
		return ""
	}
	var classes []string
	for _, c := range t.in.Facts.RuntimeClasses {
		if strings.Contains(c, "sysbox") {
			classes = append(classes, c)
		}
	}
	sort.Strings(classes)
	if len(classes) == 0 {
		return ""
	}
	return classes[0]
}

// DebugDroppedWarning starts the warning of a pod rendered without its
// debug grants.
const DebugDroppedWarning = "debug capabilities dropped by admission"

// finishDebug requests the run's debug grants, or leaves them out of a
// re-render.
func (t *translation) finishDebug() {
	d := t.debug
	if len(d.written) == 0 {
		return
	}
	t.spec.DebugGrants = append([]string(nil), d.written...)
	if t.in.DropDebugGrants {
		t.spec.DebugDropped = true
		t.warn("%s: %s (%s denies them); debuggers such as dlv and gdb cannot attach. Set debug_capabilities to require on %s to fail the create instead",
			DebugDroppedWarning, strings.Join(d.written, ", "), t.namespaceName(), t.clusterName())
		return
	}
	s := &t.spec.Security
	if d.ptrace && !containsString(s.CapAdd, "ALL") {
		s.CapAdd = appendUnique(s.CapAdd, "SYS_PTRACE")
	}
	if d.seccomp {
		s.Seccomp = SeccompUnconfined
	}
	if d.apparmor {
		s.AppArmor = AppArmorUnconfined
	}
}

func (t *translation) namespaceName() string {
	if ns := t.cluster.Namespace; ns != "" {
		return "namespace " + ns
	}
	return "the namespace"
}

// Denied decides what a create does after admission refused a render of
// pod (a server-side dry-run, or a FailedCreate event), denial being the
// refusal's message. rerender asks for a render without the debug grants
// (Translate with Input.DropDebugGrants): the render carried debug grants,
// had not dropped them yet, and the connection's debug_capabilities is auto.
// Otherwise err is the create's error: the denial, with what to change.
func Denied(cluster *fleet.KubeCluster, pod *PodSpec, denial string) (rerender bool, err error) {
	if cluster == nil {
		cluster = &fleet.KubeCluster{}
	}
	name := cluster.GetDisplayName()
	if name == "" {
		name = "the cluster"
	}
	if len(pod.DebugGrants) > 0 && !pod.DebugDropped {
		if !cluster.RequiresDebugCapabilities() {
			return true, nil
		}
		return false, &Refusal{Code: CodeAdmissionDenied + ":debug_capabilities", Reason: fmt.Sprintf(
			"%s (the devcontainer asks for %s, and debug_capabilities is require on %s; set it to auto to drop them with a warning instead)",
			denial, strings.Join(pod.DebugGrants, ", "), name)}
	}
	return false, &Refusal{Code: CodeAdmissionDenied, Reason: fmt.Sprintf("%s: %s", name, denial)}
}
