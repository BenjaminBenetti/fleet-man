package podwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	frames := []Frame{
		{Type: FrameData, Payload: []byte("hello")},
		{Type: FrameStderr, Payload: []byte{0, 1, 2, 255}},
		{Type: FrameEOF},
		{Type: FrameResize, Payload: []byte(`{"rows":24,"cols":80}`)},
		{Type: FrameSignal, Payload: []byte(`{"signal":"INT"}`)},
		{Type: FrameExit, Payload: []byte(`{"code":3}`)},
		{Type: FrameOpen, Payload: []byte(`{"pid":7}`)},
		{Type: FramePing},
		{Type: FrameResume, Payload: []byte(`{"stdin":1,"stdout":2,"stderr":3}`)},
		{Type: FrameAbort},
		{Type: FrameType(0x7f), Payload: []byte("from a newer peer")},
	}
	for _, f := range frames {
		if err := w.Write(f.Type, f.Payload); err != nil {
			t.Fatal(err)
		}
	}
	r := NewFrameReader(&buf)
	for i, want := range frames {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d: got %v %q, want %v %q", i, got.Type, got.Payload, want.Type, want.Payload)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the last frame: %v, want io.EOF", err)
	}
}

func TestFrameReaderRefusesBadFrames(t *testing.T) {
	big := make([]byte, frameHeader)
	big[0] = byte(FrameData)
	binary.BigEndian.PutUint32(big[1:], MaxFrame+1)
	cases := map[string]struct {
		in   []byte
		want error
	}{
		"over MaxFrame":       {big, ErrFrameTooLarge},
		"truncated header":    {[]byte{byte(FrameData), 0, 0}, io.ErrUnexpectedEOF},
		"truncated payload":   {[]byte{byte(FrameData), 0, 0, 0, 5, 'a', 'b'}, io.ErrUnexpectedEOF},
		"clean end of stream": {nil, io.EOF},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewFrameReader(bytes.NewReader(tc.in)).Next()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if err := NewFrameWriter(io.Discard).Write(FrameData, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("writing an oversized frame: %v", err)
	}
}

func TestWriteDataChunks(t *testing.T) {
	var buf bytes.Buffer
	data := bytes.Repeat([]byte("x"), 2*DataChunk+10)
	if err := NewFrameWriter(&buf).WriteData(FrameStderr, data); err != nil {
		t.Fatal(err)
	}
	r := NewFrameReader(&buf)
	var sizes []int
	var got []byte
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if f.Type != FrameStderr {
			t.Fatalf("type %v", f.Type)
		}
		sizes = append(sizes, len(f.Payload))
		got = append(got, f.Payload...)
	}
	if !bytes.Equal(got, data) || len(sizes) != 3 || sizes[0] != DataChunk || sizes[2] != 10 {
		t.Fatalf("chunks %v", sizes)
	}
}

// TestFrameWriterNeverInterleaves writes frames from many goroutines and
// checks every one arrives whole.
func TestFrameWriterNeverInterleaves(t *testing.T) {
	pr, pw := io.Pipe()
	w := NewFrameWriter(pw)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('a' + g)}, 1000+g)
			for range 50 {
				_ = w.Write(FrameData, payload)
			}
		}()
	}
	go func() {
		wg.Wait()
		_ = pw.Close()
	}()
	r := NewFrameReader(pr)
	n := 0
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c := f.Payload[0]
		if len(f.Payload) != 1000+int(c-'a') || strings.Trim(string(f.Payload), string(c)) != "" {
			t.Fatalf("frame mixes writers: %d bytes starting %q", len(f.Payload), c)
		}
		n++
	}
	if n != 400 {
		t.Fatalf("%d frames", n)
	}
}

func TestDecodeFrame(t *testing.T) {
	var st ExitStatus
	if err := DecodeFrame(Frame{Type: FrameExit, Payload: []byte(`{"code":2,"signal":"TERM","new_field":1}`)}, &st); err != nil {
		t.Fatal(err)
	}
	if st.Code != 2 || st.Signal != "TERM" {
		t.Fatalf("%+v", st)
	}
	st = ExitStatus{Code: 9}
	if err := DecodeFrame(Frame{Type: FrameExit}, &st); err != nil || st.Code != 9 {
		t.Fatalf("empty payload changed the value: %+v %v", st, err)
	}
	if err := DecodeFrame(Frame{Type: FrameExit, Payload: []byte("{")}, &st); err == nil {
		t.Fatal("bad JSON decoded")
	}
}

// FuzzFrameReader feeds arbitrary bytes to the frame reader: it never panics,
// never returns a payload over MaxFrame, and what it returns re-encodes to the
// bytes it consumed.
func FuzzFrameReader(f *testing.F) {
	var seed bytes.Buffer
	w := NewFrameWriter(&seed)
	_ = w.Write(FrameData, []byte("abc"))
	_ = w.Write(FrameExit, []byte(`{"code":0}`))
	_ = w.Write(FramePing, nil)
	f.Add(seed.Bytes())
	f.Add([]byte{byte(FrameData), 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewFrameReader(bytes.NewReader(in))
		var re bytes.Buffer
		rw := NewFrameWriter(&re)
		for {
			fr, err := r.Next()
			if err != nil {
				break
			}
			if len(fr.Payload) > MaxFrame {
				t.Fatalf("payload of %d bytes", len(fr.Payload))
			}
			if err := rw.Write(fr.Type, fr.Payload); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.HasPrefix(in, re.Bytes()) {
			t.Fatal("frames do not re-encode to the input")
		}
	})
}
