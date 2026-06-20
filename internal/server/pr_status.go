package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// pr_status.go computes the per-instance "auto tag": the aggregated GitHub
// pull-request state for an instance's workspace repo PLUS any nested subrepos.
// It runs `gh` INSIDE the container (so it uses the instance's own gh auth) on a
// slow cadence, parses the result, and pushes a PrStatus onto the runtime
// sidecar. The TUI renders it in the tag slot when the instance has no user-set
// tag. When gh is missing or unauthenticated inside the instance, the probe
// degrades to "no auto-tag" (PrStatus nil), preserving today's behaviour.

const (
	// prStatusInterval is the run cadence for the gh probe. PR state changes on
	// the order of minutes (CI runs), and each probe hits the GitHub API, so we
	// keep this comfortably slow.
	prStatusInterval = 45 * time.Second
	// prStatusGatePoll is how often the poller checks the runtime-wanted gate so
	// it can fire a probe promptly when a TUI first subscribes (rather than
	// waiting a full prStatusInterval). The check itself is a cheap atomic load.
	prStatusGatePoll = 5 * time.Second
	// prProbeTimeout bounds a single instance's gh probe so a hung network call
	// can't wedge the pass.
	prProbeTimeout = 20 * time.Second
	// prProbeMaxConcurrent caps how many container gh probes run at once.
	prProbeMaxConcurrent = 4

	prNoGHSentinel   = "FLEET_NO_GH"
	prNoAuthSentinel = "FLEET_NO_AUTH"
)

// prProbeScript runs inside the container with the workspace folder as its cwd
// (the backend's ExecCommand resolves that). For the workspace repo and every
// nested subrepo it emits `gh pr list ... --json ...` — a JSON array of the open
// PRs on that repo's current branch. The arrays are concatenated on stdout and
// read back with a streaming json.Decoder, so their exact whitespace/formatting
// doesn't matter. It prints a sentinel and exits 0 when gh is absent or not
// logged in, so the server can distinguish "degrade quietly" from a transient
// exec failure.
const prProbeScript = `
command -v gh >/dev/null 2>&1 || { printf '%s\n' "FLEET_NO_GH"; exit 0; }
gh auth status >/dev/null 2>&1 || { printf '%s\n' "FLEET_NO_AUTH"; exit 0; }
# Repo roots = the workspace repo plus any nested subrepos. Each match is a
# ".git"; its parent dir is the repo. Bounded depth, skipping node_modules and
# never descending into .git itself.
find . -maxdepth 5 -name node_modules -prune -o -name .git -prune -print 2>/dev/null | while IFS= read -r gitpath; do
  dir=$(dirname "$gitpath")
  br=$(git -C "$dir" rev-parse --abbrev-ref HEAD 2>/dev/null) || continue
  [ -z "$br" ] && continue
  [ "$br" = "HEAD" ] && continue
  ( cd "$dir" && gh pr list --state open --head "$br" \
      --json number,state,mergeStateStatus,reviewDecision,statusCheckRollup,url,title 2>/dev/null )
done
`

// ghPR mirrors the subset of `gh pr list --json` fields the auto-tag needs.
type ghPR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	// MergeStateStatus is GitHub's "ready to merge" verdict — CLEAN means the PR
	// is mergeable (approved, required checks passed, no conflicts), which is the
	// green signal. We use it over the plain `mergeable` field because that only
	// reflects merge CONFLICTS and would show green while CI is still running.
	MergeStateStatus  string    `json:"mergeStateStatus"` // CLEAN | BLOCKED | BEHIND | UNSTABLE | DIRTY | DRAFT | UNKNOWN | ...
	ReviewDecision    string    `json:"reviewDecision"`   // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	URL               string    `json:"url"`
	Title             string    `json:"title"`
}

// ghCheck mirrors one element of statusCheckRollup. A CheckRun carries
// status+conclusion; a StatusContext carries state — we classify from whichever
// is present rather than relying on __typename.
type ghCheck struct {
	Status     string `json:"status"`     // CheckRun: QUEUED|IN_PROGRESS|COMPLETED|...
	Conclusion string `json:"conclusion"` // CheckRun: SUCCESS|FAILURE|NEUTRAL|...
	State      string `json:"state"`      // StatusContext: SUCCESS|PENDING|FAILURE|ERROR
}

type checkResult int

const (
	checkPending checkResult = iota
	checkPass
	checkFail
)

