package podwire

// Ring keeps a byte stream's unacknowledged tail: the bytes from offset Base
// to End that were produced but not yet confirmed by the other side, so a
// re-attach can replay them. Its owner bounds it (HoldBuffer) by not
// appending while it is full. It is not safe for concurrent use.
type Ring struct {
	base int64
	buf  []byte
	off  int
}

// Base is the offset of the oldest byte kept.
func (r *Ring) Base() int64 { return r.base }

// End is the offset just past the newest byte.
func (r *Ring) End() int64 { return r.base + int64(len(r.buf)-r.off) }

// Len is how many bytes are kept.
func (r *Ring) Len() int { return len(r.buf) - r.off }

// Append keeps p after the newest byte.
func (r *Ring) Append(p []byte) {
	if r.off > 0 && r.off >= len(r.buf)/2 {
		n := copy(r.buf, r.buf[r.off:])
		r.buf = r.buf[:n]
		r.off = 0
	}
	r.buf = append(r.buf, p...)
}

// Trim drops every byte before offset to (an acknowledgement up to there).
// An offset outside what is kept is clamped.
func (r *Ring) Trim(to int64) {
	if to <= r.base {
		return
	}
	if to >= r.End() {
		r.base = r.End()
		r.buf, r.off = r.buf[:0], 0
		return
	}
	r.off += int(to - r.base)
	r.base = to
}

// From returns the kept bytes from offset off to End, and false when off is
// not within what is kept (before Base or past End). The slice is valid until
// the next Append or Trim.
func (r *Ring) From(off int64) ([]byte, bool) {
	if off < r.base || off > r.End() {
		return nil, false
	}
	return r.buf[r.off+int(off-r.base):], true
}
