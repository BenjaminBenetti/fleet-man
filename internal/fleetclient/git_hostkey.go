package fleetclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// GitHostKeyPrompt asks a human to review the daemon's fingerprint. Empty
// means reject. Implementations must stop waiting when ctx is cancelled.
type GitHostKeyPrompt func(context.Context, *fleetgrpc.UnknownSSHHostKey) string

// StartGitHostKeyPrompts registers an interactive client on this exact service
// connection and waits for registration before returning. The returned stop
// releases outstanding decisions as well as the stream. Callers may ignore an
// unsupported RPC on older daemons: their existing failure hint still works.
func StartGitHostKeyPrompts(parent context.Context, svc fleetgrpc.FleetServiceClient, prompt GitHostKeyPrompt) (func(), error) {
	ctx, cancel := context.WithCancel(parent)
	// Bound registration without putting a deadline on the long-lived stream.
	timer := time.AfterFunc(5*time.Second, cancel)
	defer timer.Stop()
	stream, err := svc.GitHostKeys(ctx)
	if err == nil {
		err = stream.Send(&fleetgrpc.GitHostKeyAnswer{})
	}
	if err == nil {
		var ready *fleetgrpc.GitHostKeyPrompt
		ready, err = stream.Recv()
		if err == nil && (ready.GetRequestId() != "" || ready.GetKey() != nil) {
			err = fmt.Errorf("invalid host-key registration acknowledgement")
		}
	}
	if err != nil {
		cancel()
		return func() {}, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var callbacks sync.WaitGroup
		defer callbacks.Wait()
		defer cancel()
		incoming := make(chan *fleetgrpc.GitHostKeyPrompt)
		go func() {
			defer close(incoming)
			for {
				req, err := stream.Recv()
				if err != nil {
					return
				}
				select {
				case incoming <- req:
				case <-ctx.Done():
					return
				}
			}
		}()
		pending := make(map[string]context.CancelFunc)
		defer func() {
			for _, stop := range pending {
				stop()
			}
		}()
		answers := make(chan *fleetgrpc.GitHostKeyAnswer)
		for {
			select {
			case <-ctx.Done():
				return
			case req, ok := <-incoming:
				if !ok {
					return
				}
				id := req.GetRequestId()
				if stop, ok := pending[id]; ok {
					stop()
					delete(pending, id)
				}
				if req.GetKey() == nil || len(req.GetKey().GetKeys()) == 0 || id == "" {
					continue
				}
				pctx, stop := context.WithCancel(ctx)
				pending[id] = stop
				callbacks.Add(1)
				go func() {
					defer callbacks.Done()
					line := prompt(pctx, req.GetKey())
					if pctx.Err() != nil {
						return
					}
					select {
					case answers <- &fleetgrpc.GitHostKeyAnswer{RequestId: id, KnownHostsLine: line}:
					case <-pctx.Done():
					}
				}()
			case answer := <-answers:
				stop, ok := pending[answer.GetRequestId()]
				if !ok {
					continue
				}
				stop()
				delete(pending, answer.GetRequestId())
				if err := stream.Send(answer); err != nil {
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}
