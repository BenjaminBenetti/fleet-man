//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube.go serves the Kubernetes connection RPCs. Every one is served to
// remote callers (the gateway tunnel, the Armada SSH listener) exactly as
// over the local socket: remote Fleet is full control, so a remote TUI or
// CLI lists the daemon host's contexts, adds, edits (every field, privileged
// pods and plugin settings included) and removes connections, tests and
// prunes them, and logs in on the daemon. Cluster I/O always runs here, so no
// reply carries kubeconfig contents, a token or the gateway credential
// (KubeCluster has no field for it).

// kubeRepointTimeout bounds the cluster reads that prove a re-point.
const kubeRepointTimeout = 30 * time.Second

// kubeRecordFromProto is a client's record with every daemon-owned field
// cleared.
func kubeRecordFromProto(pc *fleetgrpc.KubeCluster) fleet.KubeCluster {
	rec := protoconv.KubeClusterFromProto(pc)
	rec.ID, rec.Server, rec.ClusterUID = "", "", ""
	rec.CreatedAt, rec.UpdatedAt, rec.VerifiedAt, rec.Revision = time.Time{}, time.Time{}, time.Time{}, 0
	rec.Image.GatewayCredential = ""
	return rec
}

// kubeDefaults fills an unsaved record the way AddKubeCluster does: fleetd's
// kubeconfig files, the context's namespace (else "default"), the plugin
// variables fleetd has for the context's plugin (defaultPluginEnv), and
// checks the context exists there.
func (s *service) kubeDefaults(rec *fleet.KubeCluster) error {
	if rec.Context == "" {
		return invalidf("context is required")
	}
	if len(rec.KubeconfigPaths) == 0 {
		rec.KubeconfigPaths = kubeexec.DefaultKubeconfigPaths(s.kube.environ())
	}
	for i, p := range rec.KubeconfigPaths {
		if err := checkAbsPath(p); err != nil {
			return invalidf("kubeconfig_paths: %v", err)
		}
		rec.KubeconfigPaths[i] = filepath.Clean(p)
	}
	kc, err := findContext(*rec)
	if err != nil {
		return err
	}
	if rec.PluginEnv == nil {
		rec.PluginEnv = defaultPluginEnv(kc.Plugin, s.kube.environ())
	}
	if rec.Namespace == "" {
		rec.Namespace = kc.Namespace
	}
	if rec.Namespace == "" {
		rec.Namespace = "default"
	}
	return nil
}

// findContext is a record's context in its kubeconfig files.
func findContext(rec fleet.KubeCluster) (kubeexec.KubeContext, error) {
	contexts, err := kubeexec.ListContexts(rec.KubeconfigPaths)
	if err != nil {
		return kubeexec.KubeContext{}, status.Errorf(codes.FailedPrecondition, "reading the kubeconfig: %s", preflight.Message(err))
	}
	for _, c := range contexts {
		if c.Name == rec.Context {
			return c, nil
		}
	}
	return kubeexec.KubeContext{}, status.Errorf(codes.NotFound, "no context %q in %v on the daemon host", rec.Context, rec.KubeconfigPaths)
}

// ListKubeContexts lists the contexts of kubeconfig files on the daemon host.
func (s *service) ListKubeContexts(_ context.Context, req *fleetgrpc.ListKubeContextsRequest) (*fleetgrpc.ListKubeContextsReply, error) {
	environ := s.kube.environ()
	paths := req.GetKubeconfigPaths()
	if len(paths) == 0 {
		paths = kubeexec.DefaultKubeconfigPaths(environ)
	}
	for _, p := range paths {
		if err := checkAbsPath(p); err != nil {
			return nil, invalidf("kubeconfig_paths: %v", err)
		}
	}
	contexts, err := kubeexec.ListContexts(paths)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "reading the kubeconfig: %s", preflight.Message(err))
	}
	current, _ := kubeexec.CurrentContext(paths)
	connected := map[string][]string{}
	for _, st := range s.kube.statuses() {
		c := st.GetCluster()
		for _, p := range c.GetKubeconfigPaths() {
			connected[c.GetContext()+"\x00"+filepath.Clean(p)] = append(connected[c.GetContext()+"\x00"+filepath.Clean(p)], c.GetId())
		}
	}
	host, _ := daemonHostname()
	reply := &fleetgrpc.ListKubeContextsReply{KubeconfigPaths: paths, Host: host}
	for _, c := range contexts {
		kc := &fleetgrpc.KubeContext{
			Name: c.Name, Cluster: c.Cluster, Server: c.Server, Namespace: c.Namespace, User: c.User,
			Auth: string(c.Auth), Plugin: c.Plugin, Source: c.Source, Current: c.Name == current,
			Connected: connected[c.Name+"\x00"+filepath.Clean(c.Source)],
		}
		if c.Plugin != "" {
			if path, err := kubeexec.LookupPlugin(c.Plugin, nil, environ); err == nil {
				kc.PluginPath = path
			} else {
				kc.PluginMissing = preflight.Message(err)
			}
		}
		reply.Contexts = append(reply.Contexts, kc)
	}
	return reply, nil
}

