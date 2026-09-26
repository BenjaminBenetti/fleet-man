package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/creack/pty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestGitHostKeyTerminalDecision(t *testing.T) {
	key := &fleetgrpc.UnknownSSHHostKey{
		Url: "git@server:repo.git", Name: "[server]:2222", Host: "server", Port: 2222,
		KnownHostsPath: "/remote/.ssh/known_hosts",
		Keys:           []*fleetgrpc.SSHHostKey{{KeyType: "ssh-ed25519", Fingerprint: "SHA256:test-fingerprint", KnownHostsLine: "[server]:2222 ssh-ed25519 AAAA"}},
	}
	for _, input := range []string{"accept\n", "\n", "yes\n", "y\n", "a\n", "accept", ""} {
		var out bytes.Buffer
		line := readGitHostKeyDecision(strings.NewReader(input), &out, key)
		want := ""
		if input == "accept\n" {
			want = key.Keys[0].KnownHostsLine
		}
		if line != want {
			t.Errorf("%q: got %q, want %q", input, line, want)
		}
		for _, want := range []string{key.Keys[0].Fingerprint, key.KnownHostsPath, key.Url, "fleet host"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("prompt missing %q", want)
			}
		}
	}
}

func TestGitHostKeyPipedInputDoesNotRegister(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	orig := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = orig }()
	// A nil service would panic on registration: even a piped approval must
	// return without contacting the daemon or consuming stdin.
	output.WriteString("accept\n")
	gitHostKeyPromptsWhile(context.Background(), nil)()
	buf := make([]byte, 7)
	if n, err := input.Read(buf); err != nil || string(buf[:n]) != "accept\n" {
		t.Fatalf("piped input consumed: %q, %v", buf[:n], err)
	}
}

type terminalHostKeyServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
	answer chan string
}

func (s *terminalHostKeyServer) GitHostKeys(stream grpc.BidiStreamingServer[fleetgrpc.GitHostKeyAnswer, fleetgrpc.GitHostKeyPrompt]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&fleetgrpc.GitHostKeyPrompt{}); err != nil {
		return err
	}
	key := &fleetgrpc.UnknownSSHHostKey{Name: "git-host", Keys: []*fleetgrpc.SSHHostKey{{Fingerprint: "SHA256:terminal", KnownHostsLine: "git-host ssh-ed25519 AAAA"}}}
	if err := stream.Send(&fleetgrpc.GitHostKeyPrompt{RequestId: "1", Key: key}); err != nil {
		return err
	}
	answer, err := stream.Recv()
	if err != nil {
		return err
	}
	s.answer <- answer.GetKnownHostsLine()
	<-stream.Context().Done()
	return nil
}

func TestGitHostKeyTerminalProvider(t *testing.T) {
	for _, action := range []string{"accept", "cancel"} {
		t.Run(action, func(t *testing.T) {
			master, terminal, err := pty.Open()
			if err != nil {
				t.Skipf("pty unavailable: %v", err)
			}
			defer master.Close()
			defer terminal.Close()
			stdin, stderr := os.Stdin, os.Stderr
			os.Stdin, os.Stderr = terminal, terminal
			defer func() { os.Stdin, os.Stderr = stdin, stderr }()
			server := &terminalHostKeyServer{answer: make(chan string, 1)}
			gs := grpc.NewServer()
			fleetgrpc.RegisterFleetServiceServer(gs, server)
			lis := bufconn.Listen(1 << 20)
			go gs.Serve(lis)
			defer gs.Stop()
			conn, err := grpc.NewClient("passthrough:///hostkey", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop := gitHostKeyPromptsWhile(ctx, fleetgrpc.NewFleetServiceClient(conn))
			defer stop()
			shown := make(chan string, 1)
			go func() {
				var text strings.Builder
				buf := make([]byte, 1024)
				for {
					n, err := master.Read(buf)
					text.Write(buf[:n])
					if err != nil || strings.Contains(text.String(), "press Enter to reject:") {
						shown <- text.String()
						return
					}
				}
			}()
			select {
			case text := <-shown:
				if !strings.Contains(text, "SHA256:terminal") {
					t.Fatalf("fingerprint not shown: %q", text)
				}
			case <-ctx.Done():
				t.Fatal("terminal prompt never appeared")
			}
			if action == "accept" {
				master.WriteString("accept\n")
				select {
				case line := <-server.answer:
					if line != "git-host ssh-ed25519 AAAA" {
						t.Fatalf("wrong answer: %q", line)
					}
				case <-ctx.Done():
					t.Fatal("acceptance never reached the daemon")
				}
			}
			stopped := make(chan struct{})
			go func() { stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("provider did not cancel its terminal read")
			}
		})
	}
}
