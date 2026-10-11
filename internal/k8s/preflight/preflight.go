//go:build !nok8s

package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	authenticationv1 "k8s.io/api/authentication/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// Check names, in the order Full runs them. Clients show them with the
// underscores as spaces.
const (
	CheckKubeconfig            = "kubeconfig"
	CheckCredentials           = "credentials"
	CheckReachable             = "reachable"
	CheckIdentity              = "identity"
	CheckNamespace             = "namespace"
	CheckRBAC                  = "rbac"
	CheckOwner                 = "owner"
	CheckNodes                 = "nodes"
	CheckAdmissionUnprivileged = "admission_unprivileged"
	CheckAdmissionDebug        = "admission_debug_caps"
	CheckAdmissionPrivileged   = "admission_privileged"
	CheckBuildkit              = "buildkit"
	CheckRootStorage           = "root_storage"
	// CheckSharedStorage (share.go) is how fleets' shares are held; the
	// deep test's CheckSharePods probes one RWO volume from two pods.
	CheckSnapshots      = "snapshots"
	CheckLimits         = "limits"
	CheckQuota          = "quota"
	CheckMetrics        = "metrics"
	CheckRuntimeClasses = "runtime_classes"
	CheckExposure       = "exposure"
	CheckImageDelivery  = "image_delivery"
	// CheckImagePull is the deep test's pull through the gateway.
	CheckImagePull = "image_pull"
	// CheckNetworkPolicy and CheckMetadata (network.go) are the deep test's
	// look at the pods' network.
)

// Timeouts: one API call, and a whole full run.
const (
	callTimeout = 15 * time.Second
	// CredentialTimeout bounds one run of the credential plugin.
	CredentialTimeout = 30 * time.Second
	// FullTimeout bounds a whole full preflight: its checks (a few minutes
	// at most), and the multi-node storage test of a class it has no
	// result for.
	FullTimeout = 3*time.Minute + StorageTestTimeout
)

// LeaseClaimer is the connection's owner Lease (owner.Lease).
type LeaseClaimer interface {
	Ensure(ctx context.Context) (owner.LeaseStatus, error)
}

// Target is one connection as the preflight sees it.
type Target struct {
	// Cluster is the record; empty settings take their defaults.
	Cluster fleet.KubeCluster
	// Client talks to the API server.
	Client kubernetes.Interface
	// Raw GETs an absolute API path, for reads outside the typed client
	// (VolumeSnapshotClasses). nil skips them.
	Raw func(ctx context.Context, path string) ([]byte, error)
	// Credential reads the connection's credential expiry, running an exec
	// plugin (kubeexec.Connection.Credential). nil skips the check.
	Credential func(ctx context.Context) (kubeexec.Credential, error)
	// Auth and Plugin describe how the context authenticates; Source is the
	// kubeconfig file the context comes from.
	Auth   kubeexec.AuthKind
	Plugin string
	Source string
	// Lease is the connection's owner Lease; nil for an unsaved connection,
	// which claims nothing.
	Lease LeaseClaimer
	// Owner is this daemon's owner label value, put on the canaries.
	Owner string
	// Delivery judges image delivery (the image_delivery check, and the
	// deep test's pull); nil skips both.
	Delivery Delivery
	// Deep also runs the deep test's throwaway-pod probes after the full
	// run's checks.
	Deep bool
	// MultiPodRWO is what the last deep test found of two pods on one node
	// mounting one RWO volume (share_pods), which a full run without one
	// keeps: nil when none has told (then StorageCache's, if it has one).
	MultiPodRWO *bool
	// StorageCache is the connection's storage test cache
	// (OpenStorageCache): the multi-node tests' results, which a run reads
	// and adds to. nil runs no test (a draft connection): every class is
	// then as untested.
	StorageCache *StorageCache
	// TestStorage are StorageClasses to run the multi-node test on now,
	// cached result or not (TestKubeClusterRequest.storage_classes): the
	// consent a class with a large minimum size needs before Auto tests it.
	TestStorage []string
	// Now replaces time.Now (tests).
	Now func() time.Time
}

