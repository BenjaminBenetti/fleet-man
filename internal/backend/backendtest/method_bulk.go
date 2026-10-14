package backendtest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// bulkTimeout bounds one bulk transfer.
const bulkTimeout = 15 * time.Minute

// randomBytes returns n deterministic pseudo-random bytes for seed.
func randomBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	_, _ = newContent(seed).Read(b)
	return b
}

// newContent is the deterministic byte stream for seed.
func newContent(seed uint64) *rand.ChaCha8 {
	var key [32]byte
	key[0], key[1], key[2], key[3] = byte(seed), byte(seed>>8), byte(seed>>16), byte(seed>>24)
	return rand.NewChaCha8(key)
}

// treeEntry is one entry of the tar round trip's tree.
type treeEntry struct {
	name string
	kind byte // tar.TypeDir, tar.TypeReg or tar.TypeSymlink
	mode int64
	size int64
	seed uint64
	link string
	sum  [32]byte // set when the entry is written
}

// conformanceTree is the tree the tar round trip sends: total bytes of binary
// files, plus files in every mode the cases care about, an empty file, names
// with spaces, deep nesting, a private folder and symlinks (relative, dangling
// and absolute). Folders precede their contents.
func conformanceTree(total int64) []*treeEntry {
	dir := func(name string, mode int64) *treeEntry { return &treeEntry{name: name, kind: tar.TypeDir, mode: mode} }
	file := func(name string, mode, size int64, seed uint64) *treeEntry {
		return &treeEntry{name: name, kind: tar.TypeReg, mode: mode, size: size, seed: seed}
	}
	link := func(name, to string) *treeEntry {
		return &treeEntry{name: name, kind: tar.TypeSymlink, mode: 0o777, link: to}
	}
	tree := []*treeEntry{
		dir("bin", 0o755),
		dir("modes", 0o755),
		file("modes/exec.sh", 0o755, 64, 101),
		file("modes/secret", 0o600, 512, 102),
		file("modes/group", 0o640, 4096, 103),
		file("modes/readonly", 0o444, 100, 104),
		dir("private", 0o700),
		file("private/inside", 0o600, 1000, 105),
		file("empty", 0o644, 0, 0),
		dir("nested", 0o755),
		dir("nested/a", 0o755),
		dir("nested/a/b", 0o755),
		dir("nested/a/b/c", 0o755),
		file("nested/a/b/c/deep.txt", 0o644, 33, 106),
		dir("dir with space", 0o755),
		file("dir with space/file with spaces.txt", 0o644, 777, 107),
		dir("links", 0o755),
		link("links/to-exec", "../modes/exec.sh"),
		link("links/dangling", "does-not-exist"),
		link("links/absolute", "/etc/hostname"),
	}
	const blobs = 8
	for n := 0; n < blobs; n++ {
		size := total / blobs
		if n == blobs-1 {
			size = total - size*(blobs-1)
		}
		tree = append(tree, file(fmt.Sprintf("bin/blob-%d.bin", n), 0o644, size, uint64(200+n)))
	}
	return tree
}

// writeTree writes tree as a tar stream to w, recording each file's sha256.
func writeTree(w io.Writer, tree []*treeEntry) error {
	tw := tar.NewWriter(w)
	modTime := time.Unix(1700000000, 0)
	for _, e := range tree {
		hdr := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode, ModTime: modTime, Linkname: e.link}
		if e.kind == tar.TypeDir {
			hdr.Name += "/"
		}
		if e.kind == tar.TypeReg {
			hdr.Size = e.size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if e.kind != tar.TypeReg {
			continue
		}
		h := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(tw, h), newContent(e.seed), e.size); err != nil {
			return err
		}
		copy(e.sum[:], h.Sum(nil))
	}
	return tw.Close()
}

// readTree reads a tar stream, keyed by clean relative name.
func readTree(r io.Reader) (map[string]*treeEntry, error) {
	got := map[string]*treeEntry{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		e := &treeEntry{name: name, kind: hdr.Typeflag, mode: hdr.Mode & 0o7777, size: hdr.Size, link: hdr.Linkname}
		if e.kind == tar.TypeReg {
			h := sha256.New()
			if _, err := io.Copy(h, tr); err != nil {
				return got, err
			}
			copy(e.sum[:], h.Sum(nil))
		}
		got[name] = e
	}
}

// caseExecTar: a binary tree of Sizes.Tree bytes goes into the instance as a
// tar on an exec's stdin and comes back out as one on stdout unchanged:
// contents, permission bits and symlinks.
func caseExecTar(c *C) {
	i := c.Shared()
	dir := c.Scratch(i) + "/tree"
	tree := conformanceTree(c.Sizes().Tree)

	start := time.Now()
	in := i.B.ExecCommandQuiet(i.WorkspaceDir(), ShArgv(`mkdir -p "$1" && tar -xpf - -C "$1"`, dir)).Cmd
	pr, pw := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := writeTree(pw, tree)
		_ = pw.CloseWithError(err)
		written <- err
	}()
	r := runCmd(in, pr, bulkTimeout)
	_ = pr.CloseWithError(errors.New("tar in finished"))
	if werr := <-written; werr != nil && r.Err == nil {
		c.Fatalf("send the tree: %v", werr)
	}
	if r.Err != nil {
		c.Fatalf("tar into the instance: %s", r)
	}
	c.Logf("tar in: %d MiB in %s", c.Sizes().Tree>>20, time.Since(start).Round(time.Millisecond))

	start = time.Now()
	out := i.B.ExecCommandQuiet(i.WorkspaceDir(), ShArgv(`tar -cf - -C "$1" .`, dir)).Cmd
	var got map[string]*treeEntry
	r = streamOut(out, func(stdout io.Reader) error {
		var err error
		got, err = readTree(stdout)
		return err
	})
	if r.Err != nil {
		c.Fatalf("tar out of the instance: %s", r)
	}
	c.Logf("tar out: %d MiB in %s", c.Sizes().Tree>>20, time.Since(start).Round(time.Millisecond))

	for _, want := range tree {
		g, ok := got[want.name]
		switch {
		case !ok:
			c.Errorf("%s: missing after the round trip", want.name)
			continue
		case g.kind != want.kind:
			c.Errorf("%s: type %q came back %q", want.name, want.kind, g.kind)
			continue
		}
		switch want.kind {
		case tar.TypeSymlink:
			if g.link != want.link {
				c.Errorf("%s: symlink to %q came back to %q", want.name, want.link, g.link)
			}
		case tar.TypeReg:
			if g.size != want.size || g.sum != want.sum {
				c.Errorf("%s: %d bytes came back as %d bytes with different content", want.name, want.size, g.size)
			}
			fallthrough
		default:
			if g.mode&0o777 != want.mode&0o777 {
				c.Errorf("%s: mode %04o came back %04o", want.name, want.mode, g.mode)
			}
		}
		delete(got, want.name)
	}
	if len(got) > 0 {
		extra := make([]string, 0, len(got))
		for name := range got {
			extra = append(extra, name)
		}
		sort.Strings(extra)
		c.Errorf("entries the tree did not have came back: %s", strings.Join(extra, ", "))
	}
}

