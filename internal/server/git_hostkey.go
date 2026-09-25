package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A prompt belongs to one connected client and one clone attempt. Answers
// cannot write arbitrary entries or accept a prompt on another connection.
// Detached jobs survive their launcher, but an unanswered trust decision does
// not: disconnect, shutdown and the deadline all fail the clone closed.
type gitHostKeyHub struct {
	mu      sync.Mutex
	seq     uint64
	clients []*gitHostKeyClient
}

type gitHostKeyClient struct {
	ctx      context.Context
	requests chan *gitHostKeyRequest
}

type gitHostKeyRequest struct {
	ctx    context.Context
	prompt *fleetgrpc.GitHostKeyPrompt
	answer chan string
	stop   func() bool
}

func (h *gitHostKeyHub) ask(ctx context.Context, remote string, key *sshtunnel.UnknownHostKeyError) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	h.mu.Lock()
	var client *gitHostKeyClient
	for i := len(h.clients) - 1; i >= 0; i-- {
		if h.clients[i].ctx.Err() == nil {
			client = h.clients[i]
			break
		}
	}
	if client == nil {
		h.mu.Unlock()
		return "", nil
	}
	h.seq++
	req := &gitHostKeyRequest{
		ctx: ctx, answer: make(chan string, 1),
		prompt: &fleetgrpc.GitHostKeyPrompt{RequestId: fmt.Sprint(h.seq), Key: sshHostKeyDetail(remote, key)},
	}
	h.mu.Unlock()
	select {
	case client.requests <- req:
	case <-client.ctx.Done():
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case line := <-req.answer:
		if ctx.Err() != nil || client.ctx.Err() != nil {
			return "", nil
		}
		return line, nil
	case <-client.ctx.Done():
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *service) GitHostKeys(stream grpc.BidiStreamingServer[fleetgrpc.GitHostKeyAnswer, fleetgrpc.GitHostKeyPrompt]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetRequestId() != "" || first.GetKnownHostsLine() != "" {
		return status.Error(codes.InvalidArgument, "first host-key frame must be empty")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	client := &gitHostKeyClient{ctx: ctx, requests: make(chan *gitHostKeyRequest, 16)}
	h := &s.gitHostKeys
	h.mu.Lock()
	h.clients = append(h.clients, client)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, c := range h.clients {
			if c == client {
				h.clients = append(h.clients[:i], h.clients[i+1:]...)
				break
			}
		}
	}()
	// The caller waits for this before starting a job/inspection, avoiding a
	// race between the first clone failure and provider registration.
	if err := stream.Send(&fleetgrpc.GitHostKeyPrompt{}); err != nil {
		return err
	}
	answers := make(chan *fleetgrpc.GitHostKeyAnswer)
	go func() {
		defer cancel()
		for {
			answer, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case answers <- answer:
			case <-ctx.Done():
				return
			}
		}
	}()
	cancelled := make(chan string)
	pending := make(map[string]*gitHostKeyRequest)
	defer func() {
		for _, req := range pending {
			req.stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.bgCtx.Done():
			return nil
		case req := <-client.requests:
			if req.ctx.Err() != nil {
				continue
			}
			if len(pending) >= 16 {
				req.answer <- ""
				continue
			}
			id := req.prompt.GetRequestId()
			req.stop = context.AfterFunc(req.ctx, func() {
				select {
				case cancelled <- id:
				case <-ctx.Done():
				}
			})
			pending[id] = req
			if err := stream.Send(req.prompt); err != nil {
				return err
			}
		case id := <-cancelled:
			if _, ok := pending[id]; ok {
				delete(pending, id)
				if err := stream.Send(&fleetgrpc.GitHostKeyPrompt{RequestId: id}); err != nil {
					return err
				}
			}
		case answer := <-answers:
			id, line := answer.GetRequestId(), answer.GetKnownHostsLine()
			req, ok := pending[id]
			if !ok {
				continue // expired or already answered; never writes anything
			}
			if line != "" {
				offered := false
				for _, key := range req.prompt.GetKey().GetKeys() {
					offered = offered || line == key.GetKnownHostsLine()
				}
				if !offered {
					return status.Error(codes.InvalidArgument, "host key was not offered on this stream")
				}
			}
			delete(pending, id)
			req.stop()
			if req.ctx.Err() == nil {
				req.answer <- line
			}
		}
	}
}

func sshHostKeyDetail(remote string, uk *sshtunnel.UnknownHostKeyError) *fleetgrpc.UnknownSSHHostKey {
	detail := &fleetgrpc.UnknownSSHHostKey{
		Url: remote, Name: uk.Name, Host: uk.Host, Port: uint32(uk.Port),
		KeyType: uk.KeyType, KnownHostsPath: uk.KnownHostsPath,
	}
	for _, k := range uk.Keys {
		detail.Keys = append(detail.Keys, &fleetgrpc.SSHHostKey{KeyType: k.Type, Fingerprint: k.Fingerprint, KnownHostsLine: k.Line})
	}
	return detail
}
