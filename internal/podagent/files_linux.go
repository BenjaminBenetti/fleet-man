//go:build linux

package podagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// asUser runs fn with id's filesystem identity (fsuid, fsgid and groups), so
// every permission check, symlink and created file is the user's own, as a
// write by `docker exec -u` would be. The identity is set on a dedicated OS
// thread that is never unlocked, so it dies with fn's goroutine and no other
// goroutine ever runs with it. Without switch (single-uid mode, or root
// itself) fn runs as this process.
func asUser(id execas.Identity, switchUser bool, fn func() error) error {
	if !switchUser || (id.UID == 0 && id.GID == 0) {
		return fn()
	}
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := setThreadFSIdentity(id); err != nil {
			errc <- err
			return
		}
		errc <- fn()
	}()
	return <-errc
}

// setThreadFSIdentity sets this thread's supplementary groups, fsgid and
// fsuid (raw per-thread syscalls: x/sys/unix's, not syscall's, which apply to
// every thread).
func setThreadFSIdentity(id execas.Identity) error {
	groups := make([]int, len(id.Groups))
	for i, g := range id.Groups {
		groups[i] = int(g)
	}
	if err := unix.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	_ = unix.Setfsgid(int(id.GID))
	if cur, _ := unix.SetfsgidRetGid(-1); cur != int(id.GID) {
		return fmt.Errorf("setfsgid %d: still %d", id.GID, cur)
	}
	_ = unix.Setfsuid(int(id.UID))
	if cur, _ := unix.SetfsuidRetUid(-1); cur != int(id.UID) {
		return fmt.Errorf("setfsuid %d: still %d", id.UID, cur)
	}
	return nil
}

// checkPath refuses a path that is not absolute and clean.
func checkPath(p string) error {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.IndexByte(p, 0) >= 0 || p == "/" {
		return fmt.Errorf("%q is not a clean absolute file path", p)
	}
	return nil
}

// fileCode is the wire code for a file error.
func fileCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return podwire.CodeNotFound
	case errors.Is(err, fs.ErrPermission):
		return podwire.CodeDenied
	}
	return podwire.CodeFailed
}

// resolveFileUser is whom a file operation runs as: class (default exec) and
// user, as an exec would.
func (a *agent) resolveFileUser(class execas.Class, user string) (execas.Identity, error) {
	if class == "" {
		class = execas.ClassExec
	}
	return a.host.Resolve(class, user)
}

// fileLimit is the most a file stream carries on this session.
func (a *agent) fileLimit() int64 {
	if a.opts.Role == podwire.RoleBulk {
		return 0
	}
	return podwire.SmallFile
}

