package gateway

import (
	"io"
	"net"
	"sync"
)

// splice connects two conns, copying bytes in both directions until either side
// ends, then closing both. It is byte-transparent: it never parses the payload,
// which is exactly why it can carry native HTTP/2 (gRPC) — including bidi and
// server-streaming — unmodified. gRPC half-close, flow control, and trailers are
// in-band h2 frames the splice forwards verbatim; teardown happens only when one
// io.Copy returns, i.e. the whole connection closed (GOAWAY / peer done), never
// per-RPC.
func splice(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeBoth()
	}()
	wg.Wait()
}
