package execas

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRequestFrameRoundTripLeavesTheRestOfTheStream(t *testing.T) {
	req := Request{
		Class: ClassExplicit,
		Argv:  []string{"/bin/sh", "-c", "echo 'a b'\nexit 3"},
		User:  "vscode:1000",
		Cwd:   "/workspaces/x",
		Env:   map[string]string{"SECRET": "s3cr3t", "MULTI": "line\nbreak"},
	}
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "exec-as/1 ") {
		t.Fatalf("frame starts %q", buf.String()[:12])
	}
	buf.WriteString("the command's own stdin\x00\xff")

	r := &oneByteReader{r: &buf}
	got, err := ReadRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, req)
	}
	rest, _ := io.ReadAll(&buf)
	if string(rest) != "the command's own stdin\x00\xff" {
		t.Fatalf("ReadRequest consumed past its frame: rest = %q", rest)
	}
}

// oneByteReader hands out at most one byte per Read, as a slow pipe may.
type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func TestShellWrittenFrameIsAccepted(t *testing.T) {
	// The header is text so a shell's printf can write one (the itests do).
	body := `{"class":"script","argv":["true"],"some_future_field":1}`
	frame := "exec-as/1 " + strconv.Itoa(len(body)) + "\n" + body
	got, err := ReadRequest(strings.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if got.Class != ClassScript || !reflect.DeepEqual(got.Argv, []string{"true"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestReadRequestRefusesBadFrames(t *testing.T) {
	valid := `{"class":"exec","argv":["true"]}`
	for _, tc := range []struct{ name, in, want string }{
		{"empty stream", "", "no exec request on stdin"},
		{"no newline", "exec-as/1 12", "unexpected EOF"},
		{"not a frame", "GET / HTTP/1.1\n", "not an exec request frame"},
		{"newer version", "exec-as/2 2\n{}", "unsupported exec request frame exec-as/2"},
		{"bad length", "exec-as/1 x\n", "bad exec request length"},
		{"signed length", "exec-as/1 +5\n", "bad exec request length"},
		{"zero length", "exec-as/1 0\n", "bad exec request length"},
		{"leading zero", "exec-as/1 01\n{", "bad exec request length"},
		{"too large", "exec-as/1 " + strconv.Itoa(MaxRequestSize+1) + "\n", "more than"},
		{"header too long", strings.Repeat("x", 100), "header longer than"},
		{"truncated body", "exec-as/1 100\n" + valid, "unexpected EOF"},
		{"not json", "exec-as/1 3\nabc", "parse exec request"},
		{"trailing data in the body", "exec-as/1 " + strconv.Itoa(len(valid)+2) + "\n" + valid + "{}", "parse exec request"},
		{"invalid request", "exec-as/1 2\n{}", "unknown exec class"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadRequest(strings.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestReadRequestPassesOnReadErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := ReadRequest(io.MultiReader(strings.NewReader("exec-a"), errReader{boom})); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestWriteRequestRefusesWhatReadRequestWould(t *testing.T) {
	if err := WriteRequest(io.Discard, Request{Class: "nope", Argv: []string{"x"}}); err == nil {
		t.Fatal("an invalid request was framed")
	}
	huge := Request{Class: ClassScript, Argv: []string{"sh", "-c", strings.Repeat("x", MaxRequestSize)}}
	if err := WriteRequest(io.Discard, huge); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("an oversize request: err = %v", err)
	}
}

func FuzzReadRequest(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteRequest(&buf, Request{Class: ClassExec, Argv: []string{"bash", "-l"}, Env: map[string]string{"A": "b"}, TTY: true, Rows: 24, Cols: 80})
	f.Add(buf.Bytes())
	f.Add([]byte("exec-as/1 2\n{}"))
	f.Add([]byte("exec-as/1 9999999\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := bytes.NewReader(in)
		req, err := ReadRequest(r)
		if err != nil {
			return
		}
		if verr := req.Validate(); verr != nil {
			t.Fatalf("ReadRequest returned an invalid request: %v", verr)
		}
		// A frame that parsed re-frames to something that parses the same.
		var out bytes.Buffer
		if err := WriteRequest(&out, req); err != nil {
			if strings.Contains(err.Error(), "more than") {
				return // JSON escaping grew a body at the cap past it
			}
			t.Fatalf("re-frame: %v", err)
		}
		again, err := ReadRequest(&out)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		a, _ := json.Marshal(req)
		b, _ := json.Marshal(again)
		if !bytes.Equal(a, b) {
			t.Fatalf("re-read differs:\n%s\n%s", a, b)
		}
	})
}
