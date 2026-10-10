//go:build linux

package podagent

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// relayClient is the client's side of a relay: an exec stream on a session,
// or a direct exec's stdio.
type relayClient struct {
	r *podwire.FrameReader
	w *podwire.FrameWriter
	c io.Closer
	// finIsAbort tells, once a read ends with io.EOF, whether that was the
	// client's FIN (abort); nil means EOF is a lost transport.
	finIsAbort func() bool
	// ping and silence are the heartbeat of a transport that crosses the
	// network on its own; 0 means none.
	ping, silence time.Duration
}

// dialShepherd connects to the held command h names and sends h; it returns
// the connection and the shepherd's answer (RESUME, or OPEN when it is gone).
func dialShepherd(heldDir string, h podwire.ExecHeader) (*net.UnixConn, *podwire.FrameReader, podwire.Frame, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: heldSocket(heldDir, h.Resume), Net: "unix"})
	if err != nil {
		return nil, nil, podwire.Frame{}, err
	}
	if err := podwire.WritePreamble(conn, podwire.TagExec, h); err != nil {
		_ = conn.Close()
		return nil, nil, podwire.Frame{}, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(podwire.HeaderDeadline))
	r := podwire.NewFrameReader(conn)
	f, err := r.Next()
	if err != nil {
		_ = conn.Close()
		return nil, nil, podwire.Frame{}, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, r, f, nil
}

// relay copies frames between a client and a shepherd until the shepherd's
// side ends. A lost client transport just closes the shepherd connection,
// which holds the command; the client's FIN becomes ABORT.
func relay(cl relayClient, sc *net.UnixConn, sr *podwire.FrameReader) {
	sw := podwire.NewFrameWriter(sc)
	var lastRecv atomic.Int64
	lastRecv.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var closeOnce sync.Once
	var holding atomic.Bool
	hold := func() {
		closeOnce.Do(func() {
			holding.Store(true)
			_ = sc.Close()
		})
	}
	// After the client's FIN (abort) it sends nothing more, not even the
	// EXIT that acknowledges the status: once that status has gone down,
	// the shepherd connection is closed, which an aborted command takes as
	// the end.
	var endMu sync.Mutex
	var clientEnded, exitForwarded bool
	ended := func(client, exit bool) {
		endMu.Lock()
		clientEnded = clientEnded || client
		exitForwarded = exitForwarded || exit
		both := clientEnded && exitForwarded
		endMu.Unlock()
		if both {
			hold()
		}
	}

	go func() {
		defer close(done)
		exitSeen := false
		for {
			f, err := sr.Next()
			if err != nil {
				// Only a shepherd that went away on its own loses the
				// status; one this relay let go of is holding it.
				if !exitSeen && !holding.Load() {
					_ = cl.w.WriteJSON(podwire.FrameExit, podwire.ExitStatus{Code: -1, Error: "the held command's shepherd ended"})
				}
				return
			}
			if f.Type == podwire.FrameExit {
				exitSeen = true
			}
			if err := cl.w.Write(f.Type, f.Payload); err != nil {
				hold()
				return
			}
			if f.Type == podwire.FrameExit {
				ended(false, true)
			}
		}
	}()
	go func() {
		for {
			f, err := cl.r.Next()
			if err != nil {
				if errors.Is(err, io.EOF) && cl.finIsAbort != nil && cl.finIsAbort() {
					_ = sw.Write(podwire.FrameAbort, nil)
					ended(true, false)
					return
				}
				hold()
				return
			}
			lastRecv.Store(time.Now().UnixNano())
			if err := sw.Write(f.Type, f.Payload); err != nil {
				return
			}
		}
	}()
	if cl.ping > 0 {
		t := time.NewTicker(cl.ping)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRecv.Load())) > cl.silence {
					// A silent transport is a dropped one.
					hold()
					_ = cl.c.Close()
					<-done
					return
				}
				_ = cl.w.Write(podwire.FramePing, nil)
			}
		}
	}
	<-done
}