// ListKubeClusters returns every connection with its health.
func (s *service) ListKubeClusters(context.Context, *fleetgrpc.ListKubeClustersRequest) (*fleetgrpc.ListKubeClustersReply, error) {
	return &fleetgrpc.ListKubeClustersReply{Clusters: s.kube.statuses()}, nil
}

// AddKubeCluster saves a new connection and starts probing it.
func (s *service) AddKubeCluster(_ context.Context, req *fleetgrpc.AddKubeClusterRequest) (*fleetgrpc.AddKubeClusterReply, error) {
	rec := kubeRecordFromProto(req.GetCluster())
	if err := s.kubeDefaults(&rec); err != nil {
		return nil, err
	}
	if err := validateKubeCluster(&rec); err != nil {
		return nil, err
	}
	now := s.kube.now().UTC()
	err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		if err := checkUnique(&rec, k.Clusters); err != nil {
			return err
		}
		rec.ID = names.NewClusterID(rec.Context, func(id string) bool { return k.Find(id) != nil })
		rec.CreatedAt, rec.UpdatedAt, rec.Revision = now, now, 1
		k.Clusters = append(k.Clusters, rec)
		return nil
	})
	if err != nil {
		return nil, kubeStoreError(err)
	}
	flog.Info("kube: connection added", "cluster", rec.ID, "context", rec.Context, "namespace", rec.Namespace)
	s.kube.sync()
	return &fleetgrpc.AddKubeClusterReply{Cluster: s.kube.status(rec.ID)}, nil
}

// kubeStoreError passes a status error through and wraps any other failure
// to write the registry.
func kubeStoreError(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, state.ErrKubeClustersNewer) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Errorf(codes.Internal, "saving Kubernetes connections: %v", err)
}

