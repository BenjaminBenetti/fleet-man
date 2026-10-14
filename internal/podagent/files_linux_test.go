//go:build linux

package podagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// leftovers lists what is in dir besides want: a write must leave no temp
// file behind.
func leftovers(t *testing.T, dir string, want ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var extra []string
outer:
	for _, e := range entries {
		for _, w := range want {
			if e.Name() == w {
				continue outer
			}
		}
		extra = append(extra, e.Name())
	}
	return extra
}

func TestFileWriteAndRead(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	ctx := context.Background()
	dir := filepath.Join(d.run, "new", "parents")
	path := filepath.Join(dir, "f.txt")
	data := []byte("hello, pod\n")
	if err := s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Mode: 0o751, Size: int64(len(data)), SHA256: sum(data)}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o751 {
		t.Fatalf("written file %v %v", fi, err)
	}
	if extra := leftovers(t, dir, "f.txt"); extra != nil {
		t.Fatalf("left behind %v", extra)
	}
	got, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: path})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q %v", got, err)
	}
	// Overwrites atomically; an unknown size is fine.
	if err := s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Size: -1}, strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "v2" {
		t.Fatalf("%q", b)
	}
	f, err := s.c.OpenFile(ctx, podwire.FileReadHeader{Path: path})
	if err != nil || f.Info.Size != 2 || f.Info.Mode.Perm() != 0o644 {
		t.Fatalf("open %+v %v", f, err)
	}
	b, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(b) != "v2" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestFileErrors(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	ctx := context.Background()
	big := filepath.Join(d.run, "big")
	if err := os.MkdirAll(d.run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, podwire.SmallFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		err  error
		code string
	}{
		"read missing": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: filepath.Join(d.run, "nope")})
			return err
		}(), podwire.CodeNotFound},
		"read a directory": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: d.run})
			return err
		}(), podwire.CodeInvalid},
		"read relative": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: "etc/passwd"})
			return err
		}(), podwire.CodeInvalid},
		"read unclean": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: d.run + "/../x"})
			return err
		}(), podwire.CodeInvalid},
		"read over the agent session's size": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: big})
			return err
		}(), podwire.CodeTooLarge},
		"read over Max": {func() error {
			_, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: filepath.Join(d.spec), Max: 3})
			return err
		}(), podwire.CodeTooLarge},
		"write declared over the agent session's size": {s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: filepath.Join(d.run, "w1"), Size: podwire.SmallFile + 1}, bytes.NewReader(nil)), podwire.CodeTooLarge},
		"write sent over the agent session's size":     {s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: filepath.Join(d.run, "w2"), Size: -1}, bytes.NewReader(make([]byte, podwire.SmallFile+1))), podwire.CodeTooLarge},
		"write a sha that does not match":              {s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: filepath.Join(d.run, "w3"), SHA256: sum([]byte("other"))}, strings.NewReader("data")), podwire.CodeMismatch},
		"write a size that does not match":             {s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: filepath.Join(d.run, "w4"), Size: 10}, strings.NewReader("data")), podwire.CodeMismatch},
	}
	for name, tc := range cases {
		if !podwire.IsCode(tc.err, tc.code) {
			t.Errorf("%s: %v, want code %s", name, tc.err, tc.code)
		}
	}
	if extra := leftovers(t, d.run, "big", heldDirName, agentDirName); extra != nil {
		t.Fatalf("failed writes left %v", extra)
	}
}

// TestFileWriteAbandoned: a write whose client goes away (its FIN without
// the EOF trailer, or its context) leaves nothing behind.
func TestFileWriteAbandoned(t *testing.T) {
	d := newTestDirs(t)
	s := startAgent(t, d.options())
	conn, err := s.c.Open(context.Background(), podwire.TagFileWrite, podwire.FileWriteHeader{Path: filepath.Join(d.run, "half"), Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	w := podwire.NewFrameWriter(conn)
	_ = w.Write(podwire.FrameData, []byte("partial"))
	_ = conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	slow := io.MultiReader(strings.NewReader("start"), blockingReader{ctx.Done()})
	if err := s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: filepath.Join(d.run, "cancelled"), Size: -1}, slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled write: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		extra := leftovers(t, d.run, heldDirName, agentDirName)
		if extra == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandoned writes left %v", extra)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockingReader blocks until done, then fails.
type blockingReader struct{ done <-chan struct{} }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.done
	return 0, errors.New("cancelled")
}
