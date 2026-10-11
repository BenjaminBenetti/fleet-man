package sharesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// RemoteHub is what a spoke's connection needs of the hub (*Hub).
type RemoteHub interface {
	Attach(ctx context.Context, info SideInfo, peer Peer, hello Hello) (Welcome, error)
	Report(ctx context.Context, side string, r Report) (Reply, error)
}

// RemoteOptions configure ServeRemote.
type RemoteOptions struct {
	// OpenBulk opens a connection to the spoke (in a pod: a stream on the
	// bulk session) for content over InlineMax, both ways. nil: such content
	// cannot travel, and the op fails until it can.
	OpenBulk func(ctx context.Context) (net.Conn, error)
	// Accept vets the spoke's lease epoch before it is attached (a spoke
	// whose epoch is older than one already seen for the same folders
	// lost its lease); an error refuses it.
	Accept func(lease uint64) error
	// Attached hears that the side is attached, with the spoke's lease.
	Attached func(lease uint64)
	// HelloTimeout bounds the wait for the spoke's whole hello: 2 minutes.
	HelloTimeout time.Duration
	// Logger gets the connection's warnings; nil discards them.
	Logger *slog.Logger
}

// ServeRemote serves the spoke on conn as the side info names, which is the
// caller's own record of whose spoke it is (never something the spoke
// says): it reads the spoke's hello, attaches it to hub, then carries its
// reports and is its Peer until conn ends or ctx does, and returns why. It
// does not detach the side when it ends: the hub backs off a side it cannot
// reach, and a later connection of the same side replaces it (Hub.Attach),
// so the caller decides when a side is gone for good.
func ServeRemote(ctx context.Context, conn net.Conn, hub RemoteHub, info SideInfo, opts RemoteOptions) error {
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = 2 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &remoteSide{
		w: newWire(conn), hub: hub, info: info, opts: opts, ctx: ctx,
		hellos: make(chan wireMsg, 8), batches: map[uint64]*pendingBatch{},
	}
	defer r.w.fail(nil)
	stop := context.AfterFunc(ctx, func() { r.w.fail(ctx.Err()) })
	defer stop()
	go r.read()

	hello, lease, final, err := r.awaitHello()
	if err != nil {
		if final != 0 {
			r.w.answer(final, wireMsg{T: msgRefused, Err: err.Error()})
		}
		return err
	}
	if opts.Accept != nil {
		if err := opts.Accept(lease); err != nil {
			r.w.answer(final, wireMsg{T: msgRefused, Err: err.Error()})
			return err
		}
	}
	w, err := hub.Attach(ctx, info, r, hello)
	if err != nil {
		r.w.answer(final, wireMsg{T: msgRefused, Err: err.Error()})
		return err
	}
	r.w.answer(final, wireMsg{T: msgWelcome, Welcome: &w})
	if opts.Attached != nil {
		opts.Attached(lease)
	}
	<-r.w.done
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.w.Err()
}

// remoteSide is the hub's end of a spoke's connection: the side's Peer.
type remoteSide struct {
	w    *wire
	hub  RemoteHub
	info SideInfo
	opts RemoteOptions
	ctx  context.Context

	hellos chan wireMsg
	// reports and needs count the spoke's calls being served
	// (maxRemoteReports, maxRemoteNeeds).
	reports, needs atomic.Int32

	mu       sync.Mutex
	attached bool
	batches  map[uint64]*pendingBatch
}

// pendingBatch is a batch the spoke is applying: whose content it may ask
// for, and with what context.
type pendingBatch struct {
	ctx     context.Context
	content ContentFunc
	ops     map[string]Op
}

// read reads the spoke's messages until the connection ends.
func (r *remoteSide) read() {
	for {
		m, err := r.w.recv()
		if err != nil {
			r.w.fail(err)
			return
		}
		if answers[m.T] {
			r.w.deliver(m)
			continue
		}
		switch m.T {
		case msgHello:
			r.mu.Lock()
			attached := r.attached
			r.mu.Unlock()
			if attached {
				r.w.fail(errors.New("sharesync: the spoke said hello twice"))
				return
			}
			select {
			case r.hellos <- m:
			case <-r.w.done:
				return
			}
		case msgReport:
			if r.reports.Add(1) > maxRemoteReports {
				r.reports.Add(-1)
				r.w.answerErr(m.ID, errBusy)
				continue
			}
			go func() {
				defer r.reports.Add(-1)
				r.report(m)
			}()
		case msgNeed:
			if r.needs.Add(1) > maxRemoteNeeds {
				r.needs.Add(-1)
				r.w.answerErr(m.ID, errBusy)
				continue
			}
			go func() {
				defer r.needs.Add(-1)
				r.need(m)
			}()
		default:
			if m.ID != 0 {
				r.w.answerErr(m.ID, fmt.Errorf("sharesync: the hub does not take %q", m.T))
			}
		}
	}
}

