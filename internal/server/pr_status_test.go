package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

func TestRunProbeWithTimeout_CapturesOutput(t *testing.T) {
	cmd := backend.NewCmd(exec.Command("sh", "-c", `printf 'hello\nworld\n'`), nil)
	out, err := runProbeWithTimeout(cmd, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello\nworld" {
		t.Fatalf("output = %q, want \"hello\\nworld\"", got)
	}
}

func TestRunProbeWithTimeout_KillsOnTimeout(t *testing.T) {
	cmd := backend.NewCmd(exec.Command("sh", "-c", "sleep 30"), nil)
	start := time.Now()
	if _, err := runProbeWithTimeout(cmd, 150*time.Millisecond); err == nil {
		t.Fatalf("expected a timeout error, got nil")
	}
	// The kill must happen promptly, not after the full sleep.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, expected it to fire near 150ms", elapsed)
	}
}

// stubGHScript emulates the net output of the `gh` calls prProbeScript makes:
// `gh auth status` succeeds, `gh pr list ... --jq '<STATE> <number>'` echoes the
// lines in $GH_FAKE_PRLIST, and `gh pr view <n>` emits a MERGED PR object.
const stubGHScript = `#!/bin/sh
case "$1" in
  auth) exit 0 ;;
  pr)
    case "$2" in
      list) cat "$GH_FAKE_PRLIST" 2>/dev/null ; exit 0 ;;
      view) printf '{"number":%s,"state":"MERGED","url":"https://example.test/%s","title":"old"}\n' "$3" "$3" ; exit 0 ;;
    esac ;;
esac
exit 0
`

