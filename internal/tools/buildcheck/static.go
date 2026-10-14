package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
)

// runStatic checks that every file is a statically linked ELF binary. A
// .tar.gz bundle (the static tmux) is checked by its bin/ entries.
func runStatic(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("no files to check")
	}
	var failed []string
	for _, file := range args {
		if err := checkStaticFile(file); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		fmt.Fprintf(stdout, "static: %s\n", file)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "\n"))
	}
	return nil
}

func checkStaticFile(file string) error {
	if strings.HasSuffix(file, ".tar.gz") || strings.HasSuffix(file, ".tgz") {
		return checkStaticBundle(file)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	machine, err := checkStaticELF(file, f)
	if err != nil {
		return err
	}
	return lddAgrees(file, machine)
}

// checkStaticELF fails a binary that has a program interpreter or needs a
// shared library, and a Go binary built with cgo. It returns the binary's
// machine for the ldd cross-check.
func checkStaticELF(name string, r io.ReaderAt) (elf.Machine, error) {
	f, err := elf.NewFile(r)
	if err != nil {
		return 0, fmt.Errorf("%s: not an ELF binary: %w", name, err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return 0, fmt.Errorf("%s: dynamically linked (has a program interpreter)", name)
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return 0, fmt.Errorf("%s: read dynamic section: %w", name, err)
	}
	if len(libs) > 0 {
		return 0, fmt.Errorf("%s: needs shared libraries: %s", name, strings.Join(libs, ", "))
	}
	if info, err := buildinfo.Read(r); err == nil {
		if cgo := setting(info.Settings, "CGO_ENABLED"); cgo != "0" {
			return 0, fmt.Errorf("%s: Go binary built with CGO_ENABLED=%q, want 0", name, cgo)
		}
	}
	return f.Machine, nil
}

// checkStaticBundle checks every regular file under bin/ in a gzipped tar.
func checkStaticBundle(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	tr := tar.NewReader(gz)
	checked := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Dir(path.Clean(hdr.Name)) != "bin" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", file, hdr.Name, err)
		}
		if _, err := checkStaticELF(file+":"+hdr.Name, bytes.NewReader(data)); err != nil {
			return err
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("%s: no bin/ entries", file)
	}
	return nil
}

// goarchMachine maps GOARCH to the ELF machine of a binary this host can run.
var goarchMachine = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
	"386":   elf.EM_386,
	"arm":   elf.EM_ARM,
}

// lddAgrees runs ldd on a binary built for this host, when there is one, and
// fails if it lists shared libraries. ldd cannot judge a foreign-arch binary;
// the ELF check alone covers those.
func lddAgrees(file string, machine elf.Machine) error {
	if runtime.GOOS != "linux" || goarchMachine[runtime.GOARCH] != machine {
		return nil
	}
	ldd, err := exec.LookPath("ldd")
	if err != nil {
		return nil
	}
	// ldd exits non-zero for a static binary; the output decides.
	out, _ := exec.Command(ldd, file).CombinedOutput()
	if !lddSaysStatic(string(out)) {
		return fmt.Errorf("%s: ldd lists shared libraries:\n%s", file, strings.TrimSpace(string(out)))
	}
	return nil
}

// lddSaysStatic recognises the glibc and musl ldd verdicts for a static binary.
func lddSaysStatic(out string) bool {
	out = strings.ToLower(out)
	for _, verdict := range []string{"not a dynamic executable", "statically linked", "not a valid dynamic program"} {
		if strings.Contains(out, verdict) {
			return true
		}
	}
	return false
}

// runFlavor checks that every file is the build -want names.
func runFlavor(args []string, stdout io.Writer) error {
	var want buildflavor.Flavor
	if len(args) >= 2 && args[0] == "-want" {
		want, args = buildflavor.Flavor(args[1]), args[2:]
	}
	if want != buildflavor.Full && want != buildflavor.Pod {
		return fmt.Errorf("usage: flavor -want %s|%s FILE...", buildflavor.Full, buildflavor.Pod)
	}
	if len(args) == 0 {
		return errors.New("no files to check")
	}
	var failed []string
	for _, file := range args {
		got, err := buildflavor.OfFile(file)
		switch {
		case err != nil:
			failed = append(failed, err.Error())
		case got != want:
			failed = append(failed, fmt.Sprintf("%s: %s build, want %s", file, got, want))
		default:
			fmt.Fprintf(stdout, "%s: %s\n", want, file)
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "\n"))
	}
	return nil
}

func setting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
