//go:build linux

package execas

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// probeTimeout is how long the CLI waits for a probe before it gives up
	// and runs the command without the user's environment.
	probeTimeout = 10 * time.Second
	// probeMaxAge bounds how long a cached probe is trusted while no startup
	// file changes: startup files source others (nvm.sh, ~/.cargo/env) the
	// fingerprint does not see.
	probeMaxAge = 5 * time.Minute
	// probeWaitDelay is how long a probe that has exited may keep its output
	// pipe open (a daemon its .bashrc started holds it).
	probeWaitDelay = time.Second
	// probeMemMax caps the in-memory cache: one entry per user, shell and
	// starting environment.
	probeMemMax = 64
	// maxProbeOutput caps what a probe may print.
	maxProbeOutput = 4 << 20
	// maxFingerprintDirEntries caps the files fingerprinted per directory
	// (/etc/profile.d and the like).
	maxFingerprintDirEntries = 256
)

// Prober runs the devcontainer CLI's userEnvProbe and caches the result. A
// cached environment is used while the shell's startup files (and the
// target's Deps) are unchanged (their fingerprint: size, times and inode of
// each) and it is younger than
// probeMaxAge, so a command in the exec class costs a shell start only after
// something changed. The cache lives in memory and, when cacheDir is usable, in
// files there, so one-shot `fleet exec-as` runs share it with the agent.
type Prober struct {
	cacheDir string
	timeout  time.Duration
	maxAge   time.Duration
	now      func() time.Time
	run      func(ctx context.Context, t ProbeTarget) (map[string]string, bool)

	mu       sync.Mutex
	mem      map[string]probeEntry
	inflight map[string]*probeCall
}

// probeEntry is one cached probe, in memory and on disk.
type probeEntry struct {
	Fingerprint string            `json:"fingerprint"`
	At          time.Time         `json:"at"`
	Env         map[string]string `json:"env"`
}

// probeCall is a probe in flight that callers for the same key wait on.
type probeCall struct {
	done chan struct{}
	env  map[string]string
	// cancelled is set when the probe stopped because its own caller's
	// context ended. That says nothing about the shell, so a waiter whose
	// context is still live probes again instead of going without.
	cancelled bool
}

// NewProber is a Prober caching under cacheDir ("" caches in memory only). The
// directory is created mode 0700 if its parent exists, and used only while it
// is this user's and private: the cache holds environments that a planted
// file would let someone else choose.
func NewProber(cacheDir string) *Prober {
	p := &Prober{
		cacheDir: cacheDir,
		timeout:  probeTimeout,
		maxAge:   probeMaxAge,
		now:      time.Now,
		mem:      make(map[string]probeEntry),
		inflight: make(map[string]*probeCall),
	}
	p.run = p.probe
	return p
}

// ProbeEnv returns the environment t's shell exports, from the cache when it is
// still good, else from a probe; nil when the probe failed or timed out.
// Concurrent calls for one target share one probe.
func (p *Prober) ProbeEnv(ctx context.Context, t ProbeTarget) map[string]string {
	key := probeKey(t)
	fingerprint := rcFingerprint(t)

	p.mu.Lock()
	if e, ok := p.mem[key]; ok && p.fresh(e, fingerprint) {
		p.mu.Unlock()
		return maps.Clone(e.Env)
	}
	if call, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			if call.cancelled && ctx.Err() == nil {
				return p.ProbeEnv(ctx, t)
			}
			return maps.Clone(call.env)
		case <-ctx.Done():
			return nil
		}
	}
	call := &probeCall{done: make(chan struct{})}
	p.inflight[key] = call
	p.mu.Unlock()

	entry, ok := p.load(key, fingerprint)
	if !ok {
		var env map[string]string
		if env, ok = p.run(ctx, t); ok {
			entry = probeEntry{Fingerprint: fingerprint, At: p.now(), Env: env}
			p.save(key, entry)
		}
	}

	p.mu.Lock()
	delete(p.inflight, key)
	if ok {
		p.remember(key, entry)
		call.env = entry.Env
	} else {
		call.cancelled = ctx.Err() != nil
	}
	p.mu.Unlock()
	close(call.done)
	return maps.Clone(call.env)
}

