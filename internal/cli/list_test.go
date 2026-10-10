package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestListIncludesBranchColumnAndValues checks the BRANCH column comes from
// the server (GetState fresh_branches), never a checkout read on this machine:
// the live read wins, and without one the row falls back to the last branch
// the server saw, then the requested branch.
func TestListIncludesBranchColumnAndValues(t *testing.T) {
	sp := func(s string) *string { return &s }
	instance := func(name, last, requested string) *fleetgrpc.Instance {
		inst := &fleetgrpc.Instance{
			Name:         name,
			ContainerId:  sp("abc123456789999"),
			WorkspaceDir: sp("/workspace/alpha/" + name),
			CreatedAt:    timestamppb.New(time.Unix(0, 0)),
			Status:       fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING,
		}
		if last != "" {
			inst.LastBranch = sp(last)
		}
		if requested != "" {
			inst.Branch = sp(requested)
		}
		return inst
	}

	// Inject a canned reply through the server seam so the test doesn't need
	// a live fleet server.
	reply := &fleetgrpc.GetStateReply{
		State: &fleetgrpc.State{
			Fleets: map[string]*fleetgrpc.Fleet{
				"alpha": {
					Name: "alpha",
					Instances: []*fleetgrpc.Instance{
						instance("agent-1", "old-branch", "requested"),
						instance("agent-2", "", ""),
						instance("agent-3", "last-seen", "requested"),
						instance("agent-4", "", "requested-only"),
					},
				},
			},
		},
		Runtime: []*fleetgrpc.InstanceRuntime{
			{Fleet: "alpha", Instance: "agent-1", CurrentBranch: sp("feature/status-line")},
			// A live read on a detached HEAD: blank, not a fallback.
			{Fleet: "alpha", Instance: "agent-2", CurrentBranch: sp("")},
			// No live read possible: the fallbacks apply.
			{Fleet: "alpha", Instance: "agent-3"},
		},
	}
	prevFetch := fetchListState
	var asked bool
	fetchListState = func(context.Context) (*fleetgrpc.GetStateReply, error) {
		asked = true
		return reply, nil
	}
	defer func() { fetchListState = prevFetch }()

	var out bytes.Buffer
	prevOutput := listOutput
	listOutput = &out
	defer func() { listOutput = prevOutput }()

	cmd := newListCmd()
	if err := cmd.RunE(cmd, []string{"alpha"}); err != nil {
		t.Fatalf("RunE() error = %v", err)
	}
	if !asked {
		t.Fatal("list must fetch through fetchListState (GetState with fresh_branches)")
	}

	output := out.String()
	if !strings.Contains(output, "BRANCH") {
		t.Fatalf("output missing BRANCH header:\n%s", output)
	}
	for name, want := range map[string]string{
		"agent-1": "feature/status-line",
		"agent-3": "last-seen",
		"agent-4": "requested-only",
	} {
		if line := findLine(output, name); !strings.HasSuffix(strings.TrimRight(line, " "), want) {
			t.Fatalf("%s row should end with branch %q:\n%s", name, want, output)
		}
	}

	agentTwoLine := findLine(output, "agent-2")
	if agentTwoLine == "" {
		t.Fatalf("output missing agent-2 line:\n%s", output)
	}
	expectedCreated := time.Unix(0, 0).Local().Format("2006-01-02 15:04")
	if !strings.HasSuffix(strings.TrimRight(agentTwoLine, " "), expectedCreated) {
		t.Fatalf("agent-2 line should end at CREATED column when branch is empty:\n%s", agentTwoLine)
	}
}

func findLine(output, needle string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