// classifyCheck reduces one rollup element to pass / pending / fail.
func classifyCheck(c ghCheck) checkResult {
	// StatusContext-style: a legacy commit-status with a State.
	if c.State != "" {
		switch strings.ToUpper(c.State) {
		case "SUCCESS":
			return checkPass
		case "PENDING", "EXPECTED":
			return checkPending
		default: // FAILURE, ERROR
			return checkFail
		}
	}
	// CheckRun-style: status + conclusion.
	switch strings.ToUpper(c.Status) {
	case "COMPLETED":
		switch strings.ToUpper(c.Conclusion) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			return checkPass
		case "": // completed without a conclusion yet — treat as still settling
			return checkPending
		default: // FAILURE, TIMED_OUT, CANCELLED, ACTION_REQUIRED, STARTUP_FAILURE, STALE
			return checkFail
		}
	default: // QUEUED, IN_PROGRESS, PENDING, WAITING, REQUESTED, or unknown
		return checkPending
	}
}

// aggregatePRStatus folds the open PRs across an instance's repos into the
// PrStatus the TUI renders. Returns nil when there are no open PRs (no auto-tag).
func aggregatePRStatus(prs []ghPR) *fleetgrpc.PrStatus {
	if len(prs) == 0 {
		return nil
	}

	var passed, total int
	anyFail, anyPending := false, false
	anyChangesRequested, anyApproved := false, false
	allClean := true

	refs := make([]*fleetgrpc.PrRef, 0, len(prs))
	for _, pr := range prs {
		refs = append(refs, &fleetgrpc.PrRef{
			Number: int32(pr.Number),
			Url:    pr.URL,
			Title:  pr.Title,
		})
		for _, c := range pr.StatusCheckRollup {
			total++
			switch classifyCheck(c) {
			case checkPass:
				passed++
			case checkFail:
				anyFail = true
			case checkPending:
				anyPending = true
			}
		}
		switch strings.ToUpper(pr.ReviewDecision) {
		case "CHANGES_REQUESTED":
			anyChangesRequested = true
		case "APPROVED":
			anyApproved = true
		}
		if strings.ToUpper(pr.MergeStateStatus) != "CLEAN" {
			allClean = false
		}
	}

	// PR indicator colour: red on any failure/changes-requested, else green
	// when every PR is mergeable per GitHub (mergeStateStatus CLEAN), else yellow
	// (open and in progress, no failures).
	prSignal := fleetgrpc.PrSignal_PR_SIGNAL_YELLOW
	switch {
	case anyFail || anyChangesRequested:
		prSignal = fleetgrpc.PrSignal_PR_SIGNAL_RED
	case allClean:
		prSignal = fleetgrpc.PrSignal_PR_SIGNAL_GREEN
	}

	// Review element: changes-requested wins over approved; neither => hidden.
	review := fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNSPECIFIED
	switch {
	case anyChangesRequested:
		review = fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED
	case anyApproved:
		review = fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED
	}

	// Checks colour: red on any failure, else yellow while any pending, else
	// green (all passed).
	checksSignal := fleetgrpc.PrSignal_PR_SIGNAL_GREEN
	switch {
	case anyFail:
		checksSignal = fleetgrpc.PrSignal_PR_SIGNAL_RED
	case anyPending:
		checksSignal = fleetgrpc.PrSignal_PR_SIGNAL_YELLOW
	}

	return &fleetgrpc.PrStatus{
		OpenCount:    int32(len(prs)),
		PrSignal:     prSignal,
		Review:       review,
		ChecksPassed: int32(passed),
		ChecksTotal:  int32(total),
		ChecksSignal: checksSignal,
		Prs:          refs,
	}
}

// parsePRProbeOutput turns the probe's stdout — one `gh pr list --json` array
// per repo, concatenated — into a PrStatus. A nil result means "no auto-tag" (gh
// unavailable, or no open PRs) and clears any prior status. The gh-missing /
// no-auth sentinels and the empty case all map to nil. A streaming json.Decoder
// reads successive arrays regardless of their interleaving whitespace; malformed
// trailing noise stops the scan without discarding what parsed cleanly.
func parsePRProbeOutput(out string) *fleetgrpc.PrStatus {
	// Match the sentinels by PREFIX, not Contains: the script emits one as the
	// sole output (before any JSON, then exits), while real output is a JSON
	// array starting with '['. A prefix check can't be tripped by a sentinel
	// string that happens to appear inside PR JSON (a branch name, check name…).
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, prNoGHSentinel) || strings.HasPrefix(trimmed, prNoAuthSentinel) {
		return nil
	}
	var prs []ghPR
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var batch []ghPR
		if err := dec.Decode(&batch); err != nil {
			break // io.EOF on clean exhaustion, or unexpected noise.
		}
		for _, p := range batch {
			if strings.EqualFold(p.State, "OPEN") {
				prs = append(prs, p)
			}
		}
	}
	return aggregatePRStatus(prs)
}

