package jobboot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := []struct {
		t FrameType
		p []byte
	}{
		{FrameHello, []byte(`{"jobboot":1}`)},
		{FrameData, bytes.Repeat([]byte{0, 0xff}, 1000)},
		{FrameAck, nil},
		{FrameData, make([]byte, MaxFrame)},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f.t, f.p); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range frames {
		typ, payload, err := ReadFrame(&buf)
		if err != nil || typ != f.t || !bytes.Equal(payload, f.p) && len(f.p) > 0 {
			t.Fatalf("read %s %d bytes %v, want %s %d bytes", typ, len(payload), err, f.t, len(f.p))
		}
	}
	if _, _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("clean end = %v, want io.EOF", err)
	}
}

func TestFrameErrors(t *testing.T) {
	if err := WriteFrame(io.Discard, FrameType('?'), nil); err == nil {
		t.Error("an unknown type was written")
	}
	if err := WriteFrame(io.Discard, FrameData, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversized write: %v", err)
	}
	huge := []byte{byte(FrameData), 0, 0, 0, 0}
	binary.BigEndian.PutUint32(huge[1:], MaxFrame+1)
	if _, _, err := ReadFrame(bytes.NewReader(huge)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversized read: %v", err)
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{'?', 0, 0, 0, 0})); err == nil {
		t.Error("an unknown type was read")
	}
	var buf bytes.Buffer
	_ = WriteFrame(&buf, FrameData, []byte("hello"))
	cut := buf.Bytes()[:buf.Len()-2]
	if _, _, err := ReadFrame(bytes.NewReader(cut)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a frame cut short = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{'D', 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a header cut short = %v, want io.ErrUnexpectedEOF", err)
	}
}

func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteJSON(&buf, FrameHello, Hello{Proto: 1})
	f.Add(buf.Bytes())
	f.Add([]byte{'D', 0, 0, 0, 1, 'x'})
	f.Add([]byte{'D', 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		for {
			typ, payload, err := ReadFrame(r)
			if err != nil {
				return
			}
			if !typ.valid() || len(payload) > MaxFrame {
				t.Fatalf("read an invalid frame %s of %d bytes", typ, len(payload))
			}
		}
	})
}

func TestTerminationFitsAndRoundTrips(t *testing.T) {
	long := strings.Repeat("é", MaxTermination)
	term := Termination{OK: false, SHA256: strings.Repeat("a", 64), Bytes: 1 << 40, Error: long}
	msg := term.Encode()
	if len(msg) >= MaxTermination {
		t.Fatalf("encoded termination is %d bytes, over the kubelet's %d", len(msg), MaxTermination)
	}
	got, err := ParseTermination(string(msg) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Bytes != term.Bytes || got.SHA256 != term.SHA256 || !strings.HasPrefix(long, got.Error) || !utf8.ValidString(got.Error) {
		t.Fatalf("round trip: %+v", got)
	}
	ok := Termination{OK: true, SHA256: "x", Bytes: 5}
	if got, _ := ParseTermination(string(ok.Encode())); got != ok {
		t.Fatalf("got %+v want %+v", got, ok)
	}
	if _, err := ParseTermination("not json"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestWriteTermination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	if err := WriteTermination(path, Termination{OK: true, Bytes: 3}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if got, err := ParseTermination(string(data)); err != nil || !got.OK || got.Bytes != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := WriteTermination(filepath.Join(t.TempDir(), "missing", "log"), Termination{}); err != nil {
		t.Fatalf("a pod without a termination log: %v", err)
	}
}