// errBusy refuses a spoke's call past what the hub serves of it at once.
var errBusy = errors.New("sharesync: the hub is serving as many of the spoke's calls as it takes at once; try again")

// awaitHello reads the spoke's hello, chunk by chunk, bounded. final is the
// ID of its last chunk, which the answer goes to (0 when none came).
func (r *remoteSide) awaitHello() (Hello, uint64, uint64, error) {
	timer := time.NewTimer(r.opts.HelloTimeout)
	defer timer.Stop()
	roots := map[string]int{}
	var hello Hello
	var lease uint64
	held := 0
	first := true
	for {
		var m wireMsg
		select {
		case m = <-r.hellos:
		case <-r.w.done:
			return Hello{}, 0, 0, r.w.Err()
		case <-timer.C:
			return Hello{}, 0, 0, errors.New("sharesync: the spoke sent no whole hello in time")
		}
		if first {
			lease, first = m.Lease, false
		}
		if m.Hello != nil {
			if err := checkHello(m.Hello, &held); err != nil {
				return Hello{}, 0, m.ID, err
			}
			for _, rh := range m.Hello.Roots {
				i, ok := roots[rh.Root]
				if !ok {
					roots[rh.Root] = len(hello.Roots)
					hello.Roots = append(hello.Roots, RootHello{Root: rh.Root, Epoch: rh.Epoch})
					i = len(hello.Roots) - 1
				}
				hello.Roots[i].Held = append(hello.Roots[i].Held, rh.Held...)
			}
		}
		if !m.More {
			if m.ID == 0 {
				return Hello{}, 0, 0, errors.New("sharesync: the spoke's last hello is not a call")
			}
			r.mu.Lock()
			r.attached = true
			r.mu.Unlock()
			return hello, lease, m.ID, nil
		}
	}
}

// checkHello bounds what a spoke's hello may claim; held counts its paths
// across chunks.
func checkHello(h *Hello, held *int) error {
	for _, rh := range h.Roots {
		if !validPath(rh.Root) || len(rh.Epoch) > maxEpoch {
			return fmt.Errorf("sharesync: the spoke's hello names root %q", rh.Root)
		}
		*held += len(rh.Held)
		if *held > MaxRemoteHeld {
			return fmt.Errorf("sharesync: the spoke's hello names over %d paths", MaxRemoteHeld)
		}
		for _, x := range rh.Held {
			if x.Rev > maxRemoteRev {
				return fmt.Errorf("sharesync: the spoke claims rev %d of %s in %s", x.Rev, x.Path, rh.Root)
			}
		}
	}
	return nil
}

// checkReport bounds what a spoke's report may claim.
func checkReport(rep *Report) error {
	if !validPath(rep.Root) || len(rep.Epoch) > maxEpoch {
		return fmt.Errorf("sharesync: the spoke reports for root %q", rep.Root)
	}
	if len(rep.Changes) > maxReportChanges {
		return fmt.Errorf("sharesync: the spoke reports %d changes at once", len(rep.Changes))
	}
	for _, c := range rep.Changes {
		if c.Base > maxRemoteRev {
			return fmt.Errorf("sharesync: the spoke claims base rev %d of %s in %s", c.Base, c.Path, rep.Root)
		}
	}
	return nil
}

// report hands one of the spoke's reports to the hub.
func (r *remoteSide) report(m wireMsg) {
	if m.Report == nil {
		r.w.answerErr(m.ID, errors.New("sharesync: a report without changes"))
		return
	}
	if err := checkReport(m.Report); err != nil {
		r.opts.Logger.Warn("sharesync: refused a spoke's report", "side", r.info.ID, "err", err)
		r.w.answerErr(m.ID, err)
		return
	}
	reply, err := r.hub.Report(r.ctx, r.info.ID, *m.Report)
	if err != nil {
		r.w.answerErr(m.ID, err)
		return
	}
	r.w.answer(m.ID, wireMsg{T: msgReply, Reply: &reply})
}

