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
	"time"
)

// SpokeOptions configure a Spoke.
type SpokeOptions struct {
	// Info names the side in its own logs; the hub names it for itself.
	Info SideInfo
	// Side configures the spoke's Side (its watchers and scans).
	Side SideOptions
	// Lease is the spoke's lease epoch on its folders, sent with its hello.
	Lease uint64
	// BulkWait bounds the wait for the bulk connection the hub sends large
	// content on: 1 minute.
	BulkWait time.Duration
	// Logger gets the spoke's warnings; nil discards them.
	Logger *slog.Logger
}

// Spoke is a side in another process than the hub's: a pod's Tree, attached
// to the daemon's hub over a control connection (ServeRemote's other end).
type Spoke struct {
	w    *wire
	tree *Tree
	opts SpokeOptions
	log  *slog.Logger
	side *Side

	welcomed chan struct{}

	mu      sync.Mutex
	queue   []wireMsg
	kick    chan struct{}
	waiters map[uint64]chan *bulkConn
}

// bulkConn is a bulk connection handed to the request waiting for it; done
// is closed once the request is done with it.
type bulkConn struct {
	conn net.Conn
	done chan struct{}
}

// NewSpoke returns the spoke of tree, talking to the hub over conn.
func NewSpoke(conn net.Conn, tree *Tree, opts SpokeOptions) *Spoke {
	if opts.BulkWait <= 0 {
		opts.BulkWait = time.Minute
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Side.Logger == nil {
		opts.Side.Logger = log
	}
	return &Spoke{
		w: newWire(conn), tree: tree, opts: opts, log: log,
		welcomed: make(chan struct{}), kick: make(chan struct{}, 1), waiters: map[uint64]chan *bulkConn{},
	}
}

// Run attaches the spoke's tree to the hub (the tree's hello, the hub's
// welcome), then runs its side, which watches and scans the folders and
// reports to the hub, and serves the hub's requests (apply, fetch, rescan),
// until the connection ends or ctx does. It returns why.
func (s *Spoke) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.w.fail(nil)
	stop := context.AfterFunc(ctx, func() { s.w.fail(ctx.Err()) })
	defer stop()
	go s.read(ctx)

	if err := s.hello(ctx); err != nil {
		return err
	}
	s.side = NewSide(s.opts.Info, s.tree, s, s.opts.Side)
	close(s.welcomed)
	go s.applyLoop(ctx)
	go func() {
		if err := s.side.Run(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("sharesync: the spoke's side ended", "err", err)
		}
	}()
	select {
	case <-s.w.done:
		return s.w.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hello sends the tree's hello in chunks, the last one a call the hub
// answers with its welcome, which the tree takes.
func (s *Spoke) hello(ctx context.Context) error {
	chunks := chunkHello(s.tree.Hello())
	for i, h := range chunks {
		m := wireMsg{T: msgHello, Hello: &h, More: i < len(chunks)-1}
		if i == 0 {
			m.Lease = s.opts.Lease
		}
		if m.More {
			if err := s.w.send(m); err != nil {
				return err
			}
			continue
		}
		a, err := s.w.call(ctx, 0, m)
		if err != nil {
			return err
		}
		if a.T == msgRefused {
			return fmt.Errorf("sharesync: the hub refused the spoke: %s", a.Err)
		}
		if a.T != msgWelcome || a.Welcome == nil {
			return fmt.Errorf("sharesync: the hub answered the spoke's hello with %s", a.T)
		}
		return s.tree.Welcome(*a.Welcome)
	}
	return nil
}

// chunkHello splits a hello into messages of at most helloChunk held paths
// (always at least one message).
func chunkHello(h Hello) []Hello {
	var out []Hello
	cur := Hello{}
	n := 0
	flush := func() {
		out = append(out, cur)
		cur, n = Hello{}, 0
	}
	for _, rh := range h.Roots {
		held := rh.Held
		for {
			take := min(len(held), helloChunk-n)
			cur.Roots = append(cur.Roots, RootHello{Root: rh.Root, Epoch: rh.Epoch, Held: held[:take]})
			n += take
			held = held[take:]
			if n == helloChunk {
				flush()
			}
			if len(held) == 0 {
				break
			}
		}
	}
	if n > 0 || len(cur.Roots) > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// read reads the hub's messages until the connection ends.
func (s *Spoke) read(ctx context.Context) {
	for {
		m, err := s.w.recv()
		if err != nil {
			s.w.fail(err)
			return
		}
		if answers[m.T] {
			s.w.deliver(m)
			continue
		}
		switch m.T {
		case msgApply:
			// In order: the hub sends a side one batch at a time, and each
			// must land after the one before it.
			s.mu.Lock()
			s.queue = append(s.queue, m)
			s.mu.Unlock()
			select {
			case s.kick <- struct{}{}:
			default:
			}
		case msgFetch:
			go s.fetch(ctx, m)
		case msgRescan:
			go s.rescan(ctx, m)
		default:
			if m.ID != 0 {
				s.w.answerErr(m.ID, fmt.Errorf("sharesync: the spoke does not take %q", m.T))
			}
		}
	}
}

// applyLoop applies the hub's batches in the order they came.
func (s *Spoke) applyLoop(ctx context.Context) {
	for {
		s.mu.Lock()
		var m wireMsg
		ok := len(s.queue) > 0
		if ok {
			m = s.queue[0]
			s.queue = s.queue[1:]
		}
		s.mu.Unlock()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.w.done:
				return
			case <-s.kick:
			}
			continue
		}
		s.apply(ctx, m)
	}
}

// apply applies one batch, fetching its content from the hub.
func (s *Spoke) apply(ctx context.Context, m wireMsg) {
	if m.Batch == nil {
		s.w.answerErr(m.ID, errors.New("sharesync: an apply without a batch"))
		return
	}
	b := *m.Batch
	b.Content = s.contentOf(m.ID)
	results, err := s.side.Apply(ctx, b)
	if err != nil {
		s.w.answerErr(m.ID, err)
		return
	}
	s.w.answer(m.ID, wireMsg{T: msgResults, Results: results})
}

// contentOf is the content of batch's ops, asked of the hub.
func (s *Spoke) contentOf(batch uint64) ContentFunc {
	return func(ctx context.Context, op Op, offset int64) (io.ReadCloser, error) {
		id := s.w.id()
		ch := make(chan *bulkConn, 1)
		s.mu.Lock()
		s.waiters[id] = ch
		s.mu.Unlock()
		drop := func() {
			s.mu.Lock()
			delete(s.waiters, id)
			s.mu.Unlock()
		}
		a, err := s.w.call(ctx, id, wireMsg{T: msgNeed, Need: &wireNeed{Batch: batch, Op: op, Offset: offset}})
		if err != nil {
			drop()
			return nil, err
		}
		if !a.Bulk {
			drop()
			return io.NopCloser(bytes.NewReader(a.Data)), nil
		}
		timer := time.NewTimer(s.opts.BulkWait)
		defer timer.Stop()
		select {
		case bc := <-ch:
			drop()
			return &connReader{Reader: io.LimitReader(bc.conn, op.Entry.Size-offset), conn: bc.conn, done: func() { close(bc.done) }}, nil
		case <-timer.C:
			drop()
			return nil, fmt.Errorf("sharesync: the content of %s did not arrive within %s", op.Path, s.opts.BulkWait)
		case <-ctx.Done():
			drop()
			return nil, ctx.Err()
		}
	}
}

// fetch answers the hub's fetch of small content inline.
func (s *Spoke) fetch(ctx context.Context, m wireMsg) {
	g := m.Get
	if g == nil {
		s.w.answerErr(m.ID, errors.New("sharesync: a fetch of nothing"))
		return
	}
	if !s.awaitWelcome() {
		return
	}
	rc, err := s.side.Fetch(ctx, g.Root, g.Path, g.Entry, g.Offset)
	if err != nil {
		s.w.answerErr(m.ID, err)
		return
	}
	defer rc.Close()
	data, whole, err := inline(rc)
	switch {
	case err != nil:
		s.w.answerErr(m.ID, err)
	case !whole:
		s.w.answerErr(m.ID, fmt.Errorf("sharesync: %s is over %d bytes: fetch it on a bulk connection: %w", g.Path, InlineMax, ErrStale))
	default:
		s.w.answer(m.ID, wireMsg{T: msgData, Data: data})
	}
}

// rescan answers the hub's rescan once the side has scanned and reported.
func (s *Spoke) rescan(ctx context.Context, m wireMsg) {
	if !s.awaitWelcome() {
		return
	}
	if err := s.side.Rescan(ctx); err != nil {
		s.w.answerErr(m.ID, err)
		return
	}
	s.w.answer(m.ID, wireMsg{T: msgDone})
}

// awaitWelcome waits until the spoke is attached; false when the connection
// ended first.
func (s *Spoke) awaitWelcome() bool {
	select {
	case <-s.welcomed:
		return true
	case <-s.w.done:
		return false
	}
}

// Report is the side's Reporter: its changes, sent to the hub.
func (s *Spoke) Report(ctx context.Context, _ string, r Report) (Reply, error) {
	a, err := s.w.call(ctx, 0, wireMsg{T: msgReport, Report: &r})
	if err != nil {
		return Reply{}, err
	}
	if a.T != msgReply || a.Reply == nil {
		return Reply{}, fmt.Errorf("sharesync: the hub answered a report with %s", a.T)
	}
	return *a.Reply, nil
}

// ServeBulk serves one of the hub's bulk connections: the content of an op
// the spoke asked for (BulkPut), or content the hub fetches (BulkGet). It
// returns once the connection is done with.
func (s *Spoke) ServeBulk(ctx context.Context, conn net.Conn) error {
	var h BulkHeader
	if err := readMsg(conn, &h); err != nil {
		_ = conn.Close()
		return fmt.Errorf("sharesync: a bulk connection's header: %w", err)
	}
	switch h.Kind {
	case BulkPut:
		s.mu.Lock()
		ch := s.waiters[h.Token]
		s.mu.Unlock()
		if ch == nil {
			_ = conn.Close()
			return fmt.Errorf("sharesync: bulk content for request %d, which nothing waits for", h.Token)
		}
		bc := &bulkConn{conn: conn, done: make(chan struct{})}
		select {
		case ch <- bc:
		default:
			_ = conn.Close()
			return fmt.Errorf("sharesync: bulk content for request %d came twice", h.Token)
		}
		select {
		case <-bc.done:
		case <-ctx.Done():
			_ = conn.Close()
		case <-s.w.done:
			_ = conn.Close()
		}
		return nil
	case BulkGet:
		defer conn.Close()
		g := h.Get
		if g == nil {
			return errors.New("sharesync: a bulk fetch of nothing")
		}
		if !s.awaitWelcome() {
			return s.w.Err()
		}
		rc, err := s.side.Fetch(ctx, g.Root, g.Path, g.Entry, g.Offset)
		if err != nil {
			return writeMsg(conn, wireMsg{T: msgError, Err: err.Error(), Stale: errors.Is(err, ErrStale)})
		}
		defer rc.Close()
		if err := writeMsg(conn, wireMsg{T: msgData, Bulk: true}); err != nil {
			return err
		}
		_, err = io.Copy(conn, rc)
		return err
	}
	_ = conn.Close()
	return fmt.Errorf("sharesync: a bulk connection of kind %q", h.Kind)
}
