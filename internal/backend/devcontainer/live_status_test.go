package devcontainer

import (
	"errors"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

func TestInspectLiveStatus(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range []struct {
		name           string
		stdout, stderr string
		err            error
		want           backend.LiveStatus
	}{
		{name: "running", stdout: "running\n", want: backend.LiveStatusRunning},
		{name: "restarting", stdout: "restarting\n", want: backend.LiveStatusRunning},
		{name: "exited", stdout: "exited\n", want: backend.LiveStatusStopped},
		{name: "paused", stdout: "paused\n", want: backend.LiveStatusStopped},
		{name: "dead", stdout: "dead\n", want: backend.LiveStatusStopped},
		{name: "created", stdout: "created\n", want: backend.LiveStatusStopped},
		{name: "removing", stdout: "removing\n", want: backend.LiveStatusMissing},
		{name: "odd state", stdout: "weird\n", want: backend.LiveStatusUnknown},
		// docker 29: `docker inspect` of a removed container.
		{name: "no such object, lower case", stderr: "error: no such object: 681e4b073e60\n", err: exit, want: backend.LiveStatusMissing},
		{name: "no such object", stderr: "Error: No such object: 681e4b073e60\n", err: exit, want: backend.LiveStatusMissing},
		{name: "no such container", stderr: "Error response from daemon: No such container: 681e4b073e60\n", err: exit, want: backend.LiveStatusMissing},
		{name: "daemon down", stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", err: exit, want: backend.LiveStatusUnknown},
		{name: "no docker", err: errors.New(`exec: "docker": executable file not found in $PATH`), want: backend.LiveStatusUnknown},
	} {
		if got := inspectLiveStatus(tc.stdout, tc.stderr, tc.err); got != tc.want {
			t.Errorf("%s: inspectLiveStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}