// need serves the content of an op of a batch the spoke is applying: inline
// when small, else on a bulk connection.
func (r *remoteSide) need(m wireMsg) {
	n := m.Need
	if n == nil {
		r.w.answerErr(m.ID, errors.New("sharesync: a need without an op"))
		return
	}
	r.mu.Lock()
	pb := r.batches[n.Batch]
	r.mu.Unlock()
	if pb == nil || pb.content == nil {
		r.w.answerErr(m.ID, fmt.Errorf("sharesync: no batch %d is being applied: %w", n.Batch, ErrStale))
		return
	}
	op, ok := pb.ops[n.Op.Path]
	if !ok || op.Rev != n.Op.Rev || !op.Entry.Same(n.Op.Entry) || op.Entry.Kind != File || n.Offset < 0 || n.Offset > op.Entry.Size {
		r.w.answerErr(m.ID, fmt.Errorf("sharesync: %s is not an op of batch %d", n.Op.Path, n.Batch))
		return
	}
	rc, err := pb.content(pb.ctx, op, n.Offset)
	if err != nil {
		r.w.answerErr(m.ID, err)
		return
	}
	data, whole, err := inline(rc)
	if err != nil {
		_ = rc.Close()
		r.w.answerErr(m.ID, err)
		return
	}
	if whole {
		_ = rc.Close()
		r.w.answer(m.ID, wireMsg{T: msgData, Data: data})
		return
	}
	if r.opts.OpenBulk == nil {
		_ = rc.Close()
		r.w.answerErr(m.ID, fmt.Errorf("sharesync: %s is over %d bytes and no bulk connection reaches the spoke", op.Path, InlineMax))
		return
	}
	conn, err := r.opts.OpenBulk(pb.ctx)
	if err != nil {
		_ = rc.Close()
		r.w.answerErr(m.ID, fmt.Errorf("sharesync: a bulk connection to the spoke: %w", err))
		return
	}
	if err := writeMsg(conn, BulkHeader{Kind: BulkPut, Token: m.ID}); err != nil {
		_ = rc.Close()
		_ = conn.Close()
		r.w.answerErr(m.ID, err)
		return
	}
	go func() {
		defer conn.Close()
		defer rc.Close()
		// A spoke that stopped reading holds the copy no longer than its
		// control connection lasts.
		stop := context.AfterFunc(r.ctx, func() { _ = conn.Close() })
		defer stop()
		_, _ = io.Copy(conn, io.MultiReader(bytes.NewReader(data), rc))
	}()
	r.w.answer(m.ID, wireMsg{T: msgData, Bulk: true})
}

// Apply is Peer.Apply.
func (r *remoteSide) Apply(ctx context.Context, b Batch) ([]Result, error) {
	id := r.w.id()
	pb := &pendingBatch{ctx: ctx, content: b.Content, ops: make(map[string]Op, len(b.Ops))}
	for _, op := range b.Ops {
		pb.ops[op.Path] = op
	}
	r.mu.Lock()
	r.batches[id] = pb
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.batches, id)
		r.mu.Unlock()
	}()
	wb := b
	wb.Content = nil
	a, err := r.w.call(ctx, id, wireMsg{T: msgApply, Batch: &wb})
	if err != nil {
		return nil, err
	}
	if a.T != msgResults || len(a.Results) != len(b.Ops) {
		return nil, fmt.Errorf("sharesync: the spoke answered a %d-op batch with %d results", len(b.Ops), len(a.Results))
	}
	return a.Results, nil
}

// Fetch is Peer.Fetch: inline when small, else over a bulk connection.
func (r *remoteSide) Fetch(ctx context.Context, root, path string, e Entry, offset int64) (io.ReadCloser, error) {
	g := &wireGet{Root: root, Path: path, Entry: e, Offset: offset}
	if r.opts.OpenBulk == nil || e.Size-offset <= InlineMax {
		a, err := r.w.call(ctx, 0, wireMsg{T: msgFetch, Get: g})
		if err != nil {
			return nil, err
		}
		if a.T != msgData || a.Bulk || len(a.Data) > InlineMax {
			return nil, fmt.Errorf("sharesync: the spoke answered a fetch of %s with %s", path, a.T)
		}
		return io.NopCloser(bytes.NewReader(a.Data)), nil
	}
	conn, err := r.opts.OpenBulk(ctx)
	if err != nil {
		return nil, fmt.Errorf("sharesync: a bulk connection to the spoke: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := writeMsg(conn, BulkHeader{Kind: BulkGet, Get: g}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var st wireMsg
	if err := readMsg(conn, &st); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sharesync: fetching %s from the spoke: %w", path, err)
	}
	if st.T == msgError {
		_ = conn.Close()
		return nil, st.error()
	}
	if st.T != msgData || !st.Bulk {
		_ = conn.Close()
		return nil, fmt.Errorf("sharesync: the spoke answered a fetch of %s with %s", path, st.T)
	}
	return &connReader{Reader: io.LimitReader(conn, e.Size-offset), conn: conn}, nil
}

// Rescan is Peer.Rescan.
func (r *remoteSide) Rescan(ctx context.Context) error {
	a, err := r.w.call(ctx, 0, wireMsg{T: msgRescan})
	if err != nil {
		return err
	}
	if a.T != msgDone {
		return fmt.Errorf("sharesync: the spoke answered a rescan with %s", a.T)
	}
	return nil
}
