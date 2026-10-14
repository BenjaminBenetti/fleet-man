package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildTiny compiles a dependency-free Go program for linux (the host arch on
// a linux host, else amd64) and returns its path.
func buildTiny(t *testing.T, cgo string, tags ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go build")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tiny\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goarch := "amd64"
	if runtime.GOOS == "linux" {
		goarch = runtime.GOARCH
	}
	out := filepath.Join(dir, "tiny")
	args := []string{"build", "-o", out}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED="+cgo, "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// dynamicELF is a dynamically linked binary of this host, or a skip.
func dynamicELF(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sh", "/bin/ls", "/usr/bin/env"} {
		f, err := elf.Open(p)
		if err != nil {
			continue
		}
		dynamic := false
		for _, prog := range f.Progs {
			dynamic = dynamic || prog.Type == elf.PT_INTERP
		}
		f.Close()
		if dynamic {
			return p
		}
	}
	t.Skip("no dynamically linked ELF binary on this host")
	return ""
}

func TestCheckStaticGoBinary(t *testing.T) {
	if err := checkStaticFile(buildTiny(t, "0")); err != nil {
		t.Errorf("CGO_ENABLED=0 build: %v", err)
	}
}

func TestCheckStaticRejectsCgoBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a cgo build needs a linux host")
	}
	// No cgo package is imported, so the binary links statically anyway; the
	// build info still says CGO_ENABLED=1, which the release build must not.
	err := checkStaticFile(buildTiny(t, "1"))
	if err == nil || !strings.Contains(err.Error(), `CGO_ENABLED="1"`) {
		t.Errorf("err = %v, want a CGO_ENABLED complaint", err)
	}
}

func TestCheckStaticRejectsDynamicELF(t *testing.T) {
	err := checkStaticFile(dynamicELF(t))
	if err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("err = %v, want dynamically linked", err)
	}
}

func TestCheckStaticRejectsNonELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkStaticFile(path); err == nil || !strings.Contains(err.Error(), "not an ELF binary") {
		t.Errorf("err = %v, want not an ELF binary", err)
	}
}

// writeBundle writes a gzipped tar of name -> source file ("" for a directory).
func writeBundle(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, src := range entries {
		if src == "" {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckStaticBundle(t *testing.T) {
	static := buildTiny(t, "0")
	notes := filepath.Join(t.TempDir(), "notes")
	if err := os.WriteFile(notes, []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok := writeBundle(t, map[string]string{"bin/": "", "bin/tmux": static, "share/terminfo/x/xterm": notes})
	if err := checkStaticFile(ok); err != nil {
		t.Errorf("static bundle: %v", err)
	}

	empty := writeBundle(t, map[string]string{"share/terminfo/x/xterm": notes})
	if err := checkStaticFile(empty); err == nil || !strings.Contains(err.Error(), "no bin/ entries") {
		t.Errorf("bundle without bin/: err = %v", err)
	}

	if runtime.GOOS == "linux" {
		dynamic := writeBundle(t, map[string]string{"bin/tmux": dynamicELF(t)})
		if err := checkStaticFile(dynamic); err == nil || !strings.Contains(err.Error(), ":bin/tmux: dynamically linked") {
			t.Errorf("dynamic bundle: err = %v", err)
		}
	}
}

func TestLddSaysStatic(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"\tnot a dynamic executable\n", true},             // glibc, static
		{"\tstatically linked\n", true},                    // glibc, static-pie
		{"ldd: tiny: Not a valid dynamic program\n", true}, // musl
		{"\tlinux-vdso.so.1 (0x00007ffd)\n\tlibc.so.6 => /lib/libc.so.6\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := lddSaysStatic(c.out); got != c.want {
			t.Errorf("lddSaysStatic(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

func TestRunFlavor(t *testing.T) {
	full := buildTiny(t, "0")
	pod := buildTiny(t, "0", "nok8s")
	if err := runFlavor([]string{"-want", "full", full}, io.Discard); err != nil {
		t.Errorf("full build as full: %v", err)
	}
	if err := runFlavor([]string{"-want", "pod", pod}, io.Discard); err != nil {
		t.Errorf("pod build as pod: %v", err)
	}
	if err := runFlavor([]string{"-want", "pod", pod, full}, io.Discard); err == nil || !strings.Contains(err.Error(), "full build, want pod") {
		t.Errorf("full build as pod: err = %v", err)
	}
	if err := runFlavor([]string{"-want", "other", full}, io.Discard); err == nil {
		t.Error("unknown flavor accepted")
	}
	if err := runFlavor([]string{"-want", "pod"}, io.Discard); err == nil {
		t.Error("no files accepted")
	}
}

func TestRunStaticNeedsFiles(t *testing.T) {
	if err := runStatic(nil, io.Discard); err == nil {
		t.Error("no files accepted")
	}
}
