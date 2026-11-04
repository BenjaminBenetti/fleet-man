package fleetlaunch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

func TestConcurrentStagingIsAtomic(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	b := &localInstallBackend{dir: t.TempDir()}
	if err := os.Mkdir(b.dir+"/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	const payload = "#!/bin/sh\nprintf 'v9.9.9\n'\n"
	target := b.local(RemotePath)
	if err := os.WriteFile(target, []byte(payload), 0o755); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				readErr <- nil
				return
			default:
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != payload {
				readErr <- fmt.Errorf("non-atomic replacement: %q %v", data, err)
				return
			}
		}
	}()
	const count = 12
	ready := make(chan struct{}, count)
	install := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			errs <- copyBinary(b, "/ws", strings.NewReader(payload), func() error { ready <- struct{}{}; <-install; return nil })
		})
	}
	// Every candidate must coexist through verification before any rename.
	// A staging failure or timeout releases hooks before reporting the error.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for range count {
		select {
		case <-ready:
		case <-timer.C:
			close(install)
			wg.Wait()
			close(stop)
			<-readErr
			t.Fatal("timed out waiting for concurrent candidates")
		case err := <-errs:
			close(install)
			wg.Wait()
			close(stop)
			<-readErr
			t.Fatalf("staging before hook: %v", err)
		}
	}
	candidates, err := filepath.Glob(b.dir + "/bin/.fleet-launch-stage.*")
	close(install)
	wg.Wait()
	close(stop)
	if err != nil || len(candidates) != count {
		t.Errorf("unique siblings: %v %v", candidates, err)
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if err := <-readErr; err != nil {
		t.Error(err)
	}
	for _, pattern := range []string{b.dir + "/stage.*", b.dir + "/bin/.fleet-launch-stage.*"} {
		paths, err := filepath.Glob(pattern)
		if err != nil || len(paths) != 0 {
			t.Errorf("leftovers: %v %v", paths, err)
		}
	}
}

// Opt-in: this test writes /usr/bin/fleet and must run only in a disposable
// container with /tmp mounted noexec. Run the compiled test as root, as a user
// with passwordless sudo, and without sudo (FLEET_TEST_STAGE_DENIED=1).
func TestCopyBinaryNoexecTmp(t *testing.T) {
	if os.Getenv("FLEET_TEST_STAGE_CONTAINER") != "1" {
		t.Skip("requires a disposable container with noexec /tmp")
	}
	old := version.Version
	version.Version = "v9.9.9"
	t.Cleanup(func() { version.Version = old })
	const payload = "#!/bin/sh\nprintf 'v9.9.9\n'\n"
	probe, err := os.CreateTemp("/tmp", "noexec-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe.Name())
	if _, err := probe.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if err := probe.Chmod(0o755); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	err = exec.Command("sh", "-c", probe.Name()).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 126 {
		t.Fatalf("/tmp must reject execution with 126: %v", err)
	}
	denied := os.Getenv("FLEET_TEST_STAGE_DENIED") == "1"
	b := &localInstallBackend{direct: true}
	for _, failure := range []string{"execute", "version", "hook", ""} {
		t.Run(failure, func(t *testing.T) {
			before, err := os.ReadFile(RemotePath)
			if err != nil {
				t.Fatal(err)
			}
			oldFile, err := os.Open(RemotePath)
			if err != nil {
				t.Fatal(err)
			}
			defer oldFile.Close()
			candidate := payload
			if failure == "execute" {
				candidate = "not executable"
			}
			if failure == "version" {
				candidate = "#!/bin/sh\nprintf wrong"
			}
			stopped := false
			err = copyBinary(b, "/ws", strings.NewReader(candidate), func() error {
				current, err := os.ReadFile(RemotePath)
				if err != nil || string(current) != string(before) {
					return fmt.Errorf("installed before hook: %q %v", current, err)
				}
				stopped = true
				if failure == "hook" {
					return errors.New("stop failed")
				}
				return nil
			})
			success := failure == "" && !denied
			if (err == nil) != success || stopped != (!denied && (failure == "hook" || failure == "")) {
				t.Fatalf("denied=%v failure=%s stopped=%v err=%v", denied, failure, stopped, err)
			}
			after, err := os.ReadFile(RemotePath)
			if err != nil {
				t.Fatal(err)
			}
			want := string(before)
			if success {
				want = payload
			}
			if string(after) != want {
				t.Fatalf("installed bytes: %q, want %q", after, want)
			}
			held, err := io.ReadAll(oldFile)
			if err != nil || string(held) != string(before) {
				t.Fatalf("old inode changed: %q %v", held, err)
			}
			for _, pattern := range []string{"/tmp/.fleet-launch-stage.*", remoteDir + "/.fleet-launch-stage.*"} {
				paths, err := filepath.Glob(pattern)
				if err != nil || len(paths) != 0 {
					t.Fatalf("leftovers: %v %v", paths, err)
				}
			}
		})
	}
}
