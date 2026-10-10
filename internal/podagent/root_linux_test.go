//go:build linux

package podagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// The AsRoot tests need root with CAP_SETUID/CAP_SETGID, as the agent has in a
// pod. CI runs them under sudo; FLEET_PODAGENT_REQUIRE_ROOT=1 turns the skip
// into a failure.
func requireRoot(t *testing.T) {
	t.Helper()
	if !execas.CanSwitchUser() {
		if os.Getenv("FLEET_PODAGENT_REQUIRE_ROOT") == "1" {
			t.Fatal("FLEET_PODAGENT_REQUIRE_ROOT=1 but this process cannot switch users")
		}
		t.Skip("needs root (CAP_SETUID and CAP_SETGID)")
	}
}

const (
	rootTestUID   = 4242
	rootTestGroup = 5555
)

// rootHost is a Host whose remoteUser is u4242 (primary group 4242,
// supplementary group shared=5555), from a passwd/group of the test's own.
func rootHost(t *testing.T, root string) *execas.Host {
	t.Helper()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	_ = os.MkdirAll(home, 0o755)
	_ = os.Chown(home, rootTestUID, rootTestUID)
	passwd := fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\nu4242:x:%d:%d::%s:/bin/sh\n", rootTestUID, rootTestUID, home)
	group := fmt.Sprintf("root:x:0:\ng4242:x:%d:\nshared:x:%d:u4242\ntty:x:5:\n", rootTestUID, rootTestGroup)
	if err := os.WriteFile(filepath.Join(etc, "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	return &execas.Host{
		Spec:       execas.Spec{RemoteUser: "u4242", UserEnvProbe: execas.ProbeNone},
		Root:       root,
		ProcessEnv: os.Environ(),
		Self:       execas.CurrentCreds(),
	}
}

func owner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Uid, st.Gid
}

// mkdir makes dir with mode and owner.
func mkdir(t *testing.T, dir string, mode os.FileMode, uid, gid int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}

func TestAsRootFilesRunAsTheUser(t *testing.T) {
	requireRoot(t)
	d := newTestDirs(t)
	o := d.options()
	o.Host = rootHost(t, filepath.Join(filepath.Dir(d.run), "fsroot"))
	s := startAgent(t, o)
	ctx := context.Background()
	base := filepath.Join(filepath.Dir(d.run), "files")
	mkdir(t, base, 0o755, 0, 0)
	userDir := filepath.Join(base, "user")
	mkdir(t, userDir, 0o755, rootTestUID, rootTestUID)
	groupDir := filepath.Join(base, "group")
	mkdir(t, groupDir, 0o775, 0, rootTestGroup)
	rootDir := filepath.Join(base, "root")
	mkdir(t, rootDir, 0o700, 0, 0)
	secret := filepath.Join(rootDir, "secret")
	if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A link the user planted towards a root-only directory.
	if err := os.Symlink(rootDir, filepath.Join(userDir, "link")); err != nil {
		t.Fatal(err)
	}
	write := func(path string, class execas.Class) error {
		return s.c.WriteFile(ctx, podwire.FileWriteHeader{Path: path, Size: -1, Class: class}, strings.NewReader("x"))
	}

	if err := write(filepath.Join(userDir, "sub", "f"), ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(userDir, "sub"), filepath.Join(userDir, "sub", "f")} {
		if uid, gid := owner(t, p); uid != rootTestUID || gid != rootTestUID {
			t.Fatalf("%s is %d:%d, want the user's", p, uid, gid)
		}
	}
	if err := write(filepath.Join(groupDir, "f"), ""); err != nil {
		t.Fatalf("a directory the user's supplementary group may write: %v", err)
	}
	if err := write(filepath.Join(base, "f"), ""); !podwire.IsCode(err, podwire.CodeDenied) {
		t.Fatalf("root's directory: %v", err)
	}
	if err := write(filepath.Join(userDir, "link", "f"), ""); !podwire.IsCode(err, podwire.CodeDenied) {
		t.Fatalf("through the planted link: %v", err)
	}
	if _, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: secret}); !podwire.IsCode(err, podwire.CodeDenied) {
		t.Fatalf("reading root's file as the user: %v", err)
	}
	// The root class still writes as root: the user's identity stayed on its
	// own thread.
	if err := write(filepath.Join(rootDir, "f"), execas.ClassRoot); err != nil {
		t.Fatal(err)
	}
	if uid, _ := owner(t, filepath.Join(rootDir, "f")); uid != 0 {
		t.Fatalf("root's file owned by %d", uid)
	}
	if got, err := s.c.ReadFile(ctx, podwire.FileReadHeader{Path: secret, Class: execas.ClassRoot}); err != nil || string(got) != "root only" {
		t.Fatalf("%q %v", got, err)
	}
}

// TestAsRootThreadIdentityNeverLeaks mixes user and root writes: every file
// is its own class's, and the agent itself stays root.
func TestAsRootThreadIdentityNeverLeaks(t *testing.T) {
	requireRoot(t)
	d := newTestDirs(t)
	o := d.options()
	o.Host = rootHost(t, filepath.Join(filepath.Dir(d.run), "fsroot"))
	s := startAgent(t, o)
	dir := filepath.Join(filepath.Dir(d.run), "mixed")
	mkdir(t, dir, 0o777, 0, 0)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			class := execas.ClassScript
			if i%2 == 0 {
				class = execas.ClassRoot
			}
			errs <- s.c.WriteFile(context.Background(), podwire.FileWriteHeader{Path: filepath.Join(dir, fmt.Sprint(i)), Size: -1, Class: class}, strings.NewReader("x"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range 200 {
		uid, _ := owner(t, filepath.Join(dir, fmt.Sprint(i)))
		want := uint32(rootTestUID)
		if i%2 == 0 {
			want = 0
		}
		if uid != want {
			t.Fatalf("file %d owned by %d, want %d", i, uid, want)
		}
	}
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("the agent lost root")
	}
}

func TestAsRootExecsRunAsTheUser(t *testing.T) {
	requireRoot(t)
	d := newTestDirs(t)
	o := d.options()
	o.Host = rootHost(t, filepath.Join(filepath.Dir(d.run), "fsroot"))
	s := startAgent(t, o)
	script := `echo "$(id -u) $(id -g) $(id -G)"`
	for name, h := range map[string]podwire.ExecHeader{
		"agent child":    sh(script),
		"under shepherd": resumable(sh(script)),
		"tty on shepherd": func() podwire.ExecHeader {
			h := resumable(sh(script + `; stat -c '%u %g %a' "$(tty)"`))
			h.TTY = true
			return h
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			e, err := s.c.Exec(context.Background(), h)
			if err != nil {
				t.Fatal(err)
			}
			out := collect(e.Stdout())
			if code, err := e.Wait(); code != 0 || err != nil {
				t.Fatalf("code %d %v", code, err)
			}
			<-out.done
			lines := strings.Fields(strings.ReplaceAll(out.buf.String(), "\r", ""))
			if len(lines) < 4 || lines[0] != "4242" || lines[1] != "4242" || !strings.Contains(out.buf.String(), "5555") {
				t.Fatalf("identity %q", out.buf.String())
			}
			if h.TTY && !strings.Contains(out.buf.String(), "4242 5 620") {
				t.Fatalf("pty %q, want 4242:tty 0620", out.buf.String())
			}
		})
	}
}