// UpdateKubeCluster changes the fields its mask names. A re-point (context
// or kubeconfig files) of a connection that instances or agents use must
// reach the same cluster and see their objects first.
func (s *service) UpdateKubeCluster(ctx context.Context, req *fleetgrpc.UpdateKubeClusterRequest) (*fleetgrpc.UpdateKubeClusterReply, error) {
	cur, ok := s.kube.record(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no Kubernetes connection %q", req.GetId())
	}
	if want := req.GetExpectedRevision(); want != 0 && want != cur.Revision {
		return nil, status.Errorf(codes.Aborted, "connection %q changed since it was read (revision %d, now %d)", cur.GetDisplayName(), want, cur.Revision)
	}
	next, err := applyKubeMask(cur, req.GetCluster(), req.GetUpdateMask())
	if err != nil {
		return nil, err
	}
	for i, p := range next.KubeconfigPaths {
		next.KubeconfigPaths[i] = filepath.Clean(p)
	}
	if err := validateKubeCluster(&next); err != nil {
		return nil, err
	}
	repoint := next.Context != cur.Context || !samePaths(next.KubeconfigPaths, cur.KubeconfigPaths)
	if repoint {
		if _, err := findContext(next); err != nil {
			return nil, err
		}
	}
	forget := false // the probe's notes about the old target go
	if repoint || next.Namespace != cur.Namespace {
		users, err := s.kubeUsers(cur.ID)
		if err != nil {
			return nil, err
		}
		switch {
		case len(users) > 0 && next.Namespace != cur.Namespace:
			return nil, status.Errorf(codes.FailedPrecondition, "connection %q is in use (%s): its namespace cannot change; add another connection for namespace %q", cur.GetDisplayName(), describeUsers(users), next.Namespace)
		case len(users) > 0:
			if err := s.verifyRepoint(ctx, cur, next, users); err != nil {
				return nil, err
			}
		default:
			// Nothing depends on the old target: the next probe records
			// the new one's server and cluster id.
			forget = true
		}
	}
	now := s.kube.now().UTC()
	err = state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		stored := k.Find(cur.ID)
		if stored == nil {
			return status.Errorf(codes.NotFound, "no Kubernetes connection %q", cur.ID)
		}
		if stored.Revision != cur.Revision {
			return status.Errorf(codes.Aborted, "connection %q changed while it was being updated; read it again", cur.GetDisplayName())
		}
		if err := checkUnique(&next, k.Clusters); err != nil {
			return err
		}
		// The client's fields are as checked (the revision has not moved);
		// the daemon's own come from the file as it is now.
		next.Server, next.ClusterUID, next.VerifiedAt = stored.Server, stored.ClusterUID, stored.VerifiedAt
		next.Image.GatewayCredential = stored.Image.GatewayCredential
		if forget {
			next.Server, next.ClusterUID = "", ""
		}
		next.UpdatedAt, next.Revision = now, cur.Revision+1
		*stored = next
		return nil
	})
	if err != nil {
		return nil, kubeStoreError(err)
	}
	flog.Info("kube: connection updated", "cluster", cur.ID, "fields", req.GetUpdateMask().GetPaths())
	s.kube.sync()
	return &fleetgrpc.UpdateKubeClusterReply{Cluster: s.kube.status(cur.ID)}, nil
}

func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if filepath.Clean(a[i]) != filepath.Clean(b[i]) {
			return false
		}
	}
	return true
}

// verifyRepoint proves that next reaches the cluster cur did: the same
// kube-system UID, and every user instance's StatefulSet visible under this
// daemon's labels.
func (s *service) verifyRepoint(ctx context.Context, cur, next fleet.KubeCluster, users []*fleetgrpc.KubeClusterUser) error {
	if cur.ClusterUID == "" {
		return status.Errorf(codes.FailedPrecondition, "connection %q is in use, and its cluster's id was never read (kube-system was not readable): it cannot be re-pointed", cur.GetDisplayName())
	}
	link, err := s.kube.connect(next, s.kube.environ())
	if err != nil {
		_, msg := preflight.Classify(err)
		return status.Errorf(codes.FailedPrecondition, "the new target does not load: %s", msg)
	}
	ctx, cancel := context.WithTimeout(ctx, kubeRepointTimeout)
	defer cancel()
	ns, err := link.client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		_, msg := preflight.Classify(err)
		return status.Errorf(codes.FailedPrecondition, "reading the new target's cluster id: %s", msg)
	}
	if string(ns.UID) != cur.ClusterUID {
		return status.Errorf(codes.FailedPrecondition, "context %q reaches a different cluster (id %s, the connection's is %s): add it as a new connection instead", next.Context, ns.UID, cur.ClusterUID)
	}
	st, _ := state.Load()
	selector := names.Selector(s.kube.ownerLabel(), cur.ID)
	list, err := link.client.AppsV1().StatefulSets(next.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		_, msg := preflight.Classify(err)
		return status.Errorf(codes.FailedPrecondition, "listing the instances through the new target: %s", msg)
	}
	found := map[string]bool{}
	for _, sts := range list.Items {
		found[sts.Name] = true
	}
	for _, u := range users {
		if u.GetInstance() == "" || st == nil {
			continue
		}
		inst := findInstance(st, u.GetFleet(), u.GetInstance())
		if inst == nil || inst.KubeKey == "" {
			continue // never got objects
		}
		if name := names.Instance(u.GetFleet(), u.GetInstance(), inst.KubeKey); !found[name] {
			return status.Errorf(codes.FailedPrecondition, "instance %s/%s (StatefulSet %s) is not visible through context %q", u.GetFleet(), u.GetInstance(), name, next.Context)
		}
	}
	return nil
}

