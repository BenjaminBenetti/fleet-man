package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
)

// Piped input must never approve trust (including a piped "yes"). Register
// before launching the job so even an immediate SSH refusal finds the client.
func gitHostKeyPromptsWhile(ctx context.Context, svc fleetgrpc.FleetServiceClient) func() {
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stderr.Fd()) {
		return func() {}
	}
	// One terminal decision at a time, even when several daemon clones ask.
	turn := make(chan struct{}, 1)
	stop, _ := fleetclient.StartGitHostKeyPrompts(ctx, svc, func(ctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		select {
		case turn <- struct{}{}:
			defer func() { <-turn }()
		case <-ctx.Done():
			return ""
		}
		reader, err := cancelreader.NewReader(os.Stdin)
		if err != nil {
			return ""
		}
		defer reader.Close()
		stopCancel := context.AfterFunc(ctx, func() { reader.Cancel() })
		defer stopCancel()
		return readGitHostKeyDecision(reader, os.Stderr, key)
	})
	return stop
}

func readGitHostKeyDecision(input io.Reader, output io.Writer, key *fleetgrpc.UnknownSSHHostKey) string {
	if len(key.GetKeys()) == 0 {
		return ""
	}
	primary := key.GetKeys()[0]
	// %q escapes control characters from URLs and paths before they reach a
	// terminal; the fingerprint remains an uninterrupted value to compare.
	fmt.Fprintf(output, "\nUnknown SSH host key for %q (%s:%d)\nRepository: %q\nKey: %s %s\nSave on the fleet host in: %q\nVerify this fingerprint with the git server administrator.\nType 'accept' to trust it and retry the clone, or press Enter to reject: ",
		key.GetName(), key.GetHost(), key.GetPort(), key.GetUrl(), primary.GetKeyType(), primary.GetFingerprint(), key.GetKnownHostsPath())
	line, err := bufio.NewReader(input).ReadString('\n')
	if err == nil && strings.TrimSpace(line) == "accept" {
		return primary.GetKnownHostsLine()
	}
	return ""
}
