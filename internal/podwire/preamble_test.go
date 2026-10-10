package podwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

func TestPreambleRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	h := ExecHeader{
		Request:   execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", "echo hi"}, Env: map[string]string{"A": "b"}, TTY: true, Rows: 24, Cols: 80},
		Resumable: true,
		PodUID:    "uid-1",
	}
	if err := WritePreamble(&buf, TagExec, h); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("frames follow")
	tag, raw, err := ReadPreamble(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if tag != TagExec {
		t.Fatalf("tag %v", tag)
	}
	var got ExecHeader
	if err := DecodeHeader(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Class != execas.ClassScript || strings.Join(got.Argv, " ") != "sh -c echo hi" || got.Env["A"] != "b" ||
		!got.TTY || got.Rows != 24 || got.Cols != 80 || !got.Resumable || got.PodUID != "uid-1" {
		t.Fatalf("header %+v", got)
	}
	if rest := buf.String(); rest != "frames follow" {
		t.Fatalf("ReadPreamble read past the header: left %q", rest)
	}
}

func TestReadPreambleRefuses(t *testing.T) {
	preamble := func(tag Tag, body string) []byte {
		b := []byte{byte(tag), 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(len(body)))
		return append(b, body...)
	}
	over := []byte{byte(TagExec), 0, 0, 0, 0}
	binary.BigEndian.PutUint32(over[1:], MaxHeader+1)
	cases := map[string]struct {
		in   []byte
		want string
	}{
		"empty header":     {preamble(TagRPC, ""), "empty"},
		"over MaxHeader":   {over, "over"},
		"not JSON":         {preamble(TagRPC, "nope"), "not a JSON object"},
		"JSON array":       {preamble(TagRPC, "[1]"), "not a JSON object"},
		"JSON null":        {preamble(TagRPC, "null"), "not a JSON object"},
		"truncated header": {preamble(TagRPC, `{"method":"x"}`)[:10], "unexpected EOF"},
		"truncated tag":    {[]byte{byte(TagRPC), 0}, "unexpected EOF"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ReadPreamble(bytes.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error about %q", err, tc.want)
			}
		})
	}
	if _, _, err := ReadPreamble(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("empty stream: %v, want io.EOF", err)
	}
	if err := WritePreamble(io.Discard, TagRPC, RPCHeader{Method: strings.Repeat("m", MaxHeader)}); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("writing an oversized header: %v", err)
	}
}

func TestExecHeaderValidate(t *testing.T) {
	id := strings.Repeat("ab", 16)
	cases := map[string]struct {
		h  ExecHeader
		ok bool
	}{
		"command":            {ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"true"}}}, true},
		"no argv":            {ExecHeader{Request: execas.Request{Class: execas.ClassExec}}, false},
		"re-attach":          {ExecHeader{Request: execas.Request{Class: execas.ClassScript}, Resume: id}, true},
		"re-attach bad id":   {ExecHeader{Request: execas.Request{Class: execas.ClassScript}, Resume: "AB"}, false},
		"re-attach no class": {ExecHeader{Resume: id}, false},
		"re-attach offsets":  {ExecHeader{Request: execas.Request{Class: execas.ClassScript}, Resume: id, Stdout: -1}, false},
	}
	for name, tc := range cases {
		if err := tc.h.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTagNames(t *testing.T) {
	for tag := range tagNames {
		if !tag.Known() || strings.HasPrefix(tag.String(), "tag-") {
			t.Errorf("%#x: %q", byte(tag), tag)
		}
		if StreamCap(tag) <= 0 {
			t.Errorf("%s has no stream cap", tag)
		}
	}
	if Tag(0x7e).Known() || Tag(0x7e).String() != "tag-0x7e" {
		t.Fatal("unknown tag")
	}
	// The relay kinds' caps are the plan's.
	for tag, want := range map[Tag]int{TagSockControl: 32, TagSockAgent: 64, TagSockMCP: 16, TagSockHost: 32} {
		if got := StreamCap(tag); got != want {
			t.Errorf("%s cap %d, want %d", tag, got, want)
		}
	}
}

// FuzzReadPreamble never panics, and a preamble it accepts carries a JSON
// object of at most MaxHeader bytes.
func FuzzReadPreamble(f *testing.F) {
	var seed bytes.Buffer
	_ = WritePreamble(&seed, TagExec, ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"true"}}})
	f.Add(seed.Bytes())
	f.Add([]byte{byte(TagControl), 0, 0, 0, 2, '{', '}'})
	f.Add([]byte{byte(TagControl), 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, in []byte) {
		tag, raw, err := ReadPreamble(bytes.NewReader(in))
		if err != nil {
			return
		}
		if len(raw) == 0 || len(raw) > MaxHeader || !isJSONObject(raw) {
			t.Fatalf("accepted %d bytes %q", len(raw), raw)
		}
		var re bytes.Buffer
		re.WriteByte(byte(tag))
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(raw)))
		re.Write(n[:])
		re.Write(raw)
		if !bytes.HasPrefix(in, re.Bytes()) {
			t.Fatal("preamble does not re-encode to the input")
		}
		// Every header type decodes without panicking.
		var eh ExecHeader
		_ = DecodeHeader(raw, &eh)
		_ = eh.Validate()
		var fw FileWriteHeader
		_ = DecodeHeader(raw, &fw)
		var rh RPCHeader
		_ = DecodeHeader(raw, &rh)
		var ch ControlHeader
		_ = DecodeHeader(raw, &ch)
	})
}