func gitInitOnBranch(t *testing.T, dir, branch string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	for _, args := range [][]string{
		{"init", "-q"},
		{"checkout", "-q", "-b", branch},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir, c.Env = dir, env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestPRProbeScript_AlwaysExitsZero runs the real prProbeScript (with a stubbed
// gh) over a multi-repo workspace whose repos have no open PR. It guards the QA
// regression on issue #203: the no-PR path must exit 0 so probePRStatus treats a
// completed probe as authoritative (clearing or setting the tag) rather than a
// transient failure that discards stdout and freezes the prior tag. The earlier
// `[ -n "$num" ] && gh pr view …` tail left the script's exit status at 1 when the
// last repo find visited had no PR.
func TestPRProbeScript_AlwaysExitsZero(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stubGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	emptyList := filepath.Join(t.TempDir(), "empty")
	closedList := filepath.Join(t.TempDir(), "closed")
	if err := os.WriteFile(emptyList, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closedList, []byte("MERGED 210\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runScript := func(t *testing.T, prList string) (string, error) {
		// Two repos (top-level + nested subrepo), each on a no-PR branch, so the
		// last repo find traverses also has no PR — the layout that regressed.
		ws := t.TempDir()
		gitInitOnBranch(t, ws, "feat-x")
		gitInitOnBranch(t, filepath.Join(ws, "nested"), "feat-y")
		cmd := exec.Command("sh", "-c", prProbeScript)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GH_FAKE_PRLIST="+prList)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("no PRs => exit 0, no output", func(t *testing.T) {
		out, err := runScript(t, emptyList)
		if err != nil {
			t.Fatalf("probe script exited non-zero on a no-PR workspace: %v\noutput: %q", err, out)
		}
		if got := parsePRProbeOutput(out); got != nil {
			t.Errorf("no-PR workspace parsed to %+v, want nil (clears the tag)", got)
		}
	})

	t.Run("closed PR => exit 0, emits the merged PR", func(t *testing.T) {
		out, err := runScript(t, closedList)
		if err != nil {
			t.Fatalf("probe script exited non-zero: %v\noutput: %q", err, out)
		}
		got := parsePRProbeOutput(out)
		if got == nil {
			t.Fatalf("closed-PR workspace parsed to nil, want a purple tag (out=%q)", out)
		}
		// One MERGED PR per repo (both repos resolve the same stub list).
		if got.GetPrSignal() != fleetgrpc.PrSignal_PR_SIGNAL_PURPLE || got.GetClosedCount() == 0 {
			t.Errorf("got %+v, want PURPLE with ClosedCount>0", got)
		}
	})
}

func TestClassifyCheck(t *testing.T) {
	tests := []struct {
		name string
		c    ghCheck
		want checkResult
	}{
		{"checkrun success", ghCheck{Status: "COMPLETED", Conclusion: "SUCCESS"}, checkPass},
		{"checkrun neutral", ghCheck{Status: "COMPLETED", Conclusion: "NEUTRAL"}, checkPass},
		{"checkrun skipped", ghCheck{Status: "COMPLETED", Conclusion: "SKIPPED"}, checkPass},
		{"checkrun failure", ghCheck{Status: "COMPLETED", Conclusion: "FAILURE"}, checkFail},
		{"checkrun timed_out", ghCheck{Status: "COMPLETED", Conclusion: "TIMED_OUT"}, checkFail},
		{"checkrun action_required", ghCheck{Status: "COMPLETED", Conclusion: "ACTION_REQUIRED"}, checkFail},
		{"checkrun in_progress", ghCheck{Status: "IN_PROGRESS"}, checkPending},
		{"checkrun queued", ghCheck{Status: "QUEUED"}, checkPending},
		{"checkrun completed no conclusion", ghCheck{Status: "COMPLETED"}, checkPending},
		{"statuscontext success", ghCheck{State: "SUCCESS"}, checkPass},
		{"statuscontext pending", ghCheck{State: "PENDING"}, checkPending},
		{"statuscontext expected", ghCheck{State: "EXPECTED"}, checkPending},
		{"statuscontext failure", ghCheck{State: "FAILURE"}, checkFail},
		{"statuscontext error", ghCheck{State: "ERROR"}, checkFail},
		{"empty", ghCheck{}, checkPending},
		{"lowercase success", ghCheck{Status: "completed", Conclusion: "success"}, checkPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCheck(tt.c); got != tt.want {
				t.Errorf("classifyCheck(%+v) = %d, want %d", tt.c, got, tt.want)
			}
		})
	}
}

func passCheck() ghCheck { return ghCheck{Status: "COMPLETED", Conclusion: "SUCCESS"} }
func failCheck() ghCheck { return ghCheck{Status: "COMPLETED", Conclusion: "FAILURE"} }
func pendCheck() ghCheck { return ghCheck{Status: "IN_PROGRESS"} }

func TestAggregatePRStatus_Empty(t *testing.T) {
	if got := aggregatePRStatus(nil); got != nil {
		t.Errorf("aggregatePRStatus(nil) = %+v, want nil", got)
	}
	if got := aggregatePRStatus([]ghPR{}); got != nil {
		t.Errorf("aggregatePRStatus([]) = %+v, want nil", got)
	}
}

func TestAggregatePRStatus(t *testing.T) {
	tests := []struct {
		name string
		prs  []ghPR

		wantOpen     int32
		wantPRSig    fleetgrpc.PrSignal
		wantReview   fleetgrpc.PrReviewState
		wantPassed   int32
		wantTotal    int32
		wantCheckSig fleetgrpc.PrSignal
	}{
		{
			name: "single clean all-green approved",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
				StatusCheckRollup: []ghCheck{passCheck(), passCheck(), passCheck()},
			}},
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
			wantPassed: 3, wantTotal: 3, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "open with pending checks not yet clean -> yellow PR, yellow checks",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "BLOCKED", ReviewDecision: "REVIEW_REQUIRED",
				StatusCheckRollup: []ghCheck{passCheck(), pendCheck()},
			}},
			// Not CLEAN (checks still running) => PR indicator yellow, not green.
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING,
			wantPassed: 1, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
		},
		{
			name: "changes requested -> red PR + red review even if checks pass",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "BLOCKED", ReviewDecision: "CHANGES_REQUESTED",
				StatusCheckRollup: []ghCheck{passCheck()},
			}},
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED,
			wantPassed: 1, wantTotal: 1, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "check failure -> red PR + red checks",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "UNSTABLE", ReviewDecision: "APPROVED",
				StatusCheckRollup: []ghCheck{passCheck(), failCheck()},
			}},
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
			wantPassed: 1, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
		},
		{
			name: "conflicting (dirty) but no failures -> yellow PR",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "DIRTY", ReviewDecision: "",
				StatusCheckRollup: []ghCheck{passCheck()},
			}},
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING,
			wantPassed: 1, wantTotal: 1, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "three PRs summed, one failing -> PRx3 red, checks summed red",
			prs: []ghPR{
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
					StatusCheckRollup: []ghCheck{passCheck(), passCheck()}},
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "",
					StatusCheckRollup: []ghCheck{passCheck()}},
				{State: "OPEN", MergeStateStatus: "UNSTABLE", ReviewDecision: "",
					StatusCheckRollup: []ghCheck{failCheck(), passCheck()}},
			},
			wantOpen: 3, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING,
			wantPassed: 4, wantTotal: 5, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
		},
		{
			name: "two PRs both clean, one with a pending check -> green PR, yellow checks",
			prs: []ghPR{
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "",
					StatusCheckRollup: []ghCheck{passCheck()}},
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "",
					StatusCheckRollup: []ghCheck{pendCheck()}},
			},
			// PR signal (all CLEAN) and checks signal (a pending check) are
			// computed independently.
			wantOpen: 2, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING,
			wantPassed: 1, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
		},
		{
			name: "no checks at all -> green checks, total 0",
			prs: []ghPR{{
				State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
			}},
			wantOpen: 1, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
			wantPassed: 0, wantTotal: 0, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "changes requested wins over another PR's approval",
			prs: []ghPR{
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
					StatusCheckRollup: []ghCheck{passCheck()}},
				{State: "OPEN", MergeStateStatus: "BLOCKED", ReviewDecision: "CHANGES_REQUESTED",
					StatusCheckRollup: []ghCheck{passCheck()}},
			},
			wantOpen: 2, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_RED,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED,
			wantPassed: 2, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "every PR approved -> Accepted",
			prs: []ghPR{
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
					StatusCheckRollup: []ghCheck{passCheck()}},
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
					StatusCheckRollup: []ghCheck{passCheck()}},
			},
			wantOpen: 2, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED,
			wantPassed: 2, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
		{
			name: "one approved, one still under review -> Under Review (not all approved)",
			prs: []ghPR{
				{State: "OPEN", MergeStateStatus: "CLEAN", ReviewDecision: "APPROVED",
					StatusCheckRollup: []ghCheck{passCheck()}},
				{State: "OPEN", MergeStateStatus: "BLOCKED", ReviewDecision: "REVIEW_REQUIRED",
					StatusCheckRollup: []ghCheck{passCheck()}},
			},
			wantOpen: 2, wantPRSig: fleetgrpc.PrSignal_PR_SIGNAL_YELLOW,
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING,
			wantPassed: 2, wantTotal: 2, wantCheckSig: fleetgrpc.PrSignal_PR_SIGNAL_GREEN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregatePRStatus(tt.prs)
			if got == nil {
				t.Fatalf("aggregatePRStatus returned nil")
			}
			if got.GetOpenCount() != tt.wantOpen {
				t.Errorf("OpenCount = %d, want %d", got.GetOpenCount(), tt.wantOpen)
			}
			if got.GetPrSignal() != tt.wantPRSig {
				t.Errorf("PrSignal = %v, want %v", got.GetPrSignal(), tt.wantPRSig)
			}
			if got.GetReview() != tt.wantReview {
				t.Errorf("Review = %v, want %v", got.GetReview(), tt.wantReview)
			}
			if got.GetChecksPassed() != tt.wantPassed {
				t.Errorf("ChecksPassed = %d, want %d", got.GetChecksPassed(), tt.wantPassed)
			}
			if got.GetChecksTotal() != tt.wantTotal {
				t.Errorf("ChecksTotal = %d, want %d", got.GetChecksTotal(), tt.wantTotal)
			}
			if got.GetChecksSignal() != tt.wantCheckSig {
				t.Errorf("ChecksSignal = %v, want %v", got.GetChecksSignal(), tt.wantCheckSig)
			}
		})
	}
}

