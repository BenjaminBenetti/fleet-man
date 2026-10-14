package backendtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// FleetName is the fleet every conformance instance belongs to.
const FleetName = "conformance"

// Teardown bounds: how long one teardown may take, and how long the backend
// has to report the instance missing and hold nothing for it (a cluster
// deletes asynchronously).
const (
	teardownTimeout = 10 * time.Minute
	goneTimeout     = 2 * time.Minute
)

// Run runs every case in Cases against target, each as a subtest named
// <kind>/<name> (method/exec-tar, scenario/clone), one after another. The
// method cases share one instance, created by the first case that needs it and
// torn down after the last; each scenario creates and tears down its own.
func Run(t *testing.T, target Target) {
	t.Helper()
	s := newSuite(t, &target)
	for _, tc := range Cases() {
		t.Run(tc.Kind.String()+"/"+tc.Name, func(t *testing.T) { s.runCase(t, tc) })
	}
}

// suite is one Run: the target, the workspaces it creates and the shared
// method instance.
type suite struct {
	t      TB
	target *Target
	root   string

	// patience scales every wait for the backend to catch up (1 = the
	// suite's own bounds). The suite's tests shrink it, so a case against a
	// deliberately broken backend fails in seconds.
	patience float64

	mu          sync.Mutex
	seq         int
	shared      *Instance
	sharedErr   error
	sharedTried bool
}

func newSuite(t TB, target *Target) *suite {
	t.Helper()
	if target.Bind == nil || target.Leftovers == nil {
		t.Fatalf("conformance target %q needs Bind and Leftovers", target.Name)
	}
	// Not t.TempDir: an instance may leave files the test user cannot remove
	// (a container's root wrote them), which would fail the run at cleanup.
	root, err := os.MkdirTemp("", "fleet-conformance-")
	if err != nil {
		t.Fatalf("workspace root: %v", err)
	}
	s := &suite{t: t, target: target, root: root, patience: 1}
	t.Cleanup(func() {
		s.teardownShared()
		if err := os.RemoveAll(root); err != nil {
			t.Logf("leaving %s: %v", root, err)
		}
	})
	return s
}

