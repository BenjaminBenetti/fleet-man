package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/protobuf/proto"
)

// newTestJob registers a create job for alpha/i1 on m.
func newTestJob(m *jobManager) *job {
	return m.start(context.Background(), fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "i1", time.Now())
}

// jobEvents returns the job's history.
func jobEvents(j *job) []*fleetgrpc.JobEvent {
	hist, ch := j.subscribe()
	if ch != nil {
		j.unsubscribe(ch)
	}
	return hist
}

func readCreationLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(state.InstanceCreationLogPath("alpha", "i1"))
	if err != nil {
		t.Fatalf("read creation log: %v", err)
	}
	return string(data)
}

// TestProvisioningLogWritesTheFileAndTheEvents drives a log the way a create
// run does and checks the creation log and the job's events agree, line for
// line, with -e values redacted in both.
func TestProvisioningLogWritesTheFileAndTheEvents(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	ctx := backend.WithJobLog(context.Background(), l)

	backend.ReportStep(ctx, fleet.StepGitClone, "branch dev")
	_, _ = io.WriteString(backend.JobLogFrom(ctx), "Cloning into '/ws'...\npartial ")
	_, _ = io.WriteString(backend.JobLogFrom(ctx), "line\nRun: docker run -e API_KEY=sk-secret -e B=2 --entrypoint /bin/sh img\n")
	_, _ = io.WriteString(backend.JobLogFrom(ctx), "progress 10%\rprogress 50%\rprogress 100%\r\n")
	backend.ReportStep(ctx, fleet.StepBackendUp, "devcontainer")
	_, _ = io.WriteString(backend.JobLogFrom(ctx), "no newline before the warning")
	backend.ReportWarning(ctx, "deb cache: boom\nsecond -e TOKEN=hunter2 line")
	l.finish(nil)
	_, _ = io.WriteString(l, "after the end\n")
	backend.ReportStep(ctx, fleet.StepMarkRunning, "")

	file := readCreationLog(t)
	for _, secret := range []string{"sk-secret", "hunter2"} {
		if strings.Contains(file, secret) {
			t.Fatalf("creation log leaks %q:\n%s", secret, file)
		}
	}
	lines := strings.Split(strings.TrimSuffix(file, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "==> fleet create: alpha/i1 at ") {
		t.Errorf("header = %q", lines[0])
	}
	wantRest := []string{
		"==> cloning repository: branch dev",
		"Cloning into '/ws'...",
		"partial line",
		"Run: docker run -e API_KEY=<redacted> -e B=<redacted> --entrypoint /bin/sh img",
		"progress 100%",
		"==> starting container: devcontainer",
		"no newline before the warning",
		"warning: deb cache: boom",
		"warning: second -e TOKEN=<redacted>",
		"==> done",
	}
	if got := lines[1:]; strings.Join(got, "\n") != strings.Join(wantRest, "\n") {
		t.Fatalf("creation log lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantRest, "\n"))
	}

	// The same lines as events: steps as JobProgress, the rest as JobLog.
	var got []string
	for _, ev := range jobEvents(j) {
		switch {
		case ev.GetLog() != nil:
			got = append(got, fmt.Sprintf("%s %s", ev.GetLog().GetLevel(), ev.GetLog().GetLine()))
			if ev.GetLog().GetAt() == nil {
				t.Errorf("log event without a time: %v", ev)
			}
		case ev.GetProgress() != nil:
			p := ev.GetProgress()
			got = append(got, fmt.Sprintf("%s %s done=%v", p.GetStep(), p.GetMessage(), p.GetStepDone()))
		}
		if ev.GetJobId() != j.summary.GetJobId() {
			t.Errorf("event carries job id %q, want %q", ev.GetJobId(), j.summary.GetJobId())
		}
	}
	want := []string{
		"JOB_STEP_GIT_CLONE cloning repository: branch dev done=false",
		"LOG_LEVEL_INFO Cloning into '/ws'...",
		"LOG_LEVEL_INFO partial line",
		"LOG_LEVEL_INFO Run: docker run -e API_KEY=<redacted> -e B=<redacted> --entrypoint /bin/sh img",
		"LOG_LEVEL_INFO progress 100%",
		"JOB_STEP_BACKEND_UP starting container: devcontainer done=false",
		"LOG_LEVEL_INFO no newline before the warning",
		"LOG_LEVEL_WARN deb cache: boom",
		"LOG_LEVEL_WARN second -e TOKEN=<redacted>",
		"JOB_STEP_DONE done done=true",
	}
	// The header is the first log event.
	if len(got) == 0 || !strings.HasPrefix(got[0], "LOG_LEVEL_INFO ==> fleet create: alpha/i1") {
		t.Fatalf("first event = %v, want the header", got)
	}
	if strings.Join(got[1:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got[1:], "\n"), strings.Join(want, "\n"))
	}
	if step := j.currentStep(); step != fleetgrpc.JobStep_JOB_STEP_DONE {
		t.Fatalf("current step = %v, want DONE", step)
	}
}

func TestProvisioningLogRecordsTheFailure(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")

	l.finish(errors.New("devcontainer up failed: exit status 1\nError: -e K=leak boom"))

	file := readCreationLog(t)
	if !strings.HasSuffix(file, "error: devcontainer up failed: exit status 1\nerror: Error: -e K=<redacted>\n") {
		t.Fatalf("creation log does not end with the redacted error:\n%s", file)
	}
	var errs int
	for _, ev := range jobEvents(j) {
		if ev.GetLog().GetLevel() == fleetgrpc.LogLevel_LOG_LEVEL_ERROR {
			errs++
		}
		if ev.GetProgress().GetStep() == fleetgrpc.JobStep_JOB_STEP_DONE {
			t.Fatal("a failed job reported DONE")
		}
	}
	if errs != 2 {
		t.Fatalf("got %d ERROR events, want one per error line", errs)
	}
}

// TestProvisioningLogFileIsPrivateAndReplaced: the creation log is 0600, and a
// new provisioning job starts a fresh file that a still-open old one cannot
// write into.
func TestProvisioningLogFileIsPrivateAndReplaced(t *testing.T) {
	isolateFleetDir(t)
	path := state.InstanceCreationLogPath("alpha", "i1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale log of a destroyed namesake\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newJobManager()
	old := openProvisioningLog(newTestJob(m), "alpha", "i1", "create")
	fresh := openProvisioningLog(newTestJob(m), "alpha", "i1", "rebuild")
	_, _ = io.WriteString(old, "late output of the old run\n")
	_, _ = io.WriteString(fresh, "new run output\n")
	old.finish(nil)
	fresh.finish(nil)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("creation log mode = %o, want 600", mode)
	}
	file := readCreationLog(t)
	if strings.Contains(file, "stale") || strings.Contains(file, "old run") {
		t.Fatalf("creation log mixes in another run:\n%s", file)
	}
	if !strings.Contains(file, "==> fleet rebuild: alpha/i1") || !strings.Contains(file, "new run output") {
		t.Fatalf("creation log is not the new run's:\n%s", file)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// TestProvisioningLogWithoutAFile: when the log directory cannot be written
// the job's events still flow and writes never fail.
func TestProvisioningLogWithoutAFile(t *testing.T) {
	isolateFleetDir(t)
	// A file where the logs directory should be.
	if err := os.MkdirAll(state.FleetDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.FleetDir(), "logs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	if n, err := io.WriteString(l, "still an event\n"); err != nil || n != len("still an event\n") {
		t.Fatalf("Write = %d, %v; want the whole write accepted", n, err)
	}
	l.finish(nil)
	found := false
	for _, ev := range jobEvents(j) {
		if ev.GetLog().GetLine() == "still an event" {
			found = true
		}
	}
	if !found {
		t.Fatal("output did not reach the job's events")
	}
}

func TestProvisioningLogSplitsEndlessLines(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	_, _ = io.WriteString(l, strings.Repeat("x", 2*provisioningLogMaxLine+5))
	l.finish(nil)

	var sizes []int
	for _, ev := range jobEvents(j) {
		if line := ev.GetLog().GetLine(); strings.HasPrefix(line, "x") {
			sizes = append(sizes, len(line))
		}
	}
	if fmt.Sprint(sizes) != fmt.Sprint([]int{provisioningLogMaxLine, provisioningLogMaxLine, 5}) {
		t.Fatalf("line pieces = %v", sizes)
	}
}

// TestProvisioningLogRedactsALongLineWhole: a docker run line longer than one
// piece, arriving in two writes (as a pipe hands it over) with the piece
// boundary inside an env value, is redacted before it is cut, so no part of a
// value reaches the file or the events.
func TestProvisioningLogRedactsALongLineWhole(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	head := "Run: docker run "
	meta := ` -l devcontainer.metadata=[{"containerEnv":{"FIRST":"topsecret`
	pad := strings.Repeat("a", provisioningLogMaxLine-len(head)-len(meta))
	line := head + pad + meta + `value1","SECOND":"othersecret2"}}] -e TOKEN=hunter2hunter2 --entrypoint /bin/sh img` + "\n"
	_, _ = io.WriteString(l, line[:len(line)-30])
	_, _ = io.WriteString(l, line[len(line)-30:])
	l.finish(nil)

	file := readCreationLog(t)
	var events []string
	for _, ev := range jobEvents(j) {
		if line := ev.GetLog().GetLine(); line != "" {
			events = append(events, line)
			if len(line) > provisioningLogMaxLine {
				t.Errorf("event of %d bytes, want at most %d", len(line), provisioningLogMaxLine)
			}
		}
	}
	for _, secret := range []string{"value1", "othersecret2", "hunter2hunter2"} {
		if strings.Contains(file, secret) {
			t.Errorf("creation log leaks %q", secret)
		}
		if strings.Contains(strings.Join(events, "\n"), secret) {
			t.Errorf("job events leak %q", secret)
		}
	}
	if !strings.Contains(file, `"SECOND":"<redacted>"}}] -e TOKEN=<redacted> --entrypoint /bin/sh img`) {
		t.Fatalf("the end of the line is missing:\n%q", file[len(file)-300:])
	}
}

// TestProvisioningLogCutsAnOverlongLine: an unfinished line that outgrows
// provisioningLogMaxPending is passed on cut and the rest of it, which can't be
// redacted on its own, is dropped; a line redrawn with carriage returns keeps
// only its last state and is never cut.
func TestProvisioningLogCutsAnOverlongLine(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")

	_, _ = io.WriteString(l, "-e TOKEN=")
	for written := 0; written <= provisioningLogMaxPending; written += 4096 {
		_, _ = io.WriteString(l, strings.Repeat("s", 4096))
	}
	_, _ = io.WriteString(l, " -e OTHER=tail-secret\nnext line\n")
	for range provisioningLogMaxPending / 4096 * 2 {
		_, _ = io.WriteString(l, strings.Repeat("p", 4090)+" 100%\r")
	}
	_, _ = io.WriteString(l, "redrawn 100%\r\n")
	l.finish(nil)

	file := readCreationLog(t)
	if strings.Contains(file, "sss") || strings.Contains(file, "tail-secret") {
		t.Fatal("creation log leaks a value of the cut line")
	}
	lines := strings.Split(strings.TrimSuffix(file, "\n"), "\n")
	want := []string{"-e TOKEN=<redacted>" + provisioningLogCut, "next line", "redrawn 100%", "==> done"}
	if got := lines[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("creation log lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := len(l.partial); n != 0 {
		t.Fatalf("%d bytes still held", n)
	}
}

// TestProvisioningLogEventsAreValidUTF8: output that is not UTF-8, and a long
// line cut into pieces, still makes events the job stream can send (proto
// strings must be valid UTF-8, or Send fails and the client's stream breaks).
func TestProvisioningLogEventsAreValidUTF8(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	_, _ = io.WriteString(l, "latin-1 caf\xe9\n")
	_, _ = io.WriteString(l, "x"+strings.Repeat("é", provisioningLogMaxLine)+"\n")
	l.Step(fleet.StepCopyWorkspace, "template file:///tmp/caf\xe9")
	l.finish(errors.New("boom \xff"))

	var pieces int
	for _, ev := range jobEvents(j) {
		if _, err := proto.Marshal(ev); err != nil {
			t.Fatalf("event %v does not marshal: %v", ev, err)
		}
		if strings.HasPrefix(ev.GetLog().GetLine(), "x") || strings.HasPrefix(ev.GetLog().GetLine(), "é") {
			pieces++
		}
	}
	if pieces < 2 {
		t.Fatalf("the long line came out in %d pieces, want it cut", pieces)
	}
	if file := readCreationLog(t); !utf8.ValidString(file) || !strings.Contains(file, "latin-1 caf\uFFFD") {
		t.Fatalf("creation log is not valid UTF-8 or lost the line:\n%q", file[:200])
	}
}

// TestProvisioningLogTakesConcurrentWriters: commands tee stdout and stderr
// into the log from separate goroutines (run with -race).
func TestProvisioningLogTakesConcurrentWriters(t *testing.T) {
	isolateFleetDir(t)
	j := newTestJob(newJobManager())
	l := openProvisioningLog(j, "alpha", "i1", "create")
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				_, _ = fmt.Fprintf(l, "writer %d line %d\n", w, i)
				if i%10 == 0 {
					l.Step(fleet.StepBackendUp, "")
				}
			}
		}()
	}
	wg.Wait()
	l.finish(nil)
	if n := strings.Count(readCreationLog(t), "writer "); n != 200 {
		t.Fatalf("creation log holds %d writer lines, want 200", n)
	}
}

// TestJobHistoryKeepsTheLatestLogLines: a job's history keeps every progress
// event but only the most recent jobLogHistory log lines.
func TestJobHistoryKeepsTheLatestLogLines(t *testing.T) {
	j := newTestJob(newJobManager())
	emitLog := func(i int) {
		j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Log{Log: &fleetgrpc.JobLog{Line: fmt.Sprintf("line %d", i)}}})
	}
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Started{Started: &fleetgrpc.JobStarted{}}})
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Progress{Progress: &fleetgrpc.JobProgress{Step: fleetgrpc.JobStep_JOB_STEP_GIT_CLONE}}})
	for i := range jobLogHistory + 50 {
		emitLog(i)
	}
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Progress{Progress: &fleetgrpc.JobProgress{Step: fleetgrpc.JobStep_JOB_STEP_BACKEND_UP}}})

	hist := jobEvents(j)
	var logs []string
	var others int
	for _, ev := range hist {
		if ev.GetLog() != nil {
			logs = append(logs, ev.GetLog().GetLine())
		} else {
			others++
		}
	}
	if others != 3 {
		t.Fatalf("history keeps %d non-log events, want all 3", others)
	}
	if len(logs) != jobLogHistory || logs[0] != "line 50" || logs[len(logs)-1] != fmt.Sprintf("line %d", jobLogHistory+49) {
		t.Fatalf("history keeps %d log lines (%q..%q), want the last %d", len(logs), logs[0], logs[len(logs)-1], jobLogHistory)
	}
	if hist[0].GetStarted() == nil || hist[1].GetProgress() == nil {
		t.Fatal("history lost its order")
	}
}