func (t *Target) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Result is what a preflight found.
type Result struct {
	State   fleetgrpc.KubeClusterState
	Message string
	// Capabilities and Warnings are those of a full run; a shallow run leaves
	// them nil (the last full run's still stand).
	Capabilities *fleetgrpc.KubeCapabilities
	Warnings     []string
	Checks       []*fleetgrpc.KubeCheck
	// ClusterUID is the kube-system namespace UID, when it could be read.
	ClusterUID string
	// Credential is what the credentials check read (its zero value when it
	// did not run); CredentialErr why it failed.
	Credential    kubeexec.Credential
	CredentialErr error
	// Lease is the owner Lease's state, when it was claimed.
	Lease *owner.LeaseStatus
	// Local is the local cluster the nodes belong to, and Platform their
	// one os/arch ("" when unknown or mixed), when the nodes were readable.
	Local    localcluster.Cluster
	Platform string
	// CheckedAt is when the run finished, DeepCheckedAt when its deep test
	// did (zero without one).
	CheckedAt     time.Time
	DeepCheckedAt time.Time
	// MultiPodRWO is what this run's deep test found of two pods on one
	// node mounting one RWO volume (nil when it did not tell), for the next
	// run's Target (and kept in its StorageCache).
	MultiPodRWO *bool
}

// Failed reports a failure state: anything but READY, LIMITED and
// OWNER_CONFLICT (a connection that works but is not this daemon's to act
// on).
func Failed(s fleetgrpc.KubeClusterState) bool {
	switch s {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:
		return false
	}
	return true
}

// Usable reports a state a create may target: READY or LIMITED.
func Usable(s fleetgrpc.KubeClusterState) bool {
	return s == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY || s == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED
}

// LoadFailure is the result for a connection whose kubeconfig could not be
// loaded (kubeexec.Load failed): nothing else can run.
func LoadFailure(err error, now time.Time) Result {
	state, msg := Classify(err)
	return Result{
		State:     state,
		Message:   msg,
		Checks:    []*fleetgrpc.KubeCheck{{Name: CheckKubeconfig, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: msg, Fix: loadFix(state)}},
		CheckedAt: now,
	}
}

func loadFix(state fleetgrpc.KubeClusterState) string {
	switch state {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING:
		return "the context or kubeconfig was renamed or removed: re-point the connection, or remove it"
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING:
		return "install the plugin on the daemon host, or add its directory to the connection's extra PATH"
	}
	return ""
}

// run is one preflight in progress.
type run struct {
	t      *Target
	report func(*fleetgrpc.KubeCheck)
	res    Result
	caps   *fleetgrpc.KubeCapabilities

	// failure is the first failure state found, with its message.
	failure    fleetgrpc.KubeClusterState
	failureMsg string
	// limited are the degradations that make a working connection LIMITED.
	limited []string
	// ownerConflict is the OWNER_CONFLICT message, when another incarnation
	// holds the Lease.
	ownerConflict string

	// what the checks learned, for the ones after them
	user     string
	nsExists bool
	// multiNode: the nodes were read and are several; nodesKnown: they
	// were read. providers are their provider ids, by name.
	multiNode  bool
	nodesKnown bool
	providers  map[string]string
	csiLimits  map[string]map[string]int32 // node -> CSI driver -> attach limit
	rootClass  *fleetgrpc.KubeStorageClass
	// classObjs are the StorageClasses as listed, by name;
	// classesUnreadable says the listing was forbidden.
	classObjs         map[string]*storagev1.StorageClass
	classesUnreadable bool
	// multiPodRWO is the two-pod probe's word: the last deep test's, then
	// this one's.
	multiPodRWO *bool
	// storage are the multi-node test results of this run, by class;
	// storageFresh the keys tested in it.
	storage      map[string]StorageTest
	storageFresh map[string]bool
}

