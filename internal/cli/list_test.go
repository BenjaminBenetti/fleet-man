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

func TestListIncludesBranchColumnAndValues(t *testing.T) {
	sp := func(s string) *string { return &s }

	// Inject a canned snapshot through the server seam so the test doesn't need
	// a live fleet server (Phase 1 routes list through fleetclient.Dial+GetState).
	st := &fleetgrpc.State{
		Fleets: map[string]*fleetgrpc.Fleet{
			"alpha": {
				Name: "alpha",
				Instances: []*fleetgrpc.Instance{
					{
						Name:         "agent-1",
						ContainerId:  sp("abc123456789999"),
						WorkspaceDir: sp("/workspace/alpha/agent-1"),
						CreatedAt:    timestamppb.New(time.Unix(0, 0)),
						Status:       fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING,
					},
					{
						Name:         "agent-2",
						ContainerId:  sp("def987654321000"),
						WorkspaceDir: sp("/workspace/alpha/agent-2"),
						CreatedAt:    timestamppb.New(time.Unix(0, 0)),
						Status:       fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPED,
					},
				},
			},
		},
	}
	prevFetch := fetchFleetState
	fetchFleetState = func(context.Context) (*fleetgrpc.State, error) { return st, nil }
	defer func() { fetchFleetState = prevFetch }()

	var out bytes.Buffer
	prevOutput := listOutput
	listOutput = &out
	defer func() { listOutput = prevOutput }()

	prevBranchName := listBranchName
	listBranchName = func(workspaceDir string) string {
		if workspaceDir == "/workspace/alpha/agent-1" {
			return "feature/status-line"
		}
		return ""
	}
	defer func() { listBranchName = prevBranchName }()

	cmd := newListCmd()
	if err := cmd.RunE(cmd, []string{"alpha"}); err != nil {
		t.Fatalf("RunE() error = %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "BRANCH") {
		t.Fatalf("output missing BRANCH header:\n%s", output)
	}
	if !strings.Contains(output, "feature/status-line") {
		t.Fatalf("output missing branch value:\n%s", output)
	}

	agentTwoLine := findLine(output, "agent-2")
	if agentTwoLine == "" {
		t.Fatalf("output missing agent-2 line:\n%s", output)
	}
	if strings.Contains(agentTwoLine, "feature/status-line") {
		t.Fatalf("agent-2 line unexpectedly contains branch value:\n%s", agentTwoLine)
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