// TestRelaySendsJobDoneDroppedFromAFullBuffer: a watcher that fell behind a
// burst of log lines still gets the stream's terminal JobDone.
func TestRelaySendsJobDoneDroppedFromAFullBuffer(t *testing.T) {
	m := newJobManager()
	j := newTestJob(m)
	stream := &blockingJobStream{ctx: context.Background(), gate: make(chan struct{})}
	relayed := make(chan error, 1)
	go func() { relayed <- relay(j, stream) }()

	// Wait until relay subscribed, then overflow its 64-slot buffer while it is
	// stuck sending, so the JobDone is dropped.
	deadline := time.Now().Add(5 * time.Second)
	for {
		j.mu.Lock()
		n := len(j.subs)
		j.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
	for i := range 200 {
		j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Log{Log: &fleetgrpc.JobLog{Line: fmt.Sprint(i)}}})
	}
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Done{Done: &fleetgrpc.JobDone{Success: true}}})
	m.finish(j.summary.GetJobId())
	close(stream.gate)

	if err := <-relayed; err != nil {
		t.Fatalf("relay: %v", err)
	}
	sent := stream.sentEvents()
	if len(sent) == 0 || sent[len(sent)-1].GetDone() == nil || !sent[len(sent)-1].GetDone().GetSuccess() {
		t.Fatalf("relay did not end on the JobDone (sent %d events)", len(sent))
	}
}

