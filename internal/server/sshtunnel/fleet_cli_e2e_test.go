//go:build !nok8s

package sshtunnel_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/cli"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestFleetCLIOverArmadaSSH runs the fleet-level settings and actions the
// Kubernetes work added — a custom mount bound to a daemon-host folder
// (fleet mount), agents bound to a cluster connection (fleet agent
// --cluster) with the daemon's login warning, the shared-folder sync (fleet
// sync status|wait|conflicts) and a rebuild — against a real fleetd over its
// local socket and then as an Armada client over SSH, and requires the same
// transcript and the same saved settings: a remote client has each of them
// exactly as a local one. Paths are the daemon host's in both runs.
func TestFleetCLIOverArmadaSSH(t *testing.T) {
	d := startSSHModeFleetd(t)
	home := filepath.Dir(d.fleetDir)
	data := filepath.Join(home, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "missing")
	if err := state.Update(func(st *state.State) error {
		st.GetOrCreateFleet("parity", "https://example.invalid/parity.git")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	session := [][]string{
		{"cluster", "add", "remote-ctx", "--name", "remote", "--kubeconfig", d.kubeconfig},
		{"mount", "add", "parity", "/opt/data", "--host", data},
		{"mount", "add", "parity", "/opt/gone", "--host", missing},
		{"mount", "add", "parity", "/opt/fleet-state", "--host", d.fleetDir},
		{"mount", "list", "parity"},
		{"mount", "add", "parity", "/opt/gone"},
		{"mount", "list", "parity"},
		{"agent", "create", "parity", "runner", "--cluster", "remote", "--command", "ANTHROPIC_API_KEY=x claude '${PROMPT}'"},
		{"agent", "create", "parity", "needlogin", "--cluster", "remote", "--command", "claude '${PROMPT}'"},
		{"agent", "create", "parity", "nowhere", "--cluster", "nowhere", "--command", "true"},
		{"agent", "list", "parity"},
		{"agent", "edit", "parity", "runner", "--backend", "devcontainer"},
		{"agent", "list", "parity"},
		{"sync", "status", "parity"},
		{"sync", "wait", "parity", "--timeout", "5s"},
		{"sync", "conflicts", "parity"},
		{"rebuild", "parity/ghost"},
	}
	// Each run leaves the daemon as it found it, so both start alike.
	cleanup := [][]string{
		{"agent", "delete", "parity", "runner"},
		{"agent", "delete", "parity", "needlogin"},
		{"mount", "remove", "parity", "/opt/data"},
		{"mount", "remove", "parity", "/opt/gone"},
		{"cluster", "rm", "remote", "--forget"},
	}
	// The connection's id is random per add.
	volatile := regexp.MustCompile(`remote-ctx-[a-z0-9]{6}`)
	execute := func(args []string, transcript *strings.Builder) {
		ctx, cancel := context.WithTimeout(d.ctx, 90*time.Second)
		defer cancel()
		cmd := cli.NewRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		if transcript == nil {
			return
		}
		fmt.Fprintf(transcript, "$ fleet %s\n%s", strings.Join(args, " "), out.String())
		if err != nil {
			fmt.Fprintf(transcript, "exit 1\n")
		}
	}
	// settings is what the daemon saved for the fleet after the session.
	settings := func() string {
		st, err := state.Load()
		if err != nil {
			t.Fatal(err)
		}
		f := st.Fleets["parity"]
		if f == nil {
			t.Fatal("the fleet is gone")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "mounts %v hosts %v\n", f.Settings.CustomMounts, f.Settings.CustomMountHostFolders)
		for _, a := range f.Settings.Agents {
			fmt.Fprintf(&b, "agent %s %s %q\n", a.Name, a.DeployTarget().Backend, a.Command)
		}
		return volatile.ReplaceAllString(b.String(), "<id>")
	}
	run := func(transport string) (string, string) {
		t.Setenv(fleetclient.EnvSSH, "")
		if transport == "armada-ssh" {
			t.Setenv(fleetclient.EnvSSH, d.url())
		}
		var transcript strings.Builder
		for _, args := range session {
			execute(args, &transcript)
		}
		saved := settings()
		for _, args := range cleanup {
			execute(args, nil)
		}
		return volatile.ReplaceAllString(transcript.String(), "<id>"), saved
	}
	local, localSaved := run("local")
	remote, remoteSaved := run("armada-ssh")
	if remote != local {
		t.Errorf("the session differs over Armada SSH:\n--- local\n%s\n--- armada-ssh\n%s", local, remote)
	}
	if remoteSaved != localSaved {
		t.Errorf("the saved settings differ over Armada SSH:\n--- local\n%s\n--- armada-ssh\n%s", localSaved, remoteSaved)
	}

	for _, want := range []string{
		// A host folder binds as is; a missing one warns; ~/.fleet is refused.
		"/opt/data  " + data,
		"warning: ",
		missing,
		"$ fleet mount add parity /opt/fleet-state --host " + d.fleetDir + "\nError: ",
		// Without --host the mount is back on its .mnt folder.
		"$ fleet mount add parity /opt/gone\n",
		// Agents on a connection, and the daemon's warning for one that has
		// no login there; one naming no connection is refused.
		"kubernetes:<id>",
		"warning: agent needlogin: log in to Claude once for this fleet on ☸ remote",
		"$ fleet agent create parity nowhere --cluster nowhere --command true\nError: ",
		"$ fleet rebuild parity/ghost\nError: ",
	} {
		if !strings.Contains(local, want) {
			t.Errorf("the session lacks %q:\n%s", want, local)
		}
	}
	if !strings.Contains(localSaved, "agent runner devcontainer") || !strings.Contains(localSaved, "agent needlogin kubernetes") ||
		!strings.Contains(localSaved, "/opt/data:"+data) {
		t.Errorf("the daemon saved:\n%s", localSaved)
	}
	if strings.Contains(localSaved, "/opt/gone:") || strings.Contains(localSaved, "fleet-state") {
		t.Errorf("the daemon kept a host folder it should not have:\n%s", localSaved)
	}

	// The SSH run went through FLEET_SSH: pointed at a port nothing serves,
	// the same command fails.
	t.Setenv(fleetclient.EnvSSH, "ssh://tester@127.0.0.1:1")
	var dead strings.Builder
	execute([]string{"mount", "list", "parity"}, &dead)
	if !strings.Contains(dead.String(), "exit 1") {
		t.Errorf("fleet mount list through an unreachable FLEET_SSH succeeded: the session never used it:\n%s", dead.String())
	}
}
