package remote

import (
	"net"
	"testing"
	"time"
)

func TestChanListenerPushAccept(t *testing.T) {
	l := NewChanListener()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go l.Push(c1)
	got, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got != c1 {
		t.Fatal("Accept returned a different conn than was pushed")
	}
}

func TestChanListenerCloseUnblocksAccept(t *testing.T) {
	l := NewChanListener()
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	_ = l.Close()
	select {
	case err := <-done:
		if err != net.ErrClosed {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
	// Close is idempotent.
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestChanListenerPushAfterCloseClosesConn ensures a stream pushed after Close is
// not leaked.
func TestChanListenerPushAfterCloseClosesConn(t *testing.T) {
	l := NewChanListener()
	_ = l.Close()
	c1, c2 := net.Pipe()
	defer c2.Close()
	l.Push(c1) // should close c1
	if _, err := c1.Read(make([]byte, 1)); err == nil {
		t.Fatal("pushed-after-close conn should have been closed")
	}
}