// TestAwaitJobReturnsADoneDroppedFromAFullBuffer: an MCP wait whose buffer
// overflowed with log lines (a failed create's error, line by line) still gets
// the job's outcome, not "job ended without a result".
func TestAwaitJobReturnsADoneDroppedFromAFullBuffer(t *testing.T) {
	m := newJobManager()
	j := newTestJob(m)
	hist, ch := j.subscribe()
	for i := range 200 {
		j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Log{Log: &fleetgrpc.JobLog{Line: fmt.Sprint(i)}}})
	}
	msg := "devcontainer up failed"
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Done{Done: &fleetgrpc.JobDone{Error: &msg}}})
	m.finish(j.summary.GetJobId())

	_, _, err := awaitSubscribed(context.Background(), j, hist, ch)
	if err == nil || err.Error() != msg {
		t.Fatalf("awaitJob = %v, want the job's error %q", err, msg)
	}
}

// blockingJobStream is a relay stream whose first Send waits for gate.
type blockingJobStream struct {
	ctx  context.Context
	gate chan struct{}
	mu   sync.Mutex
	sent []*fleetgrpc.JobEvent
}

func (b *blockingJobStream) Send(ev *fleetgrpc.JobEvent) error {
	<-b.gate
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, ev)
	return nil
}

func (b *blockingJobStream) Context() context.Context { return b.ctx }

