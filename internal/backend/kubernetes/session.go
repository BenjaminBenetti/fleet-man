//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// PodRef names a verified pod.
type PodRef struct {
	Cluster   string
	Namespace string
	Name      string
	UID       types.UID
}

// ErrSessionClosed is a session closed on fleetd's side.
var ErrSessionClosed = errors.New("pod agent session closed")

// AgentSession is fleetd's agent session with one verified pod: the main
// session (control, polls, scripts, PCM, small files) and the bulk session
// it opens on demand (large files, the workspace and folder tars, sync data),
// paced while audio flows on the pod and closed once idle.
type AgentSession struct {
	pool    *Pool
	slot    *podSlot
	cluster *Cluster
	pod     PodRef
	probe   jobboot.Probe
	// want is the sha of the daemon's build for the pod's arch ("" when the
	// store has none): a session of another build is Stale.
	want string

	main   *podwire.Client
	stream *kubeexec.Stream
	pacer  *podwire.Pacer

	mu         sync.Mutex
	closed     bool
	bulk       *podwire.Client
	bulkFlight *bulkFlight
	bulkUsed   time.Time
	tmux       string
}

type bulkFlight struct {
	done   chan struct{}
	client *podwire.Client
	err    error
}

// Pod is the verified pod the session runs in.
func (s *AgentSession) Pod() PodRef { return s.pod }

// Client is the main session.
func (s *AgentSession) Client() *podwire.Client { return s.main }

// Hello is what the agent said it is.
func (s *AgentSession) Hello() podwire.Hello { return s.main.Hello() }

// Arch is the pod's arch (GOARCH).
func (s *AgentSession) Arch() string { return s.probe.Arch }

// Stale reports whether the agent is another build than the daemon's: it is
// accepted (same protocol major) and restaged lazily (PodTarget.Restage).
func (s *AgentSession) Stale() bool { return s.want != "" && s.main.Hello().SHA != s.want }

// Activity is the pod's audio activity, which paces the bulk session (and
// orders reconnects): the mic and output paths feed it.
func (s *AgentSession) Activity() *podwire.AudioActivity { return s.slot.activity }

// Pacer paces the bulk session; the output path feeds it late PCM frames.
func (s *AgentSession) Pacer() *podwire.Pacer { return s.pacer }

// Done is closed when the main session ends.
func (s *AgentSession) Done() <-chan struct{} { return s.main.Done() }

// Err is why the session ended; nil while it runs.
func (s *AgentSession) Err() error { return s.main.Err() }

func (s *AgentSession) alive() bool { return s.main.Err() == nil }

// Bulk is the bulk session, opened on first use (one exec upgrade) and
// closed after PoolOptions.BulkIdle without a stream.
func (s *AgentSession) Bulk(ctx context.Context) (*podwire.Client, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if b := s.bulk; b != nil && b.Err() == nil {
		s.bulkUsed = s.pool.opts.Now()
		s.mu.Unlock()
		return b, nil
	}
	f := s.bulkFlight
	if f == nil {
		f = &bulkFlight{done: make(chan struct{})}
		s.bulkFlight = f
		go s.openBulk(f)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.client, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *AgentSession) openBulk(f *bulkFlight) {
	ctx, cancel := context.WithTimeout(s.pool.ctx, s.pool.opts.HandshakeTimeout)
	defer cancel()
	client, stream, err := s.pool.dialAgent(ctx, s.cluster, s.slot, s.pod, podwire.RoleBulk, s.pacer)
	s.mu.Lock()
	s.bulkFlight = nil
	if err == nil && s.closed {
		_ = client.Close()
		client, err = nil, ErrSessionClosed
	}
	if err == nil {
		s.bulk = client
		s.bulkUsed = s.pool.opts.Now()
		go s.closeIdleBulk(client, stream)
	}
	f.client, f.err = client, err
	s.mu.Unlock()
	close(f.done)
}

// closeIdleBulk closes the bulk session once it has had no stream, and
// nobody has asked for it, for BulkIdle.
func (s *AgentSession) closeIdleBulk(b *podwire.Client, stream *kubeexec.Stream) {
	idle := s.pool.opts.BulkIdle
	t := time.NewTicker(max(idle/4, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-b.Done():
			_ = stream.Close()
			s.mu.Lock()
			if s.bulk == b {
				s.bulk = nil
			}
			s.mu.Unlock()
			return
		case <-t.C:
			now := s.pool.opts.Now()
			s.mu.Lock()
			since := b.IdleSince()
			quiet := b.Active() == 0 && !since.IsZero() && now.Sub(since) >= idle && now.Sub(s.bulkUsed) >= idle
			if quiet && s.bulk == b {
				s.bulk = nil
			}
			s.mu.Unlock()
			if quiet {
				_ = b.Close()
			}
		}
	}
}

// EnsureTmux installs the bundled static tmux into the pod, unless that very
// bundle is already installed, and returns its directory (/opt/fleet/tmux:
// bin/tmux, and the terminfo it was built to read). Callers use it for an
// image without a tmux of its own. The bundle travels on the bulk session.
func (s *AgentSession) EnsureTmux(ctx context.Context) (string, error) {
	dir := s.pool.opts.Layout.InstallDir + "/" + TmuxBundle
	art, err := s.pool.bins().Tmux(ctx, s.probe.Arch)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	done := s.tmux == art.SHA256
	s.mu.Unlock()
	if done {
		return dir, nil
	}
	check := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: installBundleArgv(s.pool.opts.Layout, art, true)}}
	res, err := s.main.Run(ctx, check, nil)
	if err != nil {
		return "", fmt.Errorf("checking the pod's tmux: %w", err)
	}
	switch res.Code {
	case 0:
	case exitBundleMissing:
		if err := s.installBundle(ctx, art); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("checking the pod's tmux: exit %d%s", res.Code, detail(string(res.Stderr)))
	}
	s.mu.Lock()
	s.tmux = art.SHA256
	s.mu.Unlock()
	return dir, nil
}