// fresh reports whether e may stand in for a probe now.
func (p *Prober) fresh(e probeEntry, fingerprint string) bool {
	age := p.now().Sub(e.At)
	return e.Fingerprint == fingerprint && age >= 0 && age < p.maxAge
}

// remember caches e in memory, evicting the oldest entry when full. Callers
// hold p.mu.
func (p *Prober) remember(key string, e probeEntry) {
	if _, ok := p.mem[key]; !ok && len(p.mem) >= probeMemMax {
		var oldest string
		for k, v := range p.mem {
			if oldest == "" || v.At.Before(p.mem[oldest].At) {
				oldest = k
			}
		}
		delete(p.mem, oldest)
	}
	p.mem[key] = e
}

// probeKey names a probe by everything that changes its answer but the
// startup files: who runs which shell how, from which environment and
// directory.
func probeKey(t ProbeTarget) string {
	data, _ := json.Marshal(struct {
		Mode     ProbeMode
		Shell    string
		Identity Identity
		Switch   bool
		Env      []string
		Dir      string
	}{t.Mode, t.Shell, t.Identity, t.Switch, t.Env, t.Dir})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// rcFingerprint hashes the state of every file a bash, zsh, sh or fish
// startup reads for t's user, and of the shell itself: path, size, mode,
// mtime, ctime and inode, or that it is missing. Drop-in directories count
// with each file in them. Editing, replacing, adding or removing any of them
// changes the fingerprint.
func rcFingerprint(t ProbeTarget) string {
	env := newEnvList(t.Env)
	home := t.Identity.Home
	xdg := filepath.Join(home, ".config")
	if v, ok := env.get("XDG_CONFIG_HOME"); ok && filepath.IsAbs(v) {
		xdg = v
	}
	files := []string{
		t.Shell,
		"/etc/environment", "/etc/profile", "/etc/bash.bashrc", "/etc/bashrc",
		"/etc/zshenv", "/etc/zprofile", "/etc/zshrc", "/etc/zlogin",
		"/etc/zsh/zshenv", "/etc/zsh/zprofile", "/etc/zsh/zshrc", "/etc/zsh/zlogin",
		"/etc/fish/config.fish",
		filepath.Join(home, ".profile"), filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".bash_login"), filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshenv"), filepath.Join(home, ".zprofile"),
		filepath.Join(home, ".zshrc"), filepath.Join(home, ".zlogin"),
		filepath.Join(xdg, "fish", "config.fish"),
	}
	for _, name := range []string{"ENV", "BASH_ENV"} {
		if v, ok := env.get(name); ok && filepath.IsAbs(v) {
			files = append(files, v)
		}
	}
	if zdot, ok := env.get("ZDOTDIR"); ok && filepath.IsAbs(zdot) {
		for _, f := range []string{".zshenv", ".zprofile", ".zshrc", ".zlogin"} {
			files = append(files, filepath.Join(zdot, f))
		}
	}
	dirs := []string{"/etc/profile.d", "/etc/fish/conf.d", filepath.Join(xdg, "fish", "conf.d")}

	h := sha256.New()
	for _, f := range append(files, t.Deps...) {
		writeFileState(h, f)
	}
	for _, d := range dirs {
		writeFileState(h, d)
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		if len(entries) > maxFingerprintDirEntries {
			entries = entries[:maxFingerprintDirEntries]
		}
		for _, e := range entries {
			writeFileState(h, filepath.Join(d, e.Name()))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeFileState writes one file's state line to w.
func writeFileState(w io.Writer, file string) {
	fi, err := os.Stat(file)
	if err != nil {
		fmt.Fprintf(w, "%s\x00-\n", file)
		return
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	if st == nil {
		fmt.Fprintf(w, "%s\x00%o\x00%d\x00%d\n", file, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return
	}
	fmt.Fprintf(w, "%s\x00%o\x00%d\x00%d.%d\x00%d.%d\x00%d\x00%d\n", file, fi.Mode(), fi.Size(),
		st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec, st.Ino, st.Dev)
}

// probe runs the CLI's probe: the shell prints its environment between two
// random marks from /proc/self/environ, or from printenv when that printed
// nothing. A shell that fails, times out or prints no marks gives nothing, and
// the command runs without the user's environment.
func (p *Prober) probe(ctx context.Context, t ProbeTarget) (map[string]string, bool) {
	mark := randomMark()
	for _, dump := range []struct {
		script string
		sep    byte
	}{{probeDumpProc, 0}, {probeDumpPrintenv, '\n'}} {
		out, ok := p.runShell(ctx, t, probeScript(mark, dump.script))
		if !ok {
			return nil, false
		}
		if env, ok := parseProbeOutput(out, mark, dump.sep); ok {
			return env, true
		}
	}
	return nil, false
}

// runShell runs one probe script in t's shell as t's user, its stdin
// /dev/null, and returns what it printed; ok is false when it could not start,
// failed, or was killed for running past the timeout or ctx.
func (p *Prober) runShell(ctx context.Context, t ProbeTarget, script string) ([]byte, bool) {
	pathEnv, _ := newEnvList(t.Env).get("PATH")
	exe, err := lookPath(t.Shell, pathEnv, t.Dir, t.Identity)
	if err != nil {
		return nil, false
	}
	out := &cappedBuffer{max: maxProbeOutput}
	proc, err := Start(&Plan{
		Class:    ClassExec,
		Identity: t.Identity,
		Switch:   t.Switch,
		Path:     exe,
		Argv:     probeArgs(t.Mode, t.Shell, script),
		Env:      t.Env,
		Dir:      t.Dir,
	}, Stdio{Stdout: out, WaitDelay: probeWaitDelay})
	if err != nil {
		return nil, false
	}
	type result struct {
		status ExitStatus
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := proc.Wait()
		done <- result{status, err}
	}()
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	var r result
	select {
	case r = <-done:
	case <-timer.C:
		proc.Kill()
		<-done
		return nil, false
	case <-ctx.Done():
		proc.Kill()
		<-done
		return nil, false
	}
	if r.status.Code != 0 || r.status.Signal != 0 {
		return nil, false
	}
	return out.Bytes(), true
}

// randomMark is a probe's output delimiter, which nothing a startup file
// prints can contain.
func randomMark() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// cappedBuffer keeps the first max bytes written to it and drops the rest.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// errCacheUnusable is a cache directory that is missing, not this user's or
// readable by others.
var errCacheUnusable = errors.New("probe cache directory is not private to this user")

// openCache opens the cache directory, creating it when create is set, and
// checks the directory it actually opened: owned by this user, no group or
// other access. Checking the open handle, not the path, means a directory
// swapped in by a user who can write the parent is refused, not trusted.
func (p *Prober) openCache(create bool) (*os.Root, error) {
	if p.cacheDir == "" {
		return nil, errCacheUnusable
	}
	if create {
		if err := os.Mkdir(p.cacheDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	root, err := os.OpenRoot(p.cacheDir)
	if err != nil {
		return nil, err
	}
	fi, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0o077 != 0 {
		root.Close()
		return nil, errCacheUnusable
	}
	return root, nil
}

func cacheFileName(key string) string { return "probe-" + key[:32] + ".json" }

// load reads key's cached probe from disk, when it is still good.
func (p *Prober) load(key, fingerprint string) (probeEntry, bool) {
	root, err := p.openCache(false)
	if err != nil {
		return probeEntry{}, false
	}
	defer root.Close()
	f, err := root.Open(cacheFileName(key))
	if err != nil {
		return probeEntry{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxProbeOutput+1))
	if err != nil || len(data) > maxProbeOutput {
		return probeEntry{}, false
	}
	var e probeEntry
	if json.Unmarshal(data, &e) != nil || !p.fresh(e, fingerprint) {
		return probeEntry{}, false
	}
	return e, true
}

// save writes key's probe to disk, atomically; a cache that cannot be written
// is skipped.
func (p *Prober) save(key string, e probeEntry) {
	root, err := p.openCache(true)
	if err != nil {
		return
	}
	defer root.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	tmp := ".tmp-" + randomMark()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || root.Rename(tmp, cacheFileName(key)) != nil {
		_ = root.Remove(tmp)
	}
}
