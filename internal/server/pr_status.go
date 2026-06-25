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
// nested subrepo it finds the open PRs on the current branch (gh pr list) and
// then emits the FULL detail of each via `gh pr view <n> --json ...` — one JSON
// object per PR. gh pr view is used (not gh pr list's --json) because gh pr
// list's statusCheckRollup is capped at the first 100 checks, while gh pr view
// paginates the rollup and returns every check. When the branch has NO open PR,
// it instead emits the most recent closed/merged PR (number/state/url/title
// only — no check detail needed) so the tag persists in purple rather than
// vanishing. A single `gh pr list --state all` per repo covers both cases (so a
// branch parked without an open PR costs no extra round-trip vs. the old
// open-only query). The objects are concatenated on stdout and read back with a
// streaming json.Decoder, so their exact whitespace/formatting doesn't matter.
// It prints a sentinel and exits 0 when gh is absent or not logged in, so the
// server can distinguish "degrade quietly" from a transient exec failure.
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
  ( cd "$dir" || exit 0
    # One list call for the branch (gh's embedded --jq, so no standalone jq
    # dependency): "<STATE> <number>" per PR, e.g. "OPEN 12" / "MERGED 7".
    list=$(gh pr list --state all --head "$br" --json number,state --jq '.[] | "\(.state) \(.number)"' 2>/dev/null)
    open=$(printf '%s\n' "$list" | awk '$1 == "OPEN" { print $2 }')
    if [ -n "$open" ]; then
      for num in $open; do
        gh pr view "$num" \
          --json number,state,mergeStateStatus,reviewDecision,statusCheckRollup,url,title 2>/dev/null
      done
    else
      # No open PR on this branch — surface the most recent closed/merged one (if
      # any) so the indicator persists in purple instead of disappearing. The
      # highest PR number is unambiguously the newest, so pick it without relying
      # on gh's list ordering.
      num=$(printf '%s\n' "$list" | awk '$1 != "OPEN" && $2 > max { max = $2 } END { if (max) print max }')
      if [ -n "$num" ]; then
        gh pr view "$num" --json number,state,url,title 2>/dev/null
      fi
    fi
  )
done
# Always exit 0 once the scan completes: an empty result (a repo with no PRs) is a
# valid probe outcome that should CLEAR the tag, not a transient failure. Without
# this, a no-PR repo visited last by find leaves the loop's exit status at 1, and
# probePRStatus would treat the whole probe as failed (ok=false) — discarding any
# PR JSON already emitted and freezing the prior tag. A non-zero exit (ok=false,
# keep prior) is reserved for genuine exec/timeout errors, which abort earlier.
exit 0
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

// aggregatePRStatus folds the open PRs across an instance's repos into the live
// green/yellow/red PrStatus the TUI renders. Returns nil when there are no open
// PRs — the caller then falls back to closedPRStatus for the purple closed tag.
func aggregatePRStatus(prs []ghPR) *fleetgrpc.PrStatus {
	if len(prs) == 0 {
		return nil
	}

	var passed, total int
	anyFail, anyPending := false, false
	anyChangesRequested := false
	allApproved := true // every PR carries an APPROVED decision
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
			allApproved = false
		case "APPROVED":
			// counts toward allApproved
		default:
			// REVIEW_REQUIRED, "", or anything else => not (yet) approved.
			allApproved = false
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

	// Review element: changes-requested ("Rejected", red) wins; else "Accepted"
	// (green) only when every PR is approved; otherwise "Pending" (grey). An open
	// PR therefore always shows one of the three.
	review := fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING
	switch {
	case anyChangesRequested:
		review = fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED
	case allApproved:
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

// parsePRProbeOutput turns the probe's stdout — one `gh pr view --json` object
// per PR, concatenated — into a PrStatus. Open PRs drive the live green/yellow/red
// tag and take priority; only when there are none does a closed/merged PR drive
// the persistent purple tag. A nil result means "no auto-tag" (gh unavailable, or
// the branch never had a PR) and clears any prior status. The gh-missing / no-auth
// sentinels and the empty case all map to nil. A streaming json.Decoder reads
// successive objects regardless of their interleaving whitespace; malformed
// trailing noise stops the scan without discarding what parsed cleanly.
func parsePRProbeOutput(out string) *fleetgrpc.PrStatus {
	// Match the sentinels by PREFIX, not Contains: the script emits one as the
	// sole output (before any JSON, then exits), while real output is a JSON
	// object starting with '{'. A prefix check can't be tripped by a sentinel
	// string that happens to appear inside PR JSON (a branch name, check name…).
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, prNoGHSentinel) || strings.HasPrefix(trimmed, prNoAuthSentinel) {
		return nil
	}
	var open, closed []ghPR
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var p ghPR
		if err := dec.Decode(&p); err != nil {
			break // io.EOF on clean exhaustion, or unexpected noise.
		}
		switch strings.ToUpper(p.State) {
		case "OPEN":
			open = append(open, p)
		case "CLOSED", "MERGED":
			closed = append(closed, p)
		}
	}
	// Open PRs win: their live status is the actionable one. Fall back to the
	// closed/merged tag only when nothing is open.
	if s := aggregatePRStatus(open); s != nil {
		return s
	}
	return closedPRStatus(closed)
}

// closedPRStatus builds the persistent purple "PR" tag shown when a branch has no
// open PR but DID have one that's since closed or merged. It keeps the tag
// visible — so a finished instance is distinguishable from one that never had a
// PR — and carries the closed PRs' refs so the tag stays clickable. Returns nil
// when there are no closed/merged PRs either (the branch never had one).
func closedPRStatus(closed []ghPR) *fleetgrpc.PrStatus {
	if len(closed) == 0 {
		return nil
	}
	refs := make([]*fleetgrpc.PrRef, 0, len(closed))
	for _, pr := range closed {
		refs = append(refs, &fleetgrpc.PrRef{
			Number: int32(pr.Number),
			Url:    pr.URL,
			Title:  pr.Title,
		})
	}
	return &fleetgrpc.PrStatus{
		ClosedCount: int32(len(closed)),
		PrSignal:    fleetgrpc.PrSignal_PR_SIGNAL_PURPLE,
		Prs:         refs,
	}
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