// runCase runs tc against the target with t as the case's test.
func (s *suite) runCase(t TB, tc Case) {
	if reason, ok := s.target.Skip[tc.Name]; ok {
		t.Skipf("%s cannot run this case yet: %s", s.target.Name, reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.target.caseTimeout())
	defer cancel()
	tc.Run(&C{TB: t, Ctx: ctx, suite: s, name: tc.Name})
}

// C is what a case runs with: its test, a context bounding it, and the suite's
// instances.
type C struct {
	TB
	// Ctx ends when the case's time is up.
	Ctx context.Context

	suite *suite
	name  string
}

// Target is the backend the case runs against.
func (c *C) Target() *Target { return c.suite.target }

// Sizes are the target's bulk transfer sizes.
func (c *C) Sizes() Sizes { return c.suite.target.sizes() }

// Shared returns the method cases' instance, creating it on first use. Every
// case asking after a failed create fails with that create's error.
func (c *C) Shared() *Instance {
	c.Helper()
	s := c.suite
	s.mu.Lock()
	if !s.sharedTried {
		s.sharedTried = true
		s.mu.Unlock()
		inst, err := s.provision(c.Ctx, Fixture{})
		s.mu.Lock()
		s.shared, s.sharedErr = inst, err
	}
	inst, err := s.shared, s.sharedErr
	s.mu.Unlock()
	if err != nil {
		c.Fatalf("the shared method instance could not be created: %v", err)
	}
	return inst
}

// NewInstance creates an instance of the case's own from fx, failing the case
// if the create fails. It is torn down when the case ends.
func (c *C) NewInstance(fx Fixture) *Instance {
	c.Helper()
	inst, err := c.Provision(fx)
	if err != nil {
		c.Fatalf("create: %v", err)
	}
	return inst
}

// Provision creates an instance of the case's own from fx and returns the
// create's error instead of failing on it. The instance comes back either way
// (with no ContainerID after a failed create, as the daemon records one) and
// is torn down when the case ends.
func (c *C) Provision(fx Fixture) (*Instance, error) {
	c.Helper()
	inst, err := c.suite.provision(c.Ctx, fx)
	if inst != nil {
		c.Cleanup(func() { c.suite.teardown(c, inst) })
	}
	return inst, err
}

// Teardown tears inst down now, the way the daemon does, and checks the
// backend then holds nothing for it. The case's cleanup does it otherwise.
func (c *C) Teardown(inst *Instance) {
	c.Helper()
	c.suite.teardown(c, inst)
}

// Instance is one instance the suite created: its record, as the daemon would
// persist it, and the backend bound to that record.
type Instance struct {
	// Record is the instance as state.json would hold it.
	Record *fleet.Instance
	// Result is what the last Up, Rebuild or Clone returned.
	Result *backend.UpResult
	// B is the backend bound to Record.
	B backend.Backend

	target   *Target
	tornDown bool
}

// ID is the instance's ContainerID.
func (i *Instance) ID() string { return i.Record.ContainerID }

// WorkspaceDir is the instance's host workspace folder.
func (i *Instance) WorkspaceDir() string { return i.Record.WorkspaceDir }

// RemoteWorkspace is the workspace folder inside the instance.
func (i *Instance) RemoteWorkspace() string {
	if i.Result == nil {
		return ""
	}
	return i.Result.RemoteWorkspaceFolder
}

// Rebind binds a fresh backend to Record, as the daemon does for each
// operation.
func (i *Instance) Rebind() { i.B = i.target.Bind(i.Record) }

// resultWarnings are the warnings the instance's last Up, Rebuild or Clone
// returned.
func (i *Instance) resultWarnings() []string {
	if i.Result == nil {
		return nil
	}
	return i.Result.Warnings
}

// SetResult records an Up, Rebuild or Clone result: the record takes its
// ContainerID and the backend is bound again.
func (i *Instance) SetResult(r *backend.UpResult) {
	i.Result = r
	i.Record.ContainerID = r.ContainerID
	i.Record.Status = fleet.StatusRunning
	i.Rebind()
}

// newRecord makes a CREATING record for a new instance, its workspace laid out
// the way the daemon lays one out: <workspaces>/<fleet>/<instance>/<repo>.
func (s *suite) newRecord(prefix string) *fleet.Instance {
	s.mu.Lock()
	s.seq++
	name := fmt.Sprintf("%s-%d-%s", prefix, s.seq, randHex(3))
	s.mu.Unlock()
	return &fleet.Instance{
		Name:         name,
		WorkspaceDir: filepath.Join(s.root, "workspaces", FleetName, name, FixtureRepo),
		CreatedAt:    time.Now(),
		Status:       fleet.StatusCreating,
		Backend:      s.target.Type,
	}
}

// provision creates an instance from fx through UpContext, as a create job
// does. On a failed create it returns the instance (no ContainerID) and the
// error, with the tail of the provisioning log.
func (s *suite) provision(ctx context.Context, fx Fixture) (*Instance, error) {
	rec := s.newRecord("conf")
	if err := fx.Write(rec.WorkspaceDir); err != nil {
		return nil, fmt.Errorf("lay out the fixture: %w", err)
	}
	inst := &Instance{Record: rec, target: s.target}
	inst.Rebind()
	log := &lockedBuffer{}
	result, err := backend.UpContext(backend.WithJobLog(ctx, log), inst.B, backend.UpRequest{WorkspaceDir: rec.WorkspaceDir})
	if err != nil {
		rec.Status = fleet.StatusFailed
		return inst, fmt.Errorf("%w%s", err, log.tail(40))
	}
	if result == nil || result.ContainerID == "" {
		rec.Status = fleet.StatusFailed
		return inst, errors.New("up succeeded without a ContainerID")
	}
	inst.SetResult(result)
	return inst, nil
}

// teardownShared tears the shared instance down after the last case.
func (s *suite) teardownShared() {
	s.mu.Lock()
	inst := s.shared
	s.mu.Unlock()
	if inst != nil {
		s.teardown(s.t, inst)
	}
}

// teardown removes inst the way the daemon's destroy does (Down by
// ContainerID; with none, OrphanDowner.DownByKey when the backend has it), then
// requires the backend to report the container missing and to hold nothing for
// the instance. A second teardown of the same instance does nothing.
func (s *suite) teardown(t TB, inst *Instance) {
	t.Helper()
	if inst.tornDown {
		return
	}
	inst.tornDown = true
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()

	b := s.target.Bind(inst.Record)
	id := inst.Record.ContainerID
	var err error
	switch {
	case id != "":
		err = b.Down(id)
	default:
		if od, ok := b.(backend.OrphanDowner); ok {
			err = od.DownByKey()
		}
	}
	if err != nil {
		t.Errorf("teardown %s: %v", inst.Record.Name, err)
	}
	if id != "" {
		var last backend.LiveStatus
		if !poll(ctx, s.within(goneTimeout), func() bool {
			last = b.Status(id)
			return last == backend.LiveStatusMissing
		}) {
			t.Errorf("teardown %s: Status(%s) after Down = %q, want %q", inst.Record.Name, id, last, backend.LiveStatusMissing)
		}
	}
	var held []string
	var heldErr error
	if !poll(ctx, s.within(goneTimeout), func() bool {
		held, heldErr = s.target.Leftovers(inst.Record)
		return heldErr == nil && len(held) == 0
	}) {
		if heldErr != nil {
			t.Errorf("teardown %s: list leftovers: %v", inst.Record.Name, heldErr)
		} else {
			t.Errorf("teardown %s: the backend still holds %s", inst.Record.Name, strings.Join(held, ", "))
		}
	}
	// The workspace's instance folder (<fleet>/<instance>), as destroy does.
	if err := os.RemoveAll(filepath.Dir(inst.Record.WorkspaceDir)); err != nil {
		t.Logf("teardown %s: leaving its workspace: %v", inst.Record.Name, err)
	}
}

// within scales a wait bound by the suite's patience, never below a second
// (or d, when that is shorter): a backend gets a moment however impatient the
// run.
func (s *suite) within(d time.Duration) time.Duration {
	return max(time.Duration(float64(d)*s.patience), min(d, time.Second))
}

// poll calls cond until it holds, ctx ends or timeout passes (at least twice,
// about ten times over the timeout but no more than once a second), and
// reports whether it held.
func poll(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	interval := min(time.Second, max(10*time.Millisecond, timeout/10))
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// randHex returns n random bytes in hex.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// lockedBuffer is a provisioning-log sink safe for the concurrent writes
// WithJobLog promises.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// tail is the last n lines written, as a block to append to an error ("" when
// nothing was written).
func (b *lockedBuffer) tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := strings.Split(strings.TrimRight(b.buf.String(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "\nprovisioning log (tail):\n" + strings.Join(lines, "\n")
}