func TestParsePRProbeOutput_Sentinels(t *testing.T) {
	if got := parsePRProbeOutput("FLEET_NO_GH\n"); got != nil {
		t.Errorf("no-gh sentinel = %+v, want nil", got)
	}
	if got := parsePRProbeOutput("FLEET_NO_AUTH\n"); got != nil {
		t.Errorf("no-auth sentinel = %+v, want nil", got)
	}
	if got := parsePRProbeOutput(""); got != nil {
		t.Errorf("empty output = %+v, want nil", got)
	}
	if got := parsePRProbeOutput("\n  \n"); got != nil {
		t.Errorf("blank output = %+v, want nil", got)
	}
}

func TestParsePRProbeOutput_SentinelStringInsideJSONIsNotDegrade(t *testing.T) {
	// A check name (or branch, title, …) that coincidentally contains a sentinel
	// string must NOT silently disable the probe — the real output is a JSON
	// object, so only a leading sentinel counts as "degrade".
	out := `{"number":3,"state":"OPEN","mergeStateStatus":"UNSTABLE","reviewDecision":"","statusCheckRollup":[{"name":"FLEET_NO_GH-smoke","status":"COMPLETED","conclusion":"SUCCESS"}]}`
	got := parsePRProbeOutput(out)
	if got == nil {
		t.Fatalf("parsePRProbeOutput degraded to nil on a sentinel substring inside JSON")
	}
	if got.GetOpenCount() != 1 {
		t.Errorf("OpenCount = %d, want 1", got.GetOpenCount())
	}
}

