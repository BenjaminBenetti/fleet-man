package jobboot

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandWaitsForTheSentinelThenExecsPodJob(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Dir: dir}
	// A stand-in binary that reports how it was run.
	if err := os.WriteFile(p.Binary(), []byte("#!/bin/sh\necho \"ran: $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := Command(p, 20)
	if argv[0] != "sh" || argv[1] != "-c" || len(argv) != 3 {
		t.Fatalf("command = %q, want sh -c <script>", argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if cmd.ProcessState != nil {
		t.Fatal("the loop exited before the sentinel existed")
	}
	if err := os.WriteFile(p.Ready(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait loop: %v (%s)", err, out.String())
	}
	want := "ran: pod-job --spec " + p.Spec()
	if strings.TrimSpace(out.String()) != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

func TestCommandGivesUpWithExitNotStaged(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	argv := Command(p, 1)
	start := time.Now()
	err := exec.Command(argv[0], argv[1:]...).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != ExitNotStaged {
		t.Fatalf("err %v, want exit %d", err, ExitNotStaged)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("gave up after %s, want about 2s", d)
	}
}

func TestCommandDefaultsAndQuoting(t *testing.T) {
	argv := Command(Paths{}, 0)
	for _, want := range []string{"[ $i -gt 600 ] && exit 70", "'/fleet-tools/.ready'", "exec '/fleet-tools/fleet' pod-job --spec '/fleet-tools/job.json'"} {
		if !strings.Contains(argv[2], want) {
			t.Errorf("default command %q lacks %q", argv[2], want)
		}
	}
	odd := Command(Paths{Dir: "/tools it's"}, 5)
	if !strings.Contains(odd[2], `'/tools it'\''s/.ready'`) {
		t.Fatalf("a quote in the tools dir is not escaped: %q", odd[2])
	}
}

func sha(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// runStage runs StageScript(f) with stdin as `sh -c` would in the pod.
func runStage(t *testing.T, f StageFile, stdin []byte) (string, error) {
	t.Helper()
	script, err := StageScript(f)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestStageScriptWritesTheFileAndTheSentinel(t *testing.T) {
	dir := t.TempDir()
	data := []byte("#!/bin/sh\necho staged\n\x00\xff binary-ish")
	target := filepath.Join(dir, "fleet")
	f := StageFile{Path: target, Size: int64(len(data)), SHA256: sha(data), Mode: 0o755, Ready: true}
	if _, err := exec.LookPath("sha256sum"); err == nil {
		f.CheckSHA = true
	}
	if out, err := runStage(t, f, data); err != nil {
		t.Fatalf("stage: %v\n%s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("staged %q, %v", got, err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, ReadyName)); err != nil {
		t.Fatalf("no sentinel: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".stage.*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestStageScriptRefusesAShortOrWrongStream(t *testing.T) {
	data := []byte("the real binary")
	for name, tc := range map[string]struct {
		stdin    []byte
		checkSHA bool
	}{
		"short":     {stdin: data[:5]},
		"wrong sha": {stdin: []byte("the fake binary"), checkSHA: true},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.checkSHA {
				if _, err := exec.LookPath("sha256sum"); err != nil {
					t.Skip("no sha256sum here")
				}
			}
			dir := t.TempDir()
			target := filepath.Join(dir, "fleet")
			if err := os.WriteFile(target, []byte("previous"), 0o755); err != nil {
				t.Fatal(err)
			}
			f := StageFile{Path: target, Size: int64(len(data)), SHA256: sha(data), CheckSHA: tc.checkSHA, Ready: true}
			out, err := runStage(t, f, tc.stdin)
			if err == nil {
				t.Fatalf("stage of a bad stream succeeded: %s", out)
			}
			if got, _ := os.ReadFile(target); string(got) != "previous" {
				t.Fatalf("a failed stage replaced the target: %q", got)
			}
			if _, err := os.Stat(filepath.Join(dir, ReadyName)); err == nil {
				t.Fatal("a failed stage wrote the sentinel")
			}
			if left, _ := filepath.Glob(filepath.Join(dir, ".stage.*")); len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
		})
	}
}

func TestStageScriptGunzips(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("no gzip here")
	}
	data := bytes.Repeat([]byte("compressible fleet binary "), 4096)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(data)
	_ = zw.Close()
	target := filepath.Join(t.TempDir(), "job.json")
	f := StageFile{Path: target, Size: int64(len(data)), SHA256: sha(data), Gzip: true, WireSize: int64(gz.Len())}
	if out, err := runStage(t, f, gz.Bytes()); err != nil {
		t.Fatalf("stage: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, data) {
		t.Fatal("gunzipped content differs")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Fatalf("default mode %v, want 0644", fi.Mode())
	}
}

func TestStageFileValidate(t *testing.T) {
	good := StageFile{Path: "/fleet-tools/fleet", Size: 3, SHA256: strings.Repeat("a", 64)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*StageFile){
		"relative":    func(f *StageFile) { f.Path = "fleet" },
		"unclean":     func(f *StageFile) { f.Path = "/fleet-tools/../etc/x" },
		"root":        func(f *StageFile) { f.Path = "/" },
		"negative":    func(f *StageFile) { f.Size = -1 },
		"gzip no len": func(f *StageFile) { f.Gzip = true },
		"bad sha":     func(f *StageFile) { f.SHA256 = "ABC" },
		"setuid":      func(f *StageFile) { f.Mode = 0o4755 },
	} {
		f := good
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, f)
		}
		if _, err := StageScript(f); err == nil {
			t.Errorf("%s: StageScript accepted %+v", name, f)
		}
	}
}

func TestProbeScriptRunsAndParses(t *testing.T) {
	out, err := exec.Command("sh", "-c", ProbeScript).Output()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProbe(string(out))
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if p.UID != os.Getuid() || !p.Head || p.Arch == "" {
		t.Fatalf("probe %+v from %q", p, out)
	}
}

func TestParseProbe(t *testing.T) {
	p, err := ParseProbe("arch=aarch64\nuid=1000650000\ngzip=0\nsha256sum=1\nhead=1\n")
	if err != nil {
		t.Fatal(err)
	}
	want := Probe{Arch: "arm64", UID: 1000650000, SHA256Sum: true, Head: true}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
	if p, _ := ParseProbe("arch=x86_64\nuid=\nhead=1\n"); p.Arch != "amd64" || p.UID != -1 {
		t.Fatalf("got %+v", p)
	}
	for _, bad := range []string{"", "uid=0\nhead=1\n", "arch=\nuid=0\nhead=1\n", "arch=x86_64\nhead=1\n"} {
		if _, err := ParseProbe(bad); err == nil {
			t.Errorf("ParseProbe(%q) accepted it", bad)
		}
	}
}

func TestSeedDeadline(t *testing.T) {
	for bytes, want := range map[int64]time.Duration{
		0: 15 * time.Minute, 1: 16 * time.Minute, 1 << 30: 16 * time.Minute,
		1<<30 + 1: 17 * time.Minute, 10 << 30: 25 * time.Minute, -5: 15 * time.Minute,
	} {
		if got := SeedDeadline(bytes); got != want {
			t.Errorf("SeedDeadline(%d) = %s, want %s", bytes, got, want)
		}
	}
}
