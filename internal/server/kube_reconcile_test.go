//go:build !nok8s

package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeRecoveryPool stands in for the pool: its scope is the fixture's fake
// cluster, and the undos are the test's.
type fakeRecoveryPool struct {
	mu     sync.Mutex
	scope  kubernetes.Scope
	gate   kubernetes.OwnerGate
	undo   func(kubernetes.Journal) (kubernetes.RebuildUndo, error)
	undos  []kubernetes.Journal
	clones []kubernetes.Journal
}

func (p *fakeRecoveryPool) Scope(id string) (kubernetes.Scope, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.scope
	s.Cluster = id
	gate := p.gate
	if gate != nil {
		s.Gate = func(ctx context.Context) error { return gate(ctx, id) }
	}
	return s, nil
}

func (p *fakeRecoveryPool) UndoRebuild(_ context.Context, j kubernetes.Journal, progress func(string)) (kubernetes.RebuildUndo, error) {
	p.mu.Lock()
	p.undos = append(p.undos, j)
	undo := p.undo
	p.mu.Unlock()
	progress("restoring")
	if undo == nil {
		return kubernetes.RebuildUndo{}, nil
	}
	return undo(j)
}

func (p *fakeRecoveryPool) UndoClone(_ context.Context, j kubernetes.Journal) (kubernetes.Teardown, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clones = append(p.clones, j)
	return kubernetes.Teardown{Deleted: []string{"job/copier"}}, nil
}

func (p *fakeRecoveryPool) SetOwnerGate(g kubernetes.OwnerGate) {
	p.mu.Lock()
	p.gate = g
	p.mu.Unlock()
}

func (p *fakeRecoveryPool) undoCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.undos)
}

// recoveryFixture is a kube fixture with one connection (minikube,
// namespace fleet) whose reconciler drives the fake pool.
type recoveryFixture struct {
	*kubeFixture
	id   string
	pool *fakeRecoveryPool
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	pool := &fakeRecoveryPool{scope: kubernetes.Scope{Client: f.apis["minikube"], Namespace: "fleet", Owner: testIdentity.Owner()}}
	f.svc.kubeRecovery.pool = func() kubeRecoveryPool { return pool }
	oldPoll, oldAttempts, oldRetry := kubeUndoPoll, kubeUndoAttempts, kubeUndoRetry
	kubeUndoPoll, kubeUndoAttempts, kubeUndoRetry = 10*time.Millisecond, 2, 10*time.Millisecond
	t.Cleanup(func() { kubeUndoPoll, kubeUndoAttempts, kubeUndoRetry = oldPoll, oldAttempts, oldRetry })
	return &recoveryFixture{kubeFixture: f, id: id, pool: pool}
}