func TestParsePRProbeOutput_ConcatenatedObjects(t *testing.T) {
	// Two `gh pr view --json` objects concatenated (workspace repo + a subrepo):
	// one approved all-green PR and one failing-check PR. The second is
	// pretty-printed to prove whitespace between objects doesn't matter.
	out := `{"number":12,"state":"OPEN","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}
{
  "number": 7,
  "state": "OPEN",
  "mergeStateStatus": "UNSTABLE",
  "reviewDecision": "",
  "statusCheckRollup": [
    {"status": "COMPLETED", "conclusion": "FAILURE"},
    {"state": "SUCCESS"}
  ]
}
`
	got := parsePRProbeOutput(out)
	if got == nil {
		t.Fatalf("parsePRProbeOutput returned nil")
	}
	if got.GetOpenCount() != 2 {
		t.Errorf("OpenCount = %d, want 2", got.GetOpenCount())
	}
	if got.GetPrSignal() != fleetgrpc.PrSignal_PR_SIGNAL_RED {
		t.Errorf("PrSignal = %v, want RED", got.GetPrSignal())
	}
	if got.GetChecksPassed() != 2 || got.GetChecksTotal() != 3 {
		t.Errorf("checks = %d/%d, want 2/3", got.GetChecksPassed(), got.GetChecksTotal())
	}
	if got.GetChecksSignal() != fleetgrpc.PrSignal_PR_SIGNAL_RED {
		t.Errorf("ChecksSignal = %v, want RED", got.GetChecksSignal())
	}
}

func TestParsePRProbeOutput_Empty(t *testing.T) {
	// No PRs at all => the script emits nothing => nil (the branch never had a PR).
	if got := parsePRProbeOutput(""); got != nil {
		t.Errorf("empty output = %+v, want nil", got)
	}
}

func TestParsePRProbeOutput_ClosedPersistsPurple(t *testing.T) {
	// A branch whose only PR is closed/merged keeps a persistent purple tag (issue
	// #203) rather than vanishing — open_count 0, closed_count > 0, the closed PR
	// kept as a ref so the tag stays clickable.
	for _, tc := range []struct {
		name, state string
	}{
		{"closed", "CLOSED"},
		{"merged", "MERGED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := `{"number":1,"state":"` + tc.state + `","url":"https://example.test/pr/1","title":"old work"}`
			got := parsePRProbeOutput(out)
			if got == nil {
				t.Fatalf("%s-only output = nil, want a purple tag", tc.state)
			}
			if got.GetOpenCount() != 0 {
				t.Errorf("OpenCount = %d, want 0", got.GetOpenCount())
			}
			if got.GetClosedCount() != 1 {
				t.Errorf("ClosedCount = %d, want 1", got.GetClosedCount())
			}
			if got.GetPrSignal() != fleetgrpc.PrSignal_PR_SIGNAL_PURPLE {
				t.Errorf("PrSignal = %v, want PURPLE", got.GetPrSignal())
			}
			if n := len(got.GetPrs()); n != 1 {
				t.Fatalf("len(Prs) = %d, want 1 (kept clickable)", n)
			}
			if got.GetPrs()[0].GetNumber() != 1 {
				t.Errorf("Prs[0].Number = %d, want 1", got.GetPrs()[0].GetNumber())
			}
		})
	}
}

func TestParsePRProbeOutput_OpenWinsOverClosed(t *testing.T) {
	// An open PR (workspace repo) and a closed one (a subrepo) => the open status
	// wins; the closed one neither shows nor inflates closed_count.
	closed := `{"number":1,"state":"CLOSED","url":"https://example.test/pr/1","title":"old"}`
	open := `{"number":2,"state":"OPEN","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`
	got := parsePRProbeOutput(closed + "\n" + open + "\n")
	if got == nil {
		t.Fatalf("parsePRProbeOutput returned nil, want one open PR")
	}
	if got.GetOpenCount() != 1 {
		t.Errorf("OpenCount = %d, want 1", got.GetOpenCount())
	}
	if got.GetClosedCount() != 0 {
		t.Errorf("ClosedCount = %d, want 0 (open takes priority)", got.GetClosedCount())
	}
	if got.GetPrSignal() != fleetgrpc.PrSignal_PR_SIGNAL_GREEN {
		t.Errorf("PrSignal = %v, want GREEN (the open PR)", got.GetPrSignal())
	}
}