// kubeUsers are the instances and agents that use a connection. An
// unreadable state.json is an error, never "no users": a removal or a
// re-point must not proceed on a guess.
func (s *service) kubeUsers(id string) ([]*fleetgrpc.KubeClusterUser, error) {
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	return kubeUsersIn(st, id), nil
}

// kubeUsersIn is kubeUsers of st.
func kubeUsersIn(st *state.State, id string) []*fleetgrpc.KubeClusterUser {
	var users []*fleetgrpc.KubeClusterUser
	for _, name := range sortedStateFleets(st) {
		f := st.Fleets[name]
		for _, inst := range f.Instances {
			if inst.KubeCluster == id {
				users = append(users, &fleetgrpc.KubeClusterUser{Fleet: name, Instance: inst.Name, Status: protoconv.StatusToProto(inst.Status)})
			}
		}
		for _, a := range f.Settings.Agents {
			if a.KubeCluster == id {
				users = append(users, &fleetgrpc.KubeClusterUser{Fleet: name, Agent: a.Name})
			}
		}
	}
	return users
}

func sortedStateFleets(st *state.State) []string {
	names := make([]string, 0, len(st.Fleets))
	for name := range st.Fleets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func describeUsers(users []*fleetgrpc.KubeClusterUser) string {
	instances, agents := 0, 0
	for _, u := range users {
		if u.GetAgent() != "" {
			agents++
		} else {
			instances++
		}
	}
	switch {
	case agents == 0:
		return fmt.Sprintf("%d instance%s", instances, plural(instances))
	case instances == 0:
		return fmt.Sprintf("%d agent%s", agents, plural(agents))
	}
	return fmt.Sprintf("%d instance%s and %d agent%s", instances, plural(instances), agents, plural(agents))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clusterMissing is the error FORGET leaves on the instances of a removed
// connection.
const clusterMissingPrefix = "kubernetes:cluster_missing"

// RemoveKubeCluster disconnects a connection.
func (s *service) RemoveKubeCluster(ctx context.Context, req *fleetgrpc.RemoveKubeClusterRequest) (*fleetgrpc.RemoveKubeClusterReply, error) {
	rec, ok := s.kube.record(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no Kubernetes connection %q", req.GetId())
	}
	users, err := s.kubeUsers(rec.ID)
	if err != nil {
		return nil, err
	}
	reply := &fleetgrpc.RemoveKubeClusterReply{Users: users}
	switch req.GetMode() {
	case fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_UNSPECIFIED, fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED:
		if len(users) > 0 {
			return reply, nil
		}
	case fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES:
		warnings, err := s.destroyKubeInstances(ctx, rec, users)
		reply.Warnings = warnings
		if err != nil {
			return nil, err
		}
	case fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET:
		reply.Warnings = s.forgetKubeInstances(ctx, rec, users)
	default:
		return nil, invalidf("unknown remove mode %v", req.GetMode())
	}
	// Asked before the removal stops the worker (and releases its Lease).
	leaseHeld := s.kube.leaseHeld(rec.ID)
	// The users are looked at again, and the record removed, under the state
	// lock a create holds while it checks its connection and adds its
	// record: a create cannot slip in between and leave an instance on a
	// connection that is gone. A forget takes whatever is there with it.
	refuse := req.GetMode() != fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET
	beforeKubeRemove()
	var late []*fleetgrpc.KubeClusterUser
	err = state.Update(func(st *state.State) error {
		if refuse {
			if late = kubeUsersIn(st, rec.ID); len(late) > 0 {
				return errKeepState
			}
		}
		if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
			if !k.Remove(rec.ID) {
				return status.Errorf(codes.NotFound, "no Kubernetes connection %q", rec.ID)
			}
			return nil
		}); err != nil {
			return err
		}
		return errKeepState
	})
	switch {
	case len(late) > 0:
		reply.Users = late
		return reply, nil
	case err != nil && !errors.Is(err, errKeepState):
		return nil, kubeStoreError(err)
	}
	s.forgetDefaultKubeCluster(rec.ID)
	flog.Info("kube: connection removed", "cluster", rec.ID, "mode", req.GetMode().String())
	s.kube.sync()
	s.kube.releaseImages(rec, leaseHeld)
	// The fleets' objects there are out of reach now; a fleet teardown owed
	// there with them.
	if err := kubernetes.ForgetClusterFleets(rec.ID); err != nil {
		flog.Warn("kube: forgetting a removed connection's fleets", "cluster", rec.ID, "err", err)
	}
	reply.Removed = true
	return reply, nil
}