// probePRStatus runs the gh probe inside the instance and parses the result.
// The bool is false only on a transient exec failure (timeout, container
// briefly unavailable), in which case the caller keeps the prior status rather
// than clearing it. A successful probe returns ok=true even when the PrStatus is
// nil (gh absent / no PRs), which legitimately clears the auto-tag.
func (h *hub) probePRStatus(b backend.Backend, workspaceDir string) (*fleetgrpc.PrStatus, bool) {
	cmd := b.ExecCommandQuiet(workspaceDir, []string{"sh", "-c", prProbeScript})
	out, err := runProbeWithTimeout(cmd, prProbeTimeout)
	if err != nil {
		return nil, false
	}
	return parsePRProbeOutput(string(out)), true
}

// runProbeWithTimeout runs cmd to completion, capturing stdout, and kills it if
// it overruns timeout. It drives the embedded raw *exec.Cmd directly (Start +
// Wait) so it can interrupt a hung process — Output() offers no deadline.
//
// On timeout it uses raw.Process.Kill() rather than a raw syscall.Kill on the
// numeric pid: os.Process refuses to signal a pid it has already reaped
// (returning ErrProcessDone under its internal lock), so it can never race
// Wait() into signalling a REUSED pid. It signals only the direct child, but
// that is sufficient here — WaitDelay handles the case where a helper the child
// spawned (devcontainer -> docker exec) inherited our stdout pipe and would
// otherwise keep it open: after the delay Go force-closes the pipe and Wait
// returns, and the orphaned helper exits on its next write (EOF/SIGPIPE). This
// keeps the fix self-contained, avoiding a context.Context thread through the
// Backend.ExecCommand interface.
func runProbeWithTimeout(cmd *backend.Cmd, timeout time.Duration) ([]byte, error) {
	raw := cmd.Cmd
	var buf bytes.Buffer
	raw.Stdout = &buf
	// Stderr stays nil -> /dev/null; the script already redirects gh's own
	// diagnostics, and we only care about the JSON on stdout.
	raw.WaitDelay = 3 * time.Second
	if err := raw.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- raw.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case <-time.After(timeout):
		if raw.Process != nil {
			_ = raw.Process.Kill()
		}
		<-done
		return nil, fmt.Errorf("pr status probe timed out after %s", timeout)
	}
}

// prStatusPoller fires the gh probe across running instances on the slow
// prStatusInterval cadence, plus once on the runtime-wanted false->true edge so
// the auto-tag appears shortly after a TUI first subscribes rather than after a
// full interval. The edge is detected on the next gate tick (within
// prStatusGatePoll) rather than via h.runtimeEdge, which is single-consumer
// (owned by liveStatusPoller); a few seconds' latency on a status line that
// refreshes every prStatusInterval is an acceptable trade for not reworking that
// channel into a broadcast. Like the other runtime pollers it does nothing while
// no TUI is connected.
func prStatusPoller(ctx context.Context, h *hub) {
	ticker := time.NewTicker(prStatusGatePoll)
	defer ticker.Stop()
	var lastRun time.Time
	wasWanted := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !h.runtimeWanted.Load() {
				wasWanted = false
				continue
			}
			edge := !wasWanted
			wasWanted = true
			due := lastRun.IsZero() || time.Since(lastRun) >= prStatusInterval
			if edge || due {
				prStatusPass(h)
				lastRun = time.Now()
			}
		}
	}
}

func prStatusPass(h *hub) {
	st, err := state.Load()
	if err != nil {
		return
	}
	type item struct {
		fleetName, instance, workspaceDir string
		inst                              *fleet.Instance
	}
	var items []item
	for fleetName, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.ContainerID == "" || inst.Status != fleet.StatusRunning {
				continue
			}
			// A workspace dir is required to scope the probe: the backend's exec
			// resolves it to the in-container workspace folder, which is where the
			// script's `find` runs. Without it the script would run in the
			// container's default cwd (often /) and scan the whole filesystem, so
			// skip — the instance simply gets no auto-tag (graceful degrade).
			if inst.WorkspaceDir == "" {
				continue
			}
			items = append(items, item{fleetName, inst.Name, inst.WorkspaceDir, inst})
		}
	}
	if len(items) == 0 {
		return
	}

	type result struct {
		fleetName, instance string
		status              *fleetgrpc.PrStatus
		ok                  bool
	}
	results := make([]result, len(items))
	sem := make(chan struct{}, prProbeMaxConcurrent)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, it item) {
			defer wg.Done()
			defer func() { <-sem }()
			b := h.backendFor(it.inst)
			status, ok := h.probePRStatus(b, it.workspaceDir)
			results[i] = result{it.fleetName, it.instance, status, ok}
		}(i, it)
	}
	wg.Wait()

	h.post(func(h *hub) {
		ups := make([]runtimeUpdate, 0, len(results))
		for _, r := range results {
			if !r.ok {
				// Transient probe failure: keep the prior PrStatus.
				continue
			}
			status := r.status
			ups = append(ups, runtimeUpdate{r.fleetName, r.instance, func(rt *fleetgrpc.InstanceRuntime) {
				rt.PrStatus = status
			}})
		}
		h.applyRuntime(ups)
	})
}
