package launchtui

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charmbracelet/x/term"
)

// ===========================================
// CLI progress spinner
// ===========================================
//
// The headless `fleet launch <name>` path can block for a while when it starts
// an app (the command runs, then we wait for the port to come up). A small
// spinner gives the user feedback that something is happening rather than a
// silent hang. It is deliberately tiny and self-contained — the interactive
// grid uses Bubble Tea's spinner; this is for plain CLI output.

// spinnerFrames is a smooth braille spinner cycle.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is the repaint cadence.
const spinnerInterval = 100 * time.Millisecond

// startSpinner gives feedback for a blocking operation labelled msg and returns
// a stop function to call exactly once when the operation finishes.
//
// On a terminal it animates a spinner in front of msg, repainting in place; the
// stop function halts the animation and clears the line so the caller can print
// a final result cleanly. When w is not a terminal (a pipe, a redirect, or a
// test buffer) it prints msg once as a plain line and the stop function is a
// no-op — so non-interactive output never accumulates carriage-return spam.
func startSpinner(w io.Writer, msg string) (stop func()) {
	if !isTerminalWriter(w) {
		fmt.Fprintln(w, msg)
		return func() {}
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		i := 0
		for {
			// Repaint in place: carriage return, magenta spinner frame (matching
			// the app's accent), then the message.
			fmt.Fprintf(w, "\r\x1b[38;5;170m%s\x1b[0m %s", spinnerFrames[i], msg)
			select {
			case <-done:
				return
			case <-ticker.C:
				i = (i + 1) % len(spinnerFrames)
			}
		}
	}()

	return func() {
		close(done)
		<-finished
		// Clear the spinner line: carriage return + erase to end of line.
		fmt.Fprint(w, "\r\x1b[K")
	}
}

// isTerminalWriter reports whether w is an *os.File backed by a terminal, i.e.
// one we can safely animate on.
func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(file.Fd())
}
