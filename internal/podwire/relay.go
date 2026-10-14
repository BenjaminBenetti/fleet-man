package podwire

import (
	"context"
	"fmt"
	"net"
	"time"
)

// RelayAnswerWait bounds the wait for the agent's answer to a relay stream
// fleetd opens (OpenRelay) when ctx sets no deadline: a TagSyncCtl stream is
// answered once the pod's spoke has started, which may first wait for the
// lease of one ending in another pod.
const RelayAnswerWait = 90 * time.Second

// OpenRelay opens a stream of kind tag with header, for a relay the agent
// answers with OPEN (SockReply) before the bytes flow raw: the shared-state
// streams to the pod's spoke (TagSyncCtl on the agent session, whose header
// is the spoke's spec; TagSyncBulk on the bulk session). It returns the
// connection past the answer, or a *SockRefusedError.
func (c *Client) OpenRelay(ctx context.Context, tag Tag, header any) (net.Conn, error) {
	t, err := c.open(ctx, tag, header)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(RelayAnswerWait)
	}
	_ = t.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = t.SetReadDeadline(time.Now()) })
	f, err := NewFrameReader(t).Next()
	stop()
	if err != nil {
		_ = t.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the agent's answer to a %s stream: %w", tag, ctx.Err())
		}
		return nil, fmt.Errorf("the agent's answer to a %s stream: %w", tag, noEOF(err))
	}
	if f.Type != FrameOpen {
		_ = t.Close()
		return nil, fmt.Errorf("the agent answered a %s stream with %s", tag, f.Type)
	}
	var r SockReply
	if err := DecodeFrame(f, &r); err != nil {
		_ = t.Close()
		return nil, err
	}
	if r.Refused != "" || r.Error != "" {
		_ = t.Close()
		return nil, &SockRefusedError{Tag: tag, Reason: r.Refused, Msg: r.Error, Agent: true}
	}
	_ = t.SetReadDeadline(time.Time{})
	return t, nil
}