// errKeepState ends a state.Update that only needed the state lock.
var errKeepState = errors.New("nothing to write")

// beforeKubeRemove runs between a removal's first look at the connection's
// users and its removal under the state lock (a seam for tests: a create
// that lands in between).
var beforeKubeRemove = func() {}

// destroyKubeInstances destroys every instance on the connection and waits
// for the teardowns. Agents block it: they would recreate instances there.
func (s *service) destroyKubeInstances(ctx context.Context, rec fleet.KubeCluster, users []*fleetgrpc.KubeClusterUser) ([]string, error) {
	var jobs []*job
	for _, u := range users {
		if u.GetAgent() != "" {
			return nil, status.Errorf(codes.FailedPrecondition, "agent %s/%s still targets connection %q: give it another target first", u.GetFleet(), u.GetAgent(), rec.GetDisplayName())
		}
	}
	for _, u := range users {
		instance := u.GetInstance()
		j, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: u.GetFleet(), Instance: &instance})
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	var warnings []string
	for _, j := range jobs {
		select {
		case <-j.exited:
		case <-ctx.Done():
			return warnings, status.Error(codes.DeadlineExceeded, "the instances are still being destroyed; the connection stays until they are gone")
		}
		if done := j.outcome(); done != nil {
			if done.GetError() != "" {
				return warnings, status.Errorf(codes.Aborted, "destroying %s/%s: %s", j.summary.GetFleet(), j.summary.GetInstance(), done.GetError())
			}
			warnings = append(warnings, done.GetWarnings()...)
		}
	}
	return warnings, nil
}

// forgetKubeInstances leaves a removed connection's objects in the cluster,
// scaled to 0 where it is reachable, and fails its instance records.
func (s *service) forgetKubeInstances(ctx context.Context, rec fleet.KubeCluster, users []*fleetgrpc.KubeClusterUser) []string {
	var warnings []string
	if hasInstances(users) {
		if err := s.scaleKubeInstancesDown(ctx, rec); err != nil {
			warnings = append(warnings, "the instances were left running in the cluster: "+preflight.Message(err))
		}
	}
	msg := clusterMissingPrefix + ": connection " + rec.GetDisplayName() + " was removed; its disks stay in namespace " + rec.Namespace
	_ = state.Update(func(st *state.State) error {
		for _, f := range st.Fleets {
			for _, inst := range f.Instances {
				if inst.KubeCluster == rec.ID {
					inst.Status, inst.Error = fleet.StatusFailed, msg
				}
			}
		}
		return nil
	})
	s.pushState()
	return warnings
}

func hasInstances(users []*fleetgrpc.KubeClusterUser) bool {
	for _, u := range users {
		if u.GetInstance() != "" {
			return true
		}
	}
	return false
}

