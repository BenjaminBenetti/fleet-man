package podwire

import (
	"bytes"
	"io"
	"math/rand/v2"
	"testing"
	"time"
)

func TestNegotiate(t *testing.T) {
	cases := []struct {
		aMin, aMax, bMin, bMax int
		want                   int
		ok                     bool
	}{
		{1, 1, 1, 1, 1, true},
		{1, 3, 2, 5, 3, true},
		{2, 5, 1, 3, 3, true},
		{1, 2, 3, 4, 0, false},
		{3, 4, 1, 2, 0, false},
		{0, 1, 1, 1, 0, false},
		{2, 1, 1, 1, 0, false},
	}
	for _, tc := range cases {
		got, err := Negotiate(tc.aMin, tc.aMax, tc.bMin, tc.bMax)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("Negotiate(%d-%d, %d-%d) = %d, %v", tc.aMin, tc.aMax, tc.bMin, tc.bMax, got, err)
		}
	}
}

func TestRing(t *testing.T) {
	var r Ring
	r.Append([]byte("hello "))
	r.Append([]byte("world"))
	if r.Base() != 0 || r.End() != 11 || r.Len() != 11 {
		t.Fatalf("base %d end %d len %d", r.Base(), r.End(), r.Len())
	}
	if b, ok := r.From(6); !ok || string(b) != "world" {
		t.Fatalf("From(6) = %q %v", b, ok)
	}
	r.Trim(6)
	if r.Base() != 6 || r.Len() != 5 {
		t.Fatalf("after trim: base %d len %d", r.Base(), r.Len())
	}
	if _, ok := r.From(5); ok {
		t.Fatal("From before Base")
	}
	if _, ok := r.From(12); ok {
		t.Fatal("From past End")
	}
	if b, ok := r.From(11); !ok || len(b) != 0 {
		t.Fatalf("From(End) = %q %v", b, ok)
	}
	r.Trim(3) // behind Base: nothing
	r.Trim(100)
	if r.Base() != 11 || r.Len() != 0 {
		t.Fatalf("trim past end: base %d len %d", r.Base(), r.Len())
	}
	r.Append([]byte("!"))
	if b, _ := r.From(11); string(b) != "!" {
		t.Fatalf("%q", b)
	}
}

// TestRingAgainstModel appends and trims at random and checks the ring
// against the whole stream.
func TestRingAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var r Ring
	var stream []byte
	for range 5000 {
		if rng.IntN(3) > 0 {
			chunk := make([]byte, rng.IntN(100))
			for i := range chunk {
				chunk[i] = byte(rng.IntN(256))
			}
			r.Append(chunk)
			stream = append(stream, chunk...)
		} else {
			r.Trim(r.Base() + int64(rng.IntN(r.Len()+1)))
		}
		got, ok := r.From(r.Base())
		if !ok || r.End() != int64(len(stream)) || !bytes.Equal(got, stream[r.Base():]) {
			t.Fatalf("ring diverged at base %d end %d", r.Base(), r.End())
		}
	}
}

func TestPipe(t *testing.T) {
	p := newPipe(4)
	wrote := make(chan struct{})
	go func() {
		_, _ = p.Write([]byte("abcdefgh"))
		close(wrote)
	}()
	select {
	case <-wrote:
		t.Fatal("a write over the buffer did not wait for the reader")
	case <-time.After(50 * time.Millisecond):
	}
	got, err := io.ReadAll(io.LimitReader(p, 8))
	if err != nil || string(got) != "abcdefgh" {
		t.Fatalf("%q %v", got, err)
	}
	<-wrote
	p.closeWrite(io.ErrUnexpectedEOF)
	if _, err := p.Read(make([]byte, 1)); err != io.ErrUnexpectedEOF {
		t.Fatalf("after closeWrite: %v", err)
	}

	q := newPipe(4)
	q.closeRead()
	if n, err := q.Write([]byte("discarded once the reader is gone")); err != nil || n != 33 {
		t.Fatalf("write after closeRead: %d %v", n, err)
	}
	if _, err := q.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after closeRead: %v", err)
	}
}
