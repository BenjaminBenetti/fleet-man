package server

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// Watch is the server-streaming subscription that replaces the TUI's polling.
// It registers a subscriber and (optionally) emits an initial snapshot in a
// single hub turn, then pumps conflated StateChanged / RuntimeChanged events to
// the client until it disconnects or the server shuts down.
//
// In P2 the persisted snapshots come from the state poller (disk re-Load) and
// the runtime sidecar from the runtime pollers; JobStarted/BrowserOpen and
// reattach_job_ids are not emitted yet (jobs land in P4, control in P3).
func (s *service) Watch(req *fleetgrpc.WatchRequest, stream grpc.ServerStreamingServer[fleetgrpc.Event]) error {
	ctx := stream.Context()
	sub := newSubscriber(req.GetSubscribeRuntime())

	// Register + initial snapshot in ONE hub turn, so the snapshot reflects
	// every mutation up to that instant and is delivered before any live event.
	registered := make(chan struct{})
	ok := s.hub.post(func(h *hub) {
		h.addSub(sub)
		if req.GetIncludeInitialState() {
			if h.st != nil {
				sub.enqueueState(h.st)
			}
			if sub.wantRuntime && len(h.runtime) > 0 {
				items := make([]*fleetgrpc.InstanceRuntime, 0, len(h.runtime))
				for _, r := range h.runtime {
					items = append(items, cloneRuntime(r))
				}
				sub.enqueueRuntime(items)
			}
			// The current remote-MCP tunnel status, so the settings page shows
			// the right state the moment it attaches (not gated on runtime).
			if h.remoteMcp != nil {
				sub.enqueueRemoteMcp(h.remoteMcp)
			}
		}
		close(registered)
	})
	if !ok {
		return nil // hub already stopped (server shutting down)
	}
	select {
	case <-registered:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer s.hub.post(func(h *hub) { h.removeSub(sub) })

	for {
		select {
		case <-ctx.Done():
			// Client disconnected.
			return nil
		case <-s.hub.done:
			// Server shutting down — end the stream cleanly so GracefulStop can
			// complete.
			return nil
		case <-sub.notify:
			st, rt, bo, rm := sub.drain()
			if st != nil {
				ev := &fleetgrpc.Event{Kind: &fleetgrpc.Event_StateChanged{StateChanged: &fleetgrpc.StateChanged{State: st}}}
				if err := stream.Send(ev); err != nil {
					return err
				}
			}
			if len(rt) > 0 {
				ev := &fleetgrpc.Event{Kind: &fleetgrpc.Event_RuntimeChanged{RuntimeChanged: &fleetgrpc.RuntimeChanged{Runtime: rt}}}
				if err := stream.Send(ev); err != nil {
					return err
				}
			}
			for _, b := range bo {
				ev := &fleetgrpc.Event{Kind: &fleetgrpc.Event_BrowserOpen{BrowserOpen: b}}
				if err := stream.Send(ev); err != nil {
					return err
				}
			}
			if rm != nil {
				ev := &fleetgrpc.Event{Kind: &fleetgrpc.Event_RemoteMcpStatus{RemoteMcpStatus: rm}}
				if err := stream.Send(ev); err != nil {
					return err
				}
			}
		}
	}
}