// Full runs every check, passing each to report as it starts (RUNNING) and
// once it has a result, then, with t.Deep, the deep test's. report may be
// nil.
func Full(ctx context.Context, t Target, report func(*fleetgrpc.KubeCheck)) Result {
	timeout := FullTimeout
	if t.Deep {
		timeout += DeepTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	multiPod := t.MultiPodRWO
	if multiPod == nil {
		multiPod = t.StorageCache.MultiPodRWO()
	}
	r := &run{t: &t, report: report, caps: &fleetgrpc.KubeCapabilities{}, multiPodRWO: multiPod}
	r.full(ctx)
	return r.finish(true)
}

// Shallow is the probe between full runs: the API server answers, and the
// owner Lease is renewed.
func Shallow(ctx context.Context, t Target) Result {
	r := &run{t: &t, caps: &fleetgrpc.KubeCapabilities{}}
	if !r.reachable(ctx) {
		return r.finish(false)
	}
	r.claimLease(ctx)
	return r.finish(false)
}

func (r *run) full(ctx context.Context) {
	r.pass(CheckKubeconfig, r.kubeconfigDetail())
	if !r.credentials(ctx) {
		return
	}
	if !r.reachable(ctx) {
		return
	}
	r.identity(ctx)
	r.namespace(ctx)
	r.rbac(ctx)
	r.claimLease(ctx)
	r.nodes(ctx)
	r.admission(ctx)
	r.rootStorage(ctx) // reports the snapshots check too
	r.sharedStorage(ctx)
	r.limits(ctx)
	r.quota(ctx)
	r.metrics(ctx)
	r.runtimeClasses(ctx)
	r.exposure(ctx)
	r.imageDelivery(ctx)
	if r.t.Deep {
		r.deep(ctx)
	}
}

// finish derives the state from what the checks found.
func (r *run) finish(full bool) Result {
	res := r.res
	res.CheckedAt = r.t.now()
	switch {
	case r.failure != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:
		res.State, res.Message = r.failure, r.failureMsg
	case r.ownerConflict != "":
		res.State, res.Message = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT, r.ownerConflict
	case len(r.limited) > 0:
		res.State, res.Message = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, strings.Join(r.limited, " · ")
	default:
		res.State = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY
	}
	if full {
		res.Capabilities = r.caps
		if !r.res.Credential.ExpiresAt.IsZero() {
			r.caps.CredentialsExpireAt = timestamppb.New(r.res.Credential.ExpiresAt)
		}
	}
	return res
}

// --- reporting ---

func (r *run) emit(c *fleetgrpc.KubeCheck) {
	for i, have := range r.res.Checks {
		if have.GetName() == c.GetName() {
			r.res.Checks[i] = c
			r.send(c)
			return
		}
	}
	r.res.Checks = append(r.res.Checks, c)
	r.send(c)
}

func (r *run) send(c *fleetgrpc.KubeCheck) {
	if r.report != nil {
		r.report(c)
	}
}

func (r *run) running(name string) {
	r.emit(&fleetgrpc.KubeCheck{Name: name, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING})
}

func (r *run) pass(name, detail string) {
	r.emit(&fleetgrpc.KubeCheck{Name: name, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: detail})
}

func (r *run) warn(name, detail, fix string) {
	r.emit(&fleetgrpc.KubeCheck{Name: name, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN, Detail: detail, Fix: fix})
}

func (r *run) fail(name, detail, fix string) {
	r.emit(&fleetgrpc.KubeCheck{Name: name, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: detail, Fix: fix})
}

func (r *run) skip(name, detail string) {
	r.emit(&fleetgrpc.KubeCheck{Name: name, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: detail})
}

// failState records a failure state; the first one found wins.
func (r *run) failState(state fleetgrpc.KubeClusterState, msg string) {
	if r.failure == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED {
		r.failure, r.failureMsg = state, msg
	}
}

func (r *run) limit(reason string) {
	for _, have := range r.limited {
		if have == reason {
			return
		}
	}
	r.limited = append(r.limited, reason)
}

func (r *run) warning(line string) {
	r.res.Warnings = append(r.res.Warnings, line)
}

func call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

// --- kubeconfig, credentials, reach, identity ---

func (r *run) kubeconfigDetail() string {
	detail := r.t.Cluster.Context
	if r.t.Source != "" {
		detail = r.t.Source + ": " + detail
	}
	return detail
}

func (r *run) credentials(ctx context.Context) bool {
	if r.t.Credential == nil {
		return true
	}
	r.running(CheckCredentials)
	cctx, cancel := context.WithTimeout(ctx, CredentialTimeout)
	defer cancel()
	cred, err := r.t.Credential(cctx)
	r.res.Credential, r.res.CredentialErr = cred, err
	if err != nil {
		state, msg := Classify(err)
		if state == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE {
			state = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED
		}
		fix := ""
		if r.t.Auth == kubeexec.AuthExecPlugin {
			fix = "log in again: [Run login on daemon], or the plugin's own login on the daemon host"
		}
		r.fail(CheckCredentials, msg, fix)
		r.failState(state, msg)
		return false
	}
	now := r.t.now()
	if !cred.ExpiresAt.IsZero() && !cred.Refreshable && !now.Before(cred.ExpiresAt) {
		msg := "credentials expired " + Ago(now.Sub(cred.ExpiresAt)) + " ago"
		r.fail(CheckCredentials, msg, "replace the credential in the kubeconfig on the daemon host")
		r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, msg)
		return false
	}
	r.pass(CheckCredentials, describeCredential(r.t.Auth, r.t.Plugin, cred, now))
	return true
}