func (b *blockingJobStream) sentEvents() []*fleetgrpc.JobEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fleetgrpc.JobEvent(nil), b.sent...)
}

// TestJobManagerReportsActiveJobChanges: onChange fires when a job starts,
// moves to another step and ends — not for log lines or a repeated step — and
// the summaries it then reads carry the current step.
func TestJobManagerReportsActiveJobChanges(t *testing.T) {
	m := newJobManager()
	var mu sync.Mutex
	var seen [][]string
	m.onChange = func() {
		var steps []string
		for _, s := range m.summaries() {
			steps = append(steps, s.GetInstance()+":"+s.GetCurrentStep().String())
		}
		mu.Lock()
		seen = append(seen, steps)
		mu.Unlock()
	}

	j := newTestJob(m)
	progress := func(step fleetgrpc.JobStep) {
		j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Progress{Progress: &fleetgrpc.JobProgress{Step: step}}})
	}
	progress(fleetgrpc.JobStep_JOB_STEP_GIT_CLONE)
	j.emit(&fleetgrpc.JobEvent{Event: &fleetgrpc.JobEvent_Log{Log: &fleetgrpc.JobLog{Line: "x"}}})
	progress(fleetgrpc.JobStep_JOB_STEP_GIT_CLONE)
	progress(fleetgrpc.JobStep_JOB_STEP_BACKEND_UP)
	m.finish(j.summary.GetJobId())

	want := [][]string{
		{"i1:JOB_STEP_UNSPECIFIED"},
		{"i1:JOB_STEP_GIT_CLONE"},
		{"i1:JOB_STEP_BACKEND_UP"},
		nil,
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("onChange saw %v, want %v", seen, want)
	}
}