// record seeds a Kubernetes record of fleet alpha on the connection.
func (r *recoveryFixture) record(name, key string, status fleet.InstanceStatus, errMsg string) {
	r.t.Helper()
	err := state.Update(func(st *state.State) error {
		f := st.Fleets["alpha"]
		if f == nil {
			f = &fleet.Fleet{Name: "alpha", Remote: "git@example.com:alpha.git"}
			st.Fleets["alpha"] = f
		}
		f.Instances = append(f.Instances, &fleet.Instance{Name: name, Status: status, Error: errMsg, ContainerID: "k8s:" + r.id + "/fleet/fi-alpha-" + name + "@g2",
			Backend: fleet.BackendKubernetes, KubeCluster: r.id, KubeKey: key})
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// journal writes a journal for alpha/name.
func (r *recoveryFixture) journal(name, key string, op kubernetes.Op, step kubernetes.Step, mutate func(*kubernetes.Journal)) {
	r.t.Helper()
	j := kubernetes.Journal{Op: op, Fleet: "alpha", Instance: name, Cluster: r.id, Namespace: "fleet", Key: key, Gen: 1, Step: step}
	if mutate != nil {
		mutate(&j)
	}
	if _, err := kubernetes.BeginJournal(j); err != nil {
		r.t.Fatal(err)
	}
}

func (r *recoveryFixture) inst(name string) *fleet.Instance {
	r.t.Helper()
	st, err := state.Load()
	if err != nil {
		r.t.Fatal(err)
	}
	return findInstance(st, "alpha", name)
}

func (r *recoveryFixture) awaitStatus(name string, want fleet.InstanceStatus) *fleet.Instance {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		inst := r.inst(name)
		if inst != nil && inst.Status == want {
			return inst
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s is %+v, want %s", name, inst, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func journalExists(key string) bool {
	_, err := os.Stat(kubernetes.JournalPath(key))
	return err == nil
}

// startRecovery runs the reconciler's start (the serve loop's) until the
// test ends.
func (r *recoveryFixture) startRecovery() {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	r.svc.bgCtx = ctx
	r.svc.kubeRecovery.start(ctx)
}

// At start, before any cluster is reached: interrupted creates and clones
// fail with kubernetes:interrupted naming their step (a create's journal is
// done with; a clone's waits for its cleanup), other backends' records with
// the plain error, and an interrupted rebuild is claimed and stays
// REBUILDING for its undo.
func TestFailInterruptedKubernetesJobs(t *testing.T) {
	r := newRecoveryFixture(t)
	r.record("made", "aaaaaaaa", fleet.StatusCreating, "")
	r.journal("made", "aaaaaaaa", kubernetes.OpCreate, "seed", nil)
	r.record("early", "bbbbbbbb", fleet.StatusCreating, "")
	r.record("copy", "cccccccc", fleet.StatusCloning, "")
	r.journal("copy", "cccccccc", kubernetes.OpClone, "copy", nil)
	r.record("rebuilt", "dddddddd", fleet.StatusRebuilding, "")
	r.journal("rebuilt", "dddddddd", kubernetes.OpRebuild, "seed", nil)
	// A journal that names another connection is not this record's.
	r.record("odd", "eeeeeeee", fleet.StatusRebuilding, "")
	r.journal("odd", "eeeeeeee", kubernetes.OpRebuild, "seed", func(j *kubernetes.Journal) { j.Cluster = "elsewhere" })
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"].Instances = append(st.Fleets["alpha"].Instances, &fleet.Instance{Name: "docker", Status: fleet.StatusCreating})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	r.svc.failInterruptedJobs()

	for name, want := range map[string]string{
		"made":  "kubernetes:interrupted: interrupted by daemon restart during create (at step seed); it was not resumed",
		"early": "kubernetes:interrupted: interrupted by daemon restart during create; it was not resumed",
		"copy":  "kubernetes:interrupted: interrupted by daemon restart during clone (at step copy); it was not resumed",
		"odd":   "kubernetes:interrupted: interrupted by daemon restart during rebuild; it was not resumed",
	} {
		inst := r.inst(name)
		if inst.Status != fleet.StatusFailed || !strings.HasPrefix(inst.Error, want) {
			t.Errorf("%s = %s %q, want failed %q", name, inst.Status, inst.Error, want)
		}
	}
	if d := r.inst("docker"); d.Status != fleet.StatusFailed || d.Error != interruptedByRestart {
		t.Errorf("devcontainer record = %+v", d)
	}
	if rb := r.inst("rebuilt"); rb.Status != fleet.StatusRebuilding {
		t.Errorf("the claimed rebuild was failed: %+v", rb)
	}
	if journalExists("aaaaaaaa") || !journalExists("cccccccc") || !journalExists("dddddddd") {
		t.Error("journals after start: the create's is done with, the clone's and rebuild's are kept")
	}
	if !r.svc.kubeRecovery.claims("alpha", "rebuilt") || r.svc.kubeRecovery.claims("alpha", "odd") {
		t.Error("claims")
	}
}

// The undo of an interrupted rebuild waits for the cluster, then runs as a
// rebuild job: one undone before its reset goes back to its status; one
// rolled back past its reset fails rebuild_failed at the old generation.
func TestUndoInterruptedRebuild(t *testing.T) {
	for _, tc := range []struct {
		name      string
		undo      func(kubernetes.Journal) (kubernetes.RebuildUndo, error)
		status    fleet.InstanceStatus
		errPrefix string
		journal   bool
	}{
		{"before its reset", nil, fleet.StatusRunning, "", false},
		{"past its reset", func(kubernetes.Journal) (kubernetes.RebuildUndo, error) {
			return kubernetes.RebuildUndo{RolledBack: true}, nil
		}, fleet.StatusFailed, "kubernetes:rebuild_failed: the rebuild to generation 3 was interrupted by a daemon restart; generation 2 was restored", false},
		{"stale journal", func(kubernetes.Journal) (kubernetes.RebuildUndo, error) {
			return kubernetes.RebuildUndo{}, &kubernetes.StaleJournalError{Why: "the job finished"}
		}, fleet.StatusFailed, interruptedByRestart, false},
		{"undo keeps failing", func(kubernetes.Journal) (kubernetes.RebuildUndo, error) {
			return kubernetes.RebuildUndo{}, errors.New("rollback Job failed: no space left on device")
		}, fleet.StatusFailed, "kubernetes:rebuild_failed: undoing the rebuild a daemon restart interrupted failed: rollback Job failed: no space left on device", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecoveryFixture(t)
			r.pool.undo = tc.undo
			r.record("rb", "dddddddd", fleet.StatusRebuilding, "")
			r.journal("rb", "dddddddd", kubernetes.OpRebuild, "seed", func(j *kubernetes.Journal) {
				j.Gen, j.PrevStatus, j.PrevError = 3, fleet.StatusRunning, ""
			})
			r.svc.failInterruptedJobs()
			r.startRecovery()

			// No cluster judged yet: the undo waits, the record stays.
			time.Sleep(100 * time.Millisecond)
			if r.pool.undoCount() != 0 || r.inst("rb").Status != fleet.StatusRebuilding {
				t.Fatalf("the undo did not wait for its cluster: %d undos, %+v", r.pool.undoCount(), r.inst("rb"))
			}
			jobs := r.svc.jobs.activeJobs().GetJobs()
			if len(jobs) != 1 || jobs[0].GetKind() != fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE {
				t.Fatalf("active jobs %v", jobs)
			}
			undoJob := r.svc.jobs.get(jobs[0].GetJobId())
			r.start()
			inst := r.awaitStatus("rb", tc.status)
			if !strings.HasPrefix(inst.Error, tc.errPrefix) {
				t.Fatalf("error %q, want %q", inst.Error, tc.errPrefix)
			}
			select {
			case <-undoJob.exited:
			case <-time.After(10 * time.Second):
				t.Fatal("the undo job did not end")
			}
			// The rebuild did not happen, whatever the undo did.
			if done := undoJob.outcome(); done == nil || done.GetSuccess() || done.GetError() == "" {
				t.Fatalf("undo job outcome %v", done)
			}
			if journalExists("dddddddd") != tc.journal {
				t.Fatalf("journal kept = %v, want %v", !tc.journal, tc.journal)
			}
			if tc.journal {
				// The next start claims it again and tries once more.
				r.pool.mu.Lock()
				r.pool.undo = nil
				r.pool.mu.Unlock()
				r.svc.kubeRecovery.load()
				r.svc.failInterruptedJobs()
				r.svc.kubeRecovery.start(r.svc.bgCtx)
				r.awaitStatus("rb", fleet.StatusRunning)
				if journalExists("dddddddd") {
					t.Fatal("journal kept after the undo")
				}
			}
		})
	}
}

// A destroy cancels an undo still waiting for its cluster.
func TestDestroyCancelsAWaitingUndo(t *testing.T) {
	r := newRecoveryFixture(t)
	oldDown := jobDownInstance
	jobDownInstance = func(*fleet.Instance) error { return nil }
	t.Cleanup(func() { jobDownInstance = oldDown })
	r.record("rb", "dddddddd", fleet.StatusRebuilding, "")
	r.journal("rb", "dddddddd", kubernetes.OpRebuild, "seed", nil)
	r.svc.failInterruptedJobs()
	r.startRecovery()
	j, err := r.svc.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: ptrTo("rb")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-j.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy waited on the undo")
	}
	if r.inst("rb") != nil || r.pool.undoCount() != 0 {
		t.Fatalf("after the destroy: %+v, %d undos", r.inst("rb"), r.pool.undoCount())
	}
}

func ptrTo[T any](v T) *T { return &v }

// markedObj gives obj the fleet.dev/op mark.
func markedObj[T interface{ SetAnnotations(map[string]string) }](obj T, mark string) T {
	obj.SetAnnotations(map[string]string{names.AnnotationOp: mark})
	return obj
}

func objMeta(name, cluster, key, uid string) metav1.ObjectMeta {
	labels := names.Base(testIdentity.Owner(), cluster)
	labels[names.LabelKey] = key
	return metav1.ObjectMeta{Name: name, Namespace: "fleet", Labels: labels, UID: types.UID(uid)}
}

// The background pass, once the cluster is usable: a journal no record
// holds loses its objects (as the journal knows them); one whose record
// outlived it is discarded; an interrupted clone loses its copiers; a
// destroyed instance's key loses whatever is left, its tombstone dropped
// once nothing is; a tombstone of a key a record holds again is forgotten.
func TestReconcilerPass(t *testing.T) {
	r := newRecoveryFixture(t)
	cs := r.apis["minikube"]
	ctx := context.Background()
	createObjects(t, cs,
		markedObj(&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-gone-gggggggg", r.id, "gggggggg", "u-g1")}, "create@seed"),
		&corev1.Secret{ObjectMeta: objMeta("fi-alpha-gone-gggggggg-spec-g1", r.id, "gggggggg", "u-g2")},
		// The key's, but no journal names it: left to `fleet cluster gc`.
		&corev1.Secret{ObjectMeta: objMeta("fi-alpha-gone-gggggggg-env-g1", r.id, "gggggggg", "u-g3")},
		// A journal no record holds whose job finished (a restored one).
		&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-later-llllllll", r.id, "llllllll", "u-l1")},
		// A journal a job of this daemon's still writes.
		markedObj(&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-busy-uuuuuuuu", r.id, "uuuuuuuu", "u-u1")}, "create@seed"),
		&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-ran-rrrrrrrr", r.id, "rrrrrrrr", "u-r1")},
		&appsv1.StatefulSet{ObjectMeta: objMeta("fi-alpha-dead-tttttttt", r.id, "tttttttt", "u-t1")},
		&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-dead-tttttttt", r.id, "tttttttt", "u-t2")},
		&corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-back-bbbbbbbb", r.id, "bbbbbbbb", "u-b1")},
	)
	r.journal("gone", "gggggggg", kubernetes.OpCreate, "seed", func(j *kubernetes.Journal) {
		j.Objects = []kubernetes.JournalObject{
			{Kind: kubernetes.KindPVC, Name: "fi-alpha-gone-gggggggg", UID: "u-g1", Created: true, Anchor: true},
			{Kind: kubernetes.KindSecret, Name: "fi-alpha-gone-gggggggg-spec-g1", UID: "u-g2", Created: true},
		}
	})
	r.journal("later", "llllllll", kubernetes.OpCreate, "seed", func(j *kubernetes.Journal) {
		j.Objects = []kubernetes.JournalObject{{Kind: kubernetes.KindPVC, Name: "fi-alpha-later-llllllll", UID: "u-l1", Created: true, Anchor: true}}
	})
	r.journal("busy", "uuuuuuuu", kubernetes.OpCreate, "seed", func(j *kubernetes.Journal) {
		j.Objects = []kubernetes.JournalObject{{Kind: kubernetes.KindPVC, Name: "fi-alpha-busy-uuuuuuuu", UID: "u-u1", Created: true, Anchor: true}}
	})
	busy := r.svc.jobs.start(context.Background(), fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "busy", time.Now())
	t.Cleanup(func() { r.svc.jobs.finish(busy.summary.GetJobId()) })
	r.record("ran", "rrrrrrrr", fleet.StatusRunning, "")
	r.journal("ran", "rrrrrrrr", kubernetes.OpCreate, "finish", func(j *kubernetes.Journal) {
		j.Objects = []kubernetes.JournalObject{{Kind: kubernetes.KindPVC, Name: "fi-alpha-ran-rrrrrrrr", UID: "u-r1", Created: true, Anchor: true}}
	})
	r.record("copy", "cccccccc", fleet.StatusFailed, "kubernetes:interrupted: ...")
	r.journal("copy", "cccccccc", kubernetes.OpClone, "copy", nil)
	r.record("back", "bbbbbbbb", fleet.StatusStopped, "")
	if err := kubernetes.AddTombstone(kubernetes.Tombstone{Cluster: r.id, Key: "tttttttt", Fleet: "alpha", Instance: "dead"}); err != nil {
		t.Fatal(err)
	}
	if err := kubernetes.AddTombstone(kubernetes.Tombstone{Cluster: r.id, Key: "bbbbbbbb", Fleet: "alpha", Instance: "back"}); err != nil {
		t.Fatal(err)
	}

	r.svc.failInterruptedJobs()
	r.startRecovery()
	r.start() // the worker's first usable judgement is the REACHABLE edge

	gone := func(kind, name string) bool {
		var err error
		switch kind {
		case "pvc":
			_, err = cs.CoreV1().PersistentVolumeClaims("fleet").Get(ctx, name, metav1.GetOptions{})
		case "sts":
			_, err = cs.AppsV1().StatefulSets("fleet").Get(ctx, name, metav1.GetOptions{})
		case "secret":
			_, err = cs.CoreV1().Secrets("fleet").Get(ctx, name, metav1.GetOptions{})
		}
		return err != nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for !(gone("pvc", "fi-alpha-gone-gggggggg") && gone("secret", "fi-alpha-gone-gggggggg-spec-g1") && gone("sts", "fi-alpha-dead-tttttttt") && gone("pvc", "fi-alpha-dead-tttttttt")) {
		if time.Now().After(deadline) {
			t.Fatal("the pass deleted no garbage")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gone("secret", "fi-alpha-gone-gggggggg-env-g1") || gone("pvc", "fi-alpha-later-llllllll") || gone("pvc", "fi-alpha-busy-uuuuuuuu") || !journalExists("uuuuuuuu") || gone("pvc", "fi-alpha-ran-rrrrrrrr") || gone("pvc", "fi-alpha-back-bbbbbbbb") {
		t.Fatal("the pass deleted what is not known garbage")
	}
	if journalExists("gggggggg") || journalExists("rrrrrrrr") || journalExists("llllllll") {
		t.Error("garbage and outlived journals are left")
	}
	deadline = time.Now().Add(10 * time.Second)
	for journalExists("cccccccc") {
		if time.Now().After(deadline) {
			t.Fatal("the interrupted clone's journal is left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.pool.mu.Lock()
	clones := len(r.pool.clones)
	r.pool.mu.Unlock()
	if clones != 1 {
		t.Fatalf("%d clone undos", clones)
	}
	fp, _ := kubernetes.LoadFootprint()
	if ts := fp.TombstonesOf(r.id); len(ts) != 1 || ts[0].Key != "tttttttt" {
		t.Fatalf("tombstones after the first pass: %+v", ts)
	}
	if n := r.svc.kubeRecovery.pendingTeardowns(r.id); n != 1 {
		t.Fatalf("pending teardowns %d", n)
	}
	if st := r.svc.kube.status(r.id); st.GetCapabilities().GetPendingTeardowns() != 1 {
		t.Fatalf("status pending_teardowns %d", st.GetCapabilities().GetPendingTeardowns())
	}

	// The next pass finds nothing of the key left.
	r.svc.kubeRecovery.reachable(r.id)
	deadline = time.Now().Add(10 * time.Second)
	for {
		fp, _ := kubernetes.LoadFootprint()
		if len(fp.TombstonesOf(r.id)) == 0 && r.svc.kubeRecovery.pendingTeardowns(r.id) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tombstones %+v", fp.Tombstones)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without the owner Lease the pass deletes nothing, and says what waits.
func TestReconcilerPassWithoutTheLease(t *testing.T) {
	r := newRecoveryFixture(t)
	cs := r.apis["minikube"]
	createObjects(t, cs, &appsv1.StatefulSet{ObjectMeta: objMeta("fi-alpha-dead-tttttttt", r.id, "tttttttt", "u-t1")})
	if err := kubernetes.AddTombstone(kubernetes.Tombstone{Cluster: r.id, Key: "tttttttt"}); err != nil {
		t.Fatal(err)
	}
	r.startRecovery()
	r.pool.SetOwnerGate(func(context.Context, string) error { return kubernetes.ErrOwnerConflict })
	r.start()
	deadline := time.Now().Add(10 * time.Second)
	for r.svc.kubeRecovery.pendingTeardowns(r.id) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("no pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := cs.AppsV1().StatefulSets("fleet").Get(context.Background(), "fi-alpha-dead-tttttttt", metav1.GetOptions{}); err != nil {
		t.Fatal("deleted without the Lease")
	}
}

// The owner check passes while the worker holds the Lease, and is refused
// for a connection nobody checked.
func TestKubeOwnerGate(t *testing.T) {
	r := newRecoveryFixture(t)
	if err := r.svc.kube.ownerGate(context.Background(), r.id); !errors.Is(err, kubernetes.ErrOwnerConflict) {
		t.Fatalf("before any check: %v", err)
	}
	r.start()
	r.awaitState(r.id, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	if err := r.svc.kube.ownerGate(context.Background(), r.id); err != nil {
		t.Fatalf("with the Lease held: %v", err)
	}
	if err := r.svc.kube.ownerGate(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown connection passed")
	}
}

// A real Serve: an interrupted Kubernetes create is failed with its code
// before the first RPC; an interrupted rebuild on a connection that is gone
// is undone by no one and fails saying so; neither journal is left.
func TestServeReconcilesKubernetesJobs(t *testing.T) {
	client, served := startServeSeeded(t, func() {
		st := &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "made", Status: fleet.StatusCreating, Backend: fleet.BackendKubernetes, KubeCluster: "gone-c1", KubeKey: "aaaaaaaa"},
			{Name: "rb", Status: fleet.StatusRebuilding, Backend: fleet.BackendKubernetes, KubeCluster: "gone-c1", KubeKey: "bbbbbbbb"},
		}}}}
		if err := state.Save(st); err != nil {
			t.Fatal(err)
		}
		for _, j := range []kubernetes.Journal{
			{Op: kubernetes.OpCreate, Fleet: "alpha", Instance: "made", Cluster: "gone-c1", Key: "aaaaaaaa", Gen: 1, Step: "seed"},
			{Op: kubernetes.OpRebuild, Fleet: "alpha", Instance: "rb", Cluster: "gone-c1", Key: "bbbbbbbb", Gen: 2, Step: "seed"},
		} {
			if _, err := kubernetes.BeginJournal(j); err != nil {
				t.Fatal(err)
			}
		}
	})
	reply, err := client.GetState(t.Context(), &fleetgrpc.GetStateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range reply.GetState().GetFleets()["alpha"].GetInstances() {
		if inst.GetName() == "made" && (inst.GetStatus() != fleetgrpc.InstanceStatus_INSTANCE_STATUS_FAILED || !strings.HasPrefix(inst.GetError(), "kubernetes:interrupted: interrupted by daemon restart during create (at step seed)")) {
			t.Errorf("create = %v %q", inst.GetStatus(), inst.GetError())
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := state.Load()
		if rb := findInstance(st, "alpha", "rb"); rb != nil && rb.Status == fleet.StatusFailed {
			if !strings.Contains(rb.Error, "its connection gone-c1 is gone") {
				t.Errorf("rebuild error %q", rb.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rebuild was never settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if journalExists("aaaaaaaa") || journalExists("bbbbbbbb") {
		t.Error("journals left")
	}
	shutdownAndWait(t, client, served)
	awaitFirstBackup(t)
}

// A destroy tombstones a Kubernetes record's key before its teardown, and
// the pass then deletes whatever the teardown left.
func TestDestroyTombstonesTheKey(t *testing.T) {
	r := newRecoveryFixture(t)
	old := kubePassFollowUp
	kubePassFollowUp = 20 * time.Millisecond
	t.Cleanup(func() { kubePassFollowUp = old })
	oldDown := jobDownInstance
	jobDownInstance = func(inst *fleet.Instance) error {
		// The backend's teardown failed: the cluster keeps the objects.
		if fp, _ := kubernetes.LoadFootprint(); len(fp.TombstonesOf(inst.KubeCluster)) != 1 {
			t.Error("the teardown ran before the key was tombstoned")
		}
		return errors.New("the cluster did not answer")
	}
	t.Cleanup(func() { jobDownInstance = oldDown })
	cs := r.apis["minikube"]
	createObjects(t, cs, &corev1.PersistentVolumeClaim{ObjectMeta: objMeta("fi-alpha-doomed-dddddddd", r.id, "dddddddd", "u-d1")})
	r.record("doomed", "dddddddd", fleet.StatusRunning, "")
	r.startRecovery()
	r.start()
	r.awaitState(r.id, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	j, err := r.svc.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: ptrTo("doomed")})
	if err != nil {
		t.Fatal(err)
	}
	<-j.exited
	if r.inst("doomed") != nil {
		t.Fatal("the record is left")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := cs.CoreV1().PersistentVolumeClaims("fleet").Get(context.Background(), "fi-alpha-doomed-dddddddd", metav1.GetOptions{})
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the destroyed instance's disk is left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The pass that deleted something looks again soon, and then drops the
	// tombstone of a key with nothing left.
	for {
		fp, _ := kubernetes.LoadFootprint()
		if len(fp.TombstonesOf(r.id)) == 0 && r.svc.kubeRecovery.pendingTeardowns(r.id) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tombstones %+v", fp.Tombstones)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An undo whose connection is removed while it waits fails saying so.
func TestUndoWhoseConnectionGoes(t *testing.T) {
	r := newRecoveryFixture(t)
	r.record("rb", "dddddddd", fleet.StatusRebuilding, "")
	r.journal("rb", "dddddddd", kubernetes.OpRebuild, "seed", nil)
	r.svc.failInterruptedJobs()
	r.startRecovery()
	time.Sleep(50 * time.Millisecond)
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Remove(r.id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	inst := r.awaitStatus("rb", fleet.StatusFailed)
	if !strings.Contains(inst.Error, "its connection "+r.id+" is gone") || r.pool.undoCount() != 0 || journalExists("dddddddd") {
		t.Fatalf("after the connection went: %+v, %d undos", inst, r.pool.undoCount())
	}
}