func (s *AgentSession) installBundle(ctx context.Context, art *binstore.Artifact) error {
	bulk, err := s.Bulk(ctx)
	if err != nil {
		return fmt.Errorf("installing tmux: %w", err)
	}
	r, err := art.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	install := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: installBundleArgv(s.pool.opts.Layout, art, false)}}
	res, err := bulk.Run(ctx, install, r)
	if err != nil {
		return fmt.Errorf("installing tmux: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("installing tmux: exit %d%s", res.Code, detail(string(res.Stderr)))
	}
	return nil
}

// Close ends the session at once (the agent holds its resumable commands
// for a later session).
func (s *AgentSession) Close() error {
	s.mu.Lock()
	s.closed = true
	b := s.bulk
	s.bulk = nil
	s.mu.Unlock()
	if b != nil {
		_ = b.Close()
	}
	return s.main.Close()
}

// Shutdown tells the agent and the bulk session to close and waits, within
// ctx, for them to end.
func (s *AgentSession) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	b := s.bulk
	s.bulk = nil
	s.mu.Unlock()
	var wg sync.WaitGroup
	if b != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Shutdown(ctx)
		}()
	}
	err := s.main.Shutdown(ctx)
	wg.Wait()
	_ = s.stream.Close()
	return err
}

// dialAgent starts `pod-agent serve` with role in the verified pod and runs
// the handshake (bounded by ctx), refusing an agent that reports another pod
// than pod. pacer, when set, paces the session's transport (bulk).
func (p *Pool) dialAgent(ctx context.Context, c *Cluster, slot *podSlot, pod PodRef, role podwire.Role, pacer *podwire.Pacer) (*podwire.Client, *kubeexec.Stream, error) {
	argv := []string{p.opts.Layout.Binary(), "pod-agent", "serve", "--proto", strconv.Itoa(podwire.ProtoMax)}
	if role == podwire.RoleBulk {
		argv = append(argv, "--role", string(podwire.RoleBulk))
	}
	req := kubeexec.ExecRequest{Namespace: pod.Namespace, Pod: pod.Name, Container: DevContainer, Command: argv}
	stream := kubeexec.StartStream(p.ctx, req, func(ctx context.Context, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
		return p.exec(ctx, c, req, stdio)
	})
	var transport io.ReadWriteCloser = stream
	if pacer != nil {
		transport = pacer.Wrap(stream)
	}
	client, err := podwire.Dial(ctx, transport, podwire.ClientOptions{
		Role:       role,
		Version:    p.opts.Version,
		LogLimiter: slot.logs,
		Check: func(h podwire.Hello) error {
			if types.UID(h.PodUID) != pod.UID {
				return fmt.Errorf("the agent reports pod %q, not the verified pod %s", h.PodUID, pod.UID)
			}
			return nil
		},
	})
	if err != nil {
		_ = stream.Close()
		select {
		case <-stream.Done():
		case <-time.After(time.Second):
		}
		cause := err
		var refused *podwire.RefusedError
		if exitErr := stream.Err(); !errors.As(err, &refused) && exitErr != nil && !errors.Is(exitErr, context.Canceled) {
			cause = fmt.Errorf("%w (the agent exec ended: %v)", err, exitErr)
		}
		return nil, nil, fmt.Errorf("pod agent %s in %s/%s: %w%s", role, pod.Namespace, pod.Name, cause, detail(stream.Stderr()))
	}
	return client, stream, nil
}
