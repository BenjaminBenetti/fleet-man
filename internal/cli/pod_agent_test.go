package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/podagent"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

func TestPodAgentIsHidden(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{{"pod-agent"}, {"pod-agent", "serve"}, {"pod-agent", "exec"}, {"pod-agent", "self-version"}, {"pod-agent", "shepherd"}, {"pod-agent", "install-bundle"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Fatalf("%v not registered: %v", path, err)
		}
		if cmd.Args(cmd, []string{"extra"}) == nil {
			t.Errorf("%v takes its input on stdin, never in argv", path)
		}
	}
	cmd, _, _ := root.Find([]string{"pod-agent"})
	if !cmd.Hidden {
		t.Fatal("pod-agent is internal and must stay out of fleet --help")
	}
	serve, _, _ := root.Find([]string{"pod-agent", "serve"})
	if f := serve.Flags().Lookup("role"); f == nil || f.DefValue != string(podwire.RoleMain) {
		t.Fatalf("--role %v", f)
	}
	if serve.Flags().Lookup("proto") == nil {
		t.Fatal("serve has no --proto")
	}
	exec, _, _ := root.Find([]string{"pod-agent", "exec"})
	for _, flag := range []string{"stdio", "resume"} {
		if exec.Flags().Lookup(flag) == nil {
			t.Fatalf("exec has no --%s", flag)
		}
	}
}

func TestPodAgentExecNeedsStdio(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"pod-agent", "exec"})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--stdio") {
		t.Fatalf("got %v", err)
	}
}

func TestPodAgentSelfVersion(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"pod-agent", "self-version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var v podagent.SelfVersionInfo
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	if v.ProtoMin != podwire.ProtoMin || v.ProtoMax != podwire.ProtoMax || len(v.SHA) != 64 || v.Arch == "" {
		t.Fatalf("%+v", v)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("self-version is one line: %q", out.String())
	}
}

func TestPodAgentInstallBundle(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "bin/tmux", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	sha := hex.EncodeToString(sum[:])
	dir := t.TempDir()

	code := -1
	oldExit := exitPodAgent
	defer func() { exitPodAgent = oldExit }()
	exitPodAgent = func(c int) { code = c }
	check := func() int {
		code = 0
		root := NewRootCmd()
		root.SetArgs([]string{"pod-agent", "install-bundle", "--check", "--dir", dir, "--name", "tmux", "--sha256", sha})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return code
	}
	if c := check(); c != exitBundleMissing {
		t.Fatalf("--check before the install exited %d, want %d", c, exitBundleMissing)
	}
	root := NewRootCmd()
	root.SetIn(bytes.NewReader(buf.Bytes()))
	root.SetArgs([]string{"pod-agent", "install-bundle", "--dir", dir, "--name", "tmux", "--sha256", sha})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "tmux", "bin", "tmux")); err != nil || string(got) != "hi" {
		t.Fatalf("installed %q %v", got, err)
	}
	if c := check(); c != 0 {
		t.Fatalf("--check after the install exited %d", c)
	}
}