func TestJobSummariesAreSortedCopies(t *testing.T) {
	m := newJobManager()
	base := time.Now()
	late := m.start(context.Background(), fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "late", base.Add(time.Second))
	early := m.start(context.Background(), fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "early", base)

	got := m.summaries()
	if len(got) != 2 || got[0].GetInstance() != "early" || got[1].GetInstance() != "late" {
		t.Fatalf("summaries = %v, want oldest first", got)
	}
	got[0].CurrentStep = fleetgrpc.JobStep_JOB_STEP_ROLLBACK
	if early.currentStep() != fleetgrpc.JobStep_JOB_STEP_UNSPECIFIED || late.currentStep() != fleetgrpc.JobStep_JOB_STEP_UNSPECIFIED {
		t.Fatal("summaries handed out the jobs' own records")
	}
}

// TestInterruptedJobsEndTheirCreationLog: a record the restarted daemon fails
// for an interrupted job gets the reason at the end of its creation log, the
// line its own job never got to write; an instance with no log gets none.
func TestInterruptedJobsEndTheirCreationLog(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
		{Name: "i1", Status: fleet.StatusCreating},
		{Name: "nolog", Status: fleet.StatusRebuilding},
	}}}}); err != nil {
		t.Fatal(err)
	}
	// What the dead daemon's job had written.
	l := openProvisioningLog(newTestJob(newJobManager()), "alpha", "i1", "create")
	l.Step(fleet.StepBackendUp, "devcontainer")
	if err := l.file.Close(); err != nil {
		t.Fatal(err)
	}

	newService().failInterruptedJobs()

	if log := readCreationLog(t); !strings.HasSuffix(log, "==> starting container: devcontainer\nerror: "+interruptedByRestart+"\n") {
		t.Fatalf("creation log does not end with the interruption:\n%s", log)
	}
	if _, err := os.Stat(state.InstanceCreationLogPath("alpha", "nolog")); !os.IsNotExist(err) {
		t.Fatalf("an instance without a creation log got one: %v", err)
	}
}

// A job that carries on from an earlier one's log appends to it.
func TestContinueProvisioningLogAppends(t *testing.T) {
	isolateFleetDir(t)
	path := state.InstanceCreationLogPath("alpha", "one")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("==> fleet rebuild: alpha/one\nseeding\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := newJobManager().start(context.Background(), fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE, "alpha", "one", time.Now())
	l := continueProvisioningLog(j, "alpha", "one", "rollback of the interrupted rebuild")
	l.Step(fleet.StepRollback, "restoring")
	l.finish(nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "==> fleet rebuild: alpha/one\nseeding\n==> fleet rollback of the interrupted rebuild: alpha/one at ") || !strings.Contains(got, "==> rolling back: restoring\n") {
		t.Fatalf("log:\n%s", got)
	}
}