func describeCredential(auth kubeexec.AuthKind, plugin string, cred kubeexec.Credential, now time.Time) string {
	what := string(auth)
	switch auth {
	case kubeexec.AuthExecPlugin:
		what = "exec " + plugin
	case kubeexec.AuthClientCert:
		what = "client certificate"
	case kubeexec.AuthTokenFile:
		what = "token file"
	}
	if !cred.ExpiresAt.IsZero() {
		what += " (expires in " + Ago(cred.ExpiresAt.Sub(now)) + ")"
	}
	return what
}

// Ago renders a duration the way the status lines do: "54m", "2h", "3d".
func Ago(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func (r *run) reachable(ctx context.Context) bool {
	r.running(CheckReachable)
	cctx, cancel := call(ctx)
	defer cancel()
	v, err := serverVersion(cctx, r.t.Client)
	if err != nil {
		state, msg := Classify(err)
		if state == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN {
			// /version is readable by everyone a cluster lets in at all.
			state = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED
		}
		r.fail(CheckReachable, msg, "")
		r.failState(state, msg)
		return false
	}
	r.caps.Version = v
	r.pass(CheckReachable, v)
	return true
}

func (r *run) identity(ctx context.Context) {
	r.running(CheckIdentity)
	cctx, cancel := call(ctx)
	defer cancel()
	review, err := r.t.Client.AuthenticationV1().SelfSubjectReviews().Create(cctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err == nil {
		r.user = review.Status.UserInfo.Username
		r.caps.User = r.user
	}
	ns, uidErr := r.t.Client.CoreV1().Namespaces().Get(cctx, "kube-system", metav1.GetOptions{})
	if uidErr == nil {
		r.res.ClusterUID = string(ns.UID)
	}
	var parts []string
	if r.user != "" {
		parts = append(parts, r.user)
	} else {
		parts = append(parts, "user unknown")
	}
	if r.res.ClusterUID != "" {
		parts = append(parts, "cluster "+shortUID(r.res.ClusterUID))
	}
	switch {
	case err != nil && uidErr != nil:
		r.warn(CheckIdentity, strings.Join(parts, " · ")+": cannot read who fleet acts as or the cluster's id", "re-pointing this connection needs kube-system to be readable")
	case uidErr != nil:
		r.warn(CheckIdentity, strings.Join(parts, " · ")+" · cluster id unreadable", "re-pointing this connection needs kube-system to be readable")
	default:
		r.pass(CheckIdentity, strings.Join(parts, " · "))
	}
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// --- namespace ---

// Namespace labels fleet reads.
const (
	labelPSAEnforce = "pod-security.kubernetes.io/enforce"
)

// injectionLabels are namespace labels (and their enabling values) that make
// a service mesh inject sidecars into every pod.
var injectionLabels = map[string]string{
	"istio-injection":           "enabled",
	"istio.io/rev":              "",
	"linkerd.io/inject":         "enabled",
	"kuma.io/sidecar-injection": "enabled",
}

func (r *run) namespace(ctx context.Context) {
	ns := r.t.Cluster.Namespace
	r.running(CheckNamespace)
	cctx, cancel := call(ctx)
	defer cancel()
	obj, err := r.t.Client.CoreV1().Namespaces().Get(cctx, ns, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		msg := fmt.Sprintf("namespace '%s' does not exist", ns)
		r.fail(CheckNamespace, msg, "create it from the cluster editor (shows the labels), or pick another namespace")
		r.failState(fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED, msg)
		return
	case apierrors.IsForbidden(err):
		// Readable or not, a namespace fleet can create pods in exists; the
		// admission canaries tell.
		r.nsExists = true
		r.warn(CheckNamespace, fmt.Sprintf("%s (its labels are not readable: pod-security level unknown)", ns), "")
		return
	case err != nil:
		r.warn(CheckNamespace, fmt.Sprintf("%s: %s", ns, Message(err)), "")
		r.nsExists = true
		return
	}
	r.nsExists = true
	parts := []string{ns + " exists"}
	if level := obj.Labels[labelPSAEnforce]; level != "" {
		r.caps.PsaLevel = level
		parts = append(parts, "pod-security: "+level)
	}
	for _, key := range sortedKeys(obj.Labels) {
		want, ok := injectionLabels[key]
		if ok && (want == "" || obj.Labels[key] == want) {
			r.caps.InjectionLabels = append(r.caps.InjectionLabels, key+"="+obj.Labels[key])
		}
	}
	if len(r.caps.InjectionLabels) > 0 {
		parts = append(parts, "mesh injection: "+strings.Join(r.caps.InjectionLabels, ", ")+" (fleet pods opt out)")
	}
	r.pass(CheckNamespace, strings.Join(parts, " · "))
}

// --- owner Lease ---

func (r *run) claimLease(ctx context.Context) {
	if r.t.Lease == nil {
		return
	}
	r.running(CheckOwner)
	cctx, cancel := call(ctx)
	defer cancel()
	st, err := r.t.Lease.Ensure(cctx)
	if err != nil {
		if IsUnreachable(err) {
			state, msg := Classify(err)
			r.fail(CheckOwner, msg, "")
			r.failState(state, msg)
			return
		}
		r.warn(CheckOwner, "cannot claim the owner Lease: "+Message(err), "grant create/get/update on leases (coordination.k8s.io)")
		return
	}
	r.res.Lease = &st
	if st.Held {
		r.pass(CheckOwner, "this daemon holds the owner Lease")
		return
	}
	msg := OwnerConflictMessage(st, r.t.now())
	r.fail(CheckOwner, msg, "stop the other daemon (a copy of this ~/.fleet), or wait for its Lease to expire")
	r.ownerConflict = msg
}

// OwnerConflictMessage says who holds a Lease this daemon does not.
func OwnerConflictMessage(st owner.LeaseStatus, now time.Time) string {
	host := st.HolderHost
	if host == "" {
		host = "another host"
	}
	msg := "another fleetd with this daemon's id holds the connection (on " + host
	if !st.RenewedAt.IsZero() {
		msg += ", renewed " + Ago(now.Sub(st.RenewedAt)) + " ago"
	}
	msg += ")"
	if !st.FreeAt.IsZero() {
		msg += "; adoption and GC are refused until " + st.FreeAt.Local().Format("15:04:05")
	}
	return msg
}

// --- helpers ---

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