// serveFileWrite writes one file: DATA into a temp file beside the target, as
// the user, checked against the EOF trailer (and the header), then renamed
// into place.
func (a *agent) serveFileWrite(s *podwire.Stream) {
	reply := func(res podwire.Result) { _ = s.W.WriteJSON(podwire.FrameExit, res) }
	var h podwire.FileWriteHeader
	if err := podwire.DecodeHeader(s.Header, &h); err != nil {
		s.Penalize()
		reply(podwire.Result{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	if err := checkPath(h.Path); err != nil {
		reply(podwire.Result{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	limit := a.fileLimit()
	if limit > 0 && h.Size > limit {
		reply(podwire.Result{Code: podwire.CodeTooLarge, Error: fmt.Sprintf("%d bytes is over the agent session's %d; use the bulk session", h.Size, limit)})
		return
	}
	id, err := a.resolveFileUser(h.Class, h.User)
	if err != nil {
		reply(podwire.Result{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	var res podwire.Result
	_ = asUser(id, !a.host.SingleUID, func() error {
		res = writeFile(h, s, limit)
		return nil
	})
	if res.Code != "abandoned" {
		reply(res)
	}
}

// writeFile is serveFileWrite's work, as the user.
func writeFile(h podwire.FileWriteHeader, s *podwire.Stream, limit int64) podwire.Result {
	fail := func(err error) podwire.Result { return podwire.Result{Code: fileCode(err), Error: err.Error()} }
	dir := filepath.Dir(h.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(h.Path)+".fleet-*")
	if err != nil {
		return fail(err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	sum := sha256.New()
	var n int64
	var trailer podwire.FileTrailer
	for {
		f, err := s.R.Next()
		if err != nil {
			// The client went away: nothing is written.
			return podwire.Result{Code: "abandoned"}
		}
		if f.Type == podwire.FrameAbort {
			return podwire.Result{Code: "abandoned"}
		}
		if f.Type == podwire.FrameEOF {
			if err := podwire.DecodeFrame(f, &trailer); err != nil {
				return podwire.Result{Code: podwire.CodeInvalid, Error: err.Error()}
			}
			break
		}
		if f.Type != podwire.FrameData {
			continue
		}
		n += int64(len(f.Payload))
		if limit > 0 && n > limit {
			return podwire.Result{Code: podwire.CodeTooLarge, Error: fmt.Sprintf("over the agent session's %d bytes; use the bulk session", limit)}
		}
		sum.Write(f.Payload)
		if _, err := tmp.Write(f.Payload); err != nil {
			return fail(err)
		}
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if trailer.Size != n || trailer.SHA256 != got || (h.SHA256 != "" && h.SHA256 != got) || (h.Size > 0 && h.Size != n) {
		return podwire.Result{Code: podwire.CodeMismatch, Error: fmt.Sprintf("received %d bytes sha256 %s, not what was sent", n, got)}
	}
	mode := h.Mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), h.Path); err != nil {
		return fail(err)
	}
	done = true
	return podwire.Result{Size: n, SHA256: got}
}

// serveFileRead streams one file out, as the user: OPEN with its size, DATA,
// then EXIT with what was sent.
func (a *agent) serveFileRead(s *podwire.Stream) {
	open := func(info podwire.FileInfo) { _ = s.W.WriteJSON(podwire.FrameOpen, info) }
	var h podwire.FileReadHeader
	if err := podwire.DecodeHeader(s.Header, &h); err != nil {
		s.Penalize()
		open(podwire.FileInfo{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	if err := checkPath(h.Path); err != nil {
		open(podwire.FileInfo{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	id, err := a.resolveFileUser(h.Class, h.User)
	if err != nil {
		open(podwire.FileInfo{Code: podwire.CodeInvalid, Error: err.Error()})
		return
	}
	limit := a.fileLimit()
	if h.Max > 0 && (limit == 0 || h.Max < limit) {
		limit = h.Max
	}
	// The client sends nothing more; its FIN (or ABORT) means it stopped
	// reading, and the rest of the file is not sent.
	var stopped atomic.Bool
	go func() {
		for {
			f, err := s.R.Next()
			if err != nil || f.Type == podwire.FrameAbort {
				stopped.Store(true)
				return
			}
		}
	}()
	_ = asUser(id, !a.host.SingleUID, func() error {
		readFile(h.Path, limit, s, &stopped)
		return nil
	})
}

// readFile is serveFileRead's work, as the user.
func readFile(path string, limit int64, s *podwire.Stream, stopped *atomic.Bool) {
	f, err := os.Open(path)
	if err != nil {
		_ = s.W.WriteJSON(podwire.FrameOpen, podwire.FileInfo{Code: fileCode(err), Error: err.Error()})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		_ = s.W.WriteJSON(podwire.FrameOpen, podwire.FileInfo{Code: fileCode(err), Error: err.Error()})
		return
	}
	if !fi.Mode().IsRegular() {
		_ = s.W.WriteJSON(podwire.FrameOpen, podwire.FileInfo{Code: podwire.CodeInvalid, Error: path + " is not a regular file"})
		return
	}
	if limit > 0 && fi.Size() > limit {
		_ = s.W.WriteJSON(podwire.FrameOpen, podwire.FileInfo{Code: podwire.CodeTooLarge, Error: fmt.Sprintf("%s is %d bytes, over %d", path, fi.Size(), limit)})
		return
	}
	if err := s.W.WriteJSON(podwire.FrameOpen, podwire.FileInfo{Size: fi.Size(), Mode: fi.Mode()}); err != nil {
		return
	}
	sum := sha256.New()
	buf := make([]byte, podwire.DataChunk)
	var n int64
	for !stopped.Load() {
		k, err := f.Read(buf)
		if k > 0 {
			n += int64(k)
			if limit > 0 && n > limit {
				_ = s.W.WriteJSON(podwire.FrameExit, podwire.Result{Code: podwire.CodeTooLarge, Error: path + " grew past the limit"})
				return
			}
			sum.Write(buf[:k])
			if werr := s.W.Write(podwire.FrameData, buf[:k]); werr != nil {
				return
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = s.W.WriteJSON(podwire.FrameExit, podwire.Result{Code: fileCode(err), Error: err.Error()})
			return
		}
	}
	_ = s.W.WriteJSON(podwire.FrameExit, podwire.Result{Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))})
}