// streamOut runs cmd, hands its stdout to consume while it runs, and returns
// how it ended (stderr captured; Stdout left empty).
func streamOut(cmd *exec.Cmd, consume func(io.Reader) error) Result {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Code: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return Result{Code: -1, Err: err}
	}
	timer := time.AfterFunc(bulkTimeout, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	consumeErr := consume(stdout)
	_, _ = io.Copy(io.Discard, stdout)
	err = cmd.Wait()
	if err == nil && consumeErr != nil {
		err = consumeErr
	}
	return Result{Stderr: stderr.Bytes(), Code: exitCode(err), Err: err}
}

// caseExecPipeOut: one command writes Sizes.Pipe bytes to stdout and every
// byte arrives, in order.
func caseExecPipeOut(c *C) {
	i := c.Shared()
	n := c.Sizes().Pipe
	const line = "fleet-conformance\n"
	start := time.Now()
	var got int64
	var mismatchAt int64 = -1
	cmd := i.B.ExecCommandQuiet(i.WorkspaceDir(), ShArgv(`yes fleet-conformance | head -c "$1"`, strconv.FormatInt(n, 10))).Cmd
	r := streamOut(cmd, func(stdout io.Reader) error {
		buf := make([]byte, 64<<10)
		for {
			k, err := stdout.Read(buf)
			for _, b := range buf[:k] {
				if mismatchAt < 0 && b != line[got%int64(len(line))] {
					mismatchAt = got
				}
				got++
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
	if r.Err != nil {
		c.Fatalf("pipe out: %s", r)
	}
	if got != n || mismatchAt >= 0 {
		c.Errorf("piped out %d bytes, want %d (first wrong byte at %d)", got, n, mismatchAt)
	}
	c.Logf("piped out %d MiB in %s", n>>20, time.Since(start).Round(time.Millisecond))
}

// caseCopyFile: CopyFile streams Sizes.Copy bytes in, creating the parent
// folders, with the mode asked for; copying again replaces the file and its
// mode; nothing else is left in the folder.
func caseCopyFile(c *C) {
	i := c.Shared()
	folder := c.Scratch(i) + "/deep/new"
	dest := folder + "/copy.bin"
	data := randomBytes(int(c.Sizes().Copy), 3)

	start := time.Now()
	if err := i.B.CopyFile(i.WorkspaceDir(), bytes.NewReader(data), dest, 0o640); err != nil {
		c.Fatalf("CopyFile: %v", err)
	}
	c.Logf("CopyFile: %d MiB in %s", len(data)>>20, time.Since(start).Round(time.Millisecond))

	want := sha256.Sum256(data)
	var gotSum [32]byte
	var gotLen int64
	r := streamOut(i.B.ExecCommandQuiet(i.WorkspaceDir(), []string{"cat", dest}).Cmd, func(stdout io.Reader) error {
		h := sha256.New()
		var err error
		gotLen, err = io.Copy(h, stdout)
		copy(gotSum[:], h.Sum(nil))
		return err
	})
	if r.Err != nil {
		c.Fatalf("read the copy back: %s", r)
	}
	if gotLen != int64(len(data)) || gotSum != want {
		c.Errorf("the copy reads back %d bytes, want the %d sent, unchanged", gotLen, len(data))
	}
	checkMode(c, i, dest, "640")

	again := []byte("replaced\n")
	if err := i.B.CopyFile(i.WorkspaceDir(), bytes.NewReader(again), dest, 0o755); err != nil {
		c.Fatalf("CopyFile over an existing file: %v", err)
	}
	if got := c.Sh(i, `cat "$1"`, dest); got != "replaced" {
		c.Errorf("after copying over it the file reads %q, want %q", got, "replaced")
	}
	checkMode(c, i, dest, "755")
	if got := c.Sh(i, `ls -A "$1"`, folder); got != "copy.bin" {
		c.Errorf("the destination folder holds %q, want only copy.bin (no temp files)", got)
	}
}

// checkMode fails the case unless path's permission bits are exactly mode
// (octal, find -perm form).
func checkMode(c *C, i *Instance, path, mode string) {
	c.Helper()
	if got := c.Sh(i, `find "$1" -prune -perm "$2"`, path, mode); got != path {
		c.Errorf("%s: mode is not %s (%s)", path, mode, c.Sh(i, `ls -ld "$1"`, path))
	}
}
