package server

import (
	"os/exec"
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
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNDER_REVIEW,
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
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNDER_REVIEW,
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
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNDER_REVIEW,
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
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNDER_REVIEW,
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
			wantReview: fleetgrpc.PrReviewState_PR_REVIEW_STATE_UNDER_REVIEW,
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
	// array, so only a leading sentinel counts as "degrade".
	out := `[{"number":3,"state":"OPEN","mergeStateStatus":"UNSTABLE","reviewDecision":"","statusCheckRollup":[{"name":"FLEET_NO_GH-smoke","status":"COMPLETED","conclusion":"SUCCESS"}]}]`
	got := parsePRProbeOutput(out)
	if got == nil {
		t.Fatalf("parsePRProbeOutput degraded to nil on a sentinel substring inside JSON")
	}
	if got.GetOpenCount() != 1 {
		t.Errorf("OpenCount = %d, want 1", got.GetOpenCount())
	}
}

func TestParsePRProbeOutput_ConcatenatedArrays(t *testing.T) {
	// Two repos' `gh pr list --json` arrays concatenated: the workspace repo
	// (one approved, all-green PR) and a subrepo (one failing-check PR). The
	// second array is pretty-printed to prove whitespace doesn't matter.
	out := `[{"number":12,"state":"OPEN","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}]
[
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
]
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

func TestParsePRProbeOutput_EmptyArraysAndClosed(t *testing.T) {
	// A repo with no PRs emits `[]`; a repo whose only PR is closed contributes
	// nothing. Combined => no open PRs => nil.
	out := "[]\n[{\"number\":1,\"state\":\"CLOSED\",\"mergeStateStatus\":\"CLEAN\",\"statusCheckRollup\":[]}]\n[]\n"
	if got := parsePRProbeOutput(out); got != nil {
		t.Errorf("parsePRProbeOutput = %+v, want nil (no open PRs)", got)
	}

	// Same but with one genuinely open PR mixed in.
	out2 := "[]\n[{\"number\":2,\"state\":\"OPEN\",\"mergeStateStatus\":\"CLEAN\",\"reviewDecision\":\"APPROVED\",\"statusCheckRollup\":[{\"status\":\"COMPLETED\",\"conclusion\":\"SUCCESS\"}]}]\n"
	got := parsePRProbeOutput(out2)
	if got == nil {
		t.Fatalf("parsePRProbeOutput returned nil, want one open PR")
	}
	if got.GetOpenCount() != 1 {
		t.Errorf("OpenCount = %d, want 1", got.GetOpenCount())
	}
}