// scaleKubeInstancesDown stops every instance StatefulSet of a connection.
func (s *service) scaleKubeInstancesDown(ctx context.Context, rec fleet.KubeCluster) error {
	link, err := s.kube.connect(rec, s.kube.environ())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, kubeRepointTimeout)
	defer cancel()
	sel := names.Selector(s.kube.ownerLabel(), rec.ID) + "," + names.LabelRole + "=" + string(names.RoleInstance)
	list, err := link.client.AppsV1().StatefulSets(rec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return err
	}
	for _, sts := range list.Items {
		if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
			continue
		}
		if _, err := link.client.AppsV1().StatefulSets(rec.Namespace).Patch(ctx, sts.Name, types.MergePatchType, []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// forgetDefaultKubeCluster drops a removed connection as the remembered
// default deploy target.
func (s *service) forgetDefaultKubeCluster(id string) {
	s.muWrite.Lock()
	defer s.muWrite.Unlock()
	cfg, err := state.LoadConfig()
	if err != nil || cfg.DefaultKubeCluster != id {
		return
	}
	cfg.DefaultKubeCluster = ""
	if cfg.DefaultBackend == string(fleet.BackendKubernetes) {
		cfg.DefaultBackend = ""
	}
	if err := state.SaveConfig(cfg); err != nil {
		flog.Warn("kube: forget the remembered default target", "cluster", id, "err", err)
	}
}

// TestKubeCluster runs the full preflight on a saved or an unsaved
// connection, streaming each check.
func (s *service) TestKubeCluster(req *fleetgrpc.TestKubeClusterRequest, stream grpc.ServerStreamingServer[fleetgrpc.TestKubeClusterEvent]) error {
	ctx := stream.Context()
	var sendErr error
	report := func(c *fleetgrpc.KubeCheck) {
		if sendErr == nil {
			sendErr = stream.Send(&fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Check{Check: c}})
		}
	}
	var result *fleetgrpc.KubeClusterStatus
	if id := req.GetId(); id != "" {
		st, err := s.kube.test(ctx, id, req.GetDeep(), report)
		if err != nil {
			return err
		}
		result = st
	} else {
		rec := kubeRecordFromProto(req.GetCluster())
		if err := s.kubeDefaults(&rec); err != nil {
			return err
		}
		if err := validateKubeCluster(&rec); err != nil {
			return err
		}
		result = s.kube.testDraft(ctx, rec, report)
	}
	if sendErr != nil {
		return sendErr
	}
	return stream.Send(&fleetgrpc.TestKubeClusterEvent{Kind: &fleetgrpc.TestKubeClusterEvent_Result{Result: result}})
}

// test runs a full preflight on a saved connection, with deep the deep
// test's probes too; the result becomes its health.
func (m *kubeManager) test(ctx context.Context, id string, deep bool, report func(*fleetgrpc.KubeCheck)) (*fleetgrpc.KubeClusterStatus, error) {
	w := m.worker(id)
	if w == nil {
		// No serve loop runs the connection (a newService test, or the
		// moment before run starts it): judge it with a transient worker.
		rec, ok := m.record(id)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no Kubernetes connection %q", id)
		}
		w = m.newWorker(rec)
	}
	w.runFullDeep(ctx, report, deep)
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return w.status(), nil
}

// testDraft judges an unsaved connection: nothing is saved or claimed.
func (m *kubeManager) testDraft(ctx context.Context, rec fleet.KubeCluster, report func(*fleetgrpc.KubeCheck)) *fleetgrpc.KubeClusterStatus {
	var res preflight.Result
	link, err := m.connect(rec, m.environ())
	if err != nil {
		res = preflight.LoadFailure(err, m.now())
		if report != nil {
			for _, c := range res.Checks {
				report(c)
			}
		}
	} else {
		w := &kubeWorker{m: m, rec: rec}
		res = preflight.Full(ctx, w.target(rec, link, nil), report)
	}
	w := &kubeWorker{m: m, rec: rec, haveFull: true, full: res, lastFull: res.CheckedAt, lastProbe: res.CheckedAt, cred: res.Credential}
	return w.status()
}

