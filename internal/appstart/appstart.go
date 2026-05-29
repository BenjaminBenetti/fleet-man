// Package appstart provides the shared "ensure a local app is running on a
// port" logic used to bring a dev service up on demand from inside an
// instance.
//
// The behaviour was first written for the landing page's app tabs (start the
// app's command if its port isn't already answering, then hold until it comes
// up) and is reused by the `fleet launch` TUI, so it lives here as a small
// reusable package rather than being duplicated. Readiness is always
// determined by polling the port — never by a command's exit — because the
// commands are long-running servers that are started detached and not waited
// on.
package appstart

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// reachTimeout caps a single readiness probe so a slow or hung service can't
// stall the poll.
const reachTimeout = time.Second

// startDeadline bounds how long EnsureRunningOnPort waits for a freshly
// started command to bind its port before giving up. It is deliberately
// generous: the command often returns well before the service it launches is
// actually listening (e.g. `docker run -d` exits as soon as the container is
// created, but the server inside takes seconds to boot — longer still on a
// first run that has to pull the image, or for a heavier app like Grafana).
// A short window made activations spuriously fail with "did not become
// reachable" even though a moment later the port was up. The wait is cheap to
// extend because it ends the instant the port answers (see pollInterval) and
// runs on a background command, so the UI stays responsive throughout.
const startDeadline = 60 * time.Second

// pollInterval is the gap between readiness probes while waiting for a port to
// come up. Short so a quick-binding app opens with little perceptible lag — the
// wait returns on the first successful probe, not after a fixed delay.
const pollInterval = 250 * time.Millisecond

// Reachable reports whether url answers an HTTP request within a short timeout
// (one second). Any response — even an error status — counts as reachable: it
// means the app's own server is up and listening, which is all the caller
// needs to know before pointing a browser at it.
func Reachable(url string) bool {
	client := &http.Client{Timeout: reachTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// WaitForReachable polls url until it answers or timeout elapses, reporting
// whether it came up. Used to hold a request open until an app's HTTP server
// is listening, since a server can take a moment to bind its port after its
// command is launched.
func WaitForReachable(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if Reachable(url) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// LocalURL returns the conventional http://localhost:<port> address for a
// service listening on port. Centralised so every caller forms the URL the
// same way.
func LocalURL(port int) string {
	return fmt.Sprintf("http://localhost:%d", port)
}

// EnsureRunningOnPort makes sure something is answering on port, starting
// command first if needed, then blocks until the port is reachable or the
// startDeadline passes — retrying every pollInterval so a service that isn't up
// the instant its command exits is still caught once it finishes booting.
//
// It is idempotent: if the port is already answering it returns nil
// immediately and leaves whatever is running untouched (so a repeated activate
// or relaunch never double-starts the app). An empty command means "the app is
// already running; just wait for / confirm its port" — nothing is started and
// readiness is decided purely by the poll.
//
// command, when run, is executed via `bash -c`, started detached, and NOT
// waited on: it is expected to be a long-running server (or to detach itself,
// e.g. `docker run -d`). Because readiness is determined by polling the port
// rather than by the command's exit, a command that never binds the port
// surfaces here as a "did not become reachable" error rather than a hang.
func EnsureRunningOnPort(command string, port int) error {
	url := LocalURL(port)
	if Reachable(url) {
		return nil // already serving; leave it alone
	}

	if strings.TrimSpace(command) != "" {
		cmd := exec.Command("bash", "-c", command)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start command: %w", err)
		}
	}

	if !WaitForReachable(url, startDeadline) {
		return fmt.Errorf("did not become reachable on port %d within %s", port, startDeadline)
	}
	return nil
}