// LoginKubeCluster runs a connection's login plugin on the daemon host and
// relays only its device-code URL and code; then the connection is tested.
func (s *service) LoginKubeCluster(req *fleetgrpc.LoginKubeClusterRequest, stream grpc.ServerStreamingServer[fleetgrpc.LoginKubeClusterEvent]) error {
	ctx := stream.Context()
	rec, ok := s.kube.record(req.GetId())
	if !ok {
		return status.Errorf(codes.NotFound, "no Kubernetes connection %q", req.GetId())
	}
	link, err := s.kube.connect(rec, s.kube.environ())
	if err != nil {
		_, msg := preflight.Classify(err)
		return status.Errorf(codes.FailedPrecondition, "the connection does not load: %s", msg)
	}
	if link.login == nil || link.auth != kubeexec.AuthExecPlugin {
		return status.Errorf(codes.FailedPrecondition, "connection %q does not log in through a credential plugin (it uses %s)", rec.GetDisplayName(), link.auth)
	}
	var sendErr error
	_, err = link.login(ctx, func(code kubeexec.DeviceCode) {
		if sendErr == nil {
			sendErr = stream.Send(&fleetgrpc.LoginKubeClusterEvent{Kind: &fleetgrpc.LoginKubeClusterEvent_DeviceCode{
				DeviceCode: &fleetgrpc.KubeDeviceCode{Url: code.URL, Code: code.Code},
			}})
		}
	})
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		return status.Errorf(codes.Unauthenticated, "login failed: %s", preflight.Message(err))
	}
	flog.Info("kube: login on daemon succeeded", "cluster", rec.ID)
	result, err := s.kube.test(ctx, rec.ID, false, nil)
	if err != nil {
		return err
	}
	return stream.Send(&fleetgrpc.LoginKubeClusterEvent{Kind: &fleetgrpc.LoginKubeClusterEvent_Result{Result: result}})
}

// CreateKubeNamespace creates (or with dry_run renders) a connection's
// namespace. Fleet never creates one on its own: this is the explicit
// action, and the manifest shows exactly what it makes.
func (s *service) CreateKubeNamespace(ctx context.Context, req *fleetgrpc.CreateKubeNamespaceRequest) (*fleetgrpc.CreateKubeNamespaceReply, error) {
	var rec fleet.KubeCluster
	if id := req.GetId(); id != "" {
		var ok bool
		if rec, ok = s.kube.record(id); !ok {
			return nil, status.Errorf(codes.NotFound, "no Kubernetes connection %q", id)
		}
	} else {
		rec = kubeRecordFromProto(req.GetCluster())
		if err := s.kubeDefaults(&rec); err != nil {
			return nil, err
		}
	}
	name := req.GetNamespace()
	if name == "" {
		name = rec.Namespace
	}
	if !names.IsDNS1123Label(name) {
		return nil, invalidf("namespace %q is not a valid namespace name", name)
	}
	labels := req.GetLabels()
	if len(labels) == 0 {
		labels = map[string]string{names.LabelManagedBy: names.ManagedBy}
	}
	for k, v := range labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, invalidf("label %q: %v", k, errs)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return nil, invalidf("label %s=%q: %v", k, v, errs)
		}
	}
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	manifest, err := yaml.Marshal(ns)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "rendering the namespace: %v", err)
	}
	reply := &fleetgrpc.CreateKubeNamespaceReply{Manifest: cleanManifest(string(manifest))}
	if req.GetDryRun() {
		return reply, nil
	}
	link, err := s.kube.connect(rec, s.kube.environ())
	if err != nil {
		_, msg := preflight.Classify(err)
		return nil, status.Errorf(codes.FailedPrecondition, "the connection does not load: %s", msg)
	}
	cctx, cancel := context.WithTimeout(ctx, kubeRepointTimeout)
	defer cancel()
	ns.TypeMeta = metav1.TypeMeta{}
	if _, err := link.client.CoreV1().Namespaces().Create(cctx, ns, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, status.Errorf(codes.AlreadyExists, "namespace %q already exists", name)
		}
		kind, msg := preflight.Classify(err)
		if kind == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN {
			return nil, status.Errorf(codes.PermissionDenied, "creating namespace %q: %s (it needs namespaces create; apply the manifest as someone who has it)", name, msg)
		}
		return nil, status.Errorf(codes.Unavailable, "creating namespace %q: %s", name, msg)
	}
	flog.Info("kube: namespace created", "cluster", rec.ID, "namespace", name)
	reply.Created = true
	if rec.ID != "" {
		if w := s.kube.worker(rec.ID); w != nil {
			w.mu.Lock()
			w.fullWanted = true
			w.mu.Unlock()
			w.poke()
		}
	}
	return reply, nil
}

// cleanManifest drops the empty fields yaml renders for an unsaved object.
func cleanManifest(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		switch line {
		case "  creationTimestamp: null", "spec: {}", "status: {}":
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
