package flog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A test binary that has not given its tests a HOME of their own logs
// nothing (the invoking user's ~/.fleet/fleet.log stays as it was); once a
// test has one, the log opens there, and a test handing HOME back is
// discarded again.
func TestATestBinaryNeverWritesTheInvokingUsersLog(t *testing.T) {
	if !testBinary() {
		t.Fatal("a go test binary is not recognised as one")
	}
	invoker, isolated := t.TempDir(), t.TempDir()
	origHome, origLogger := startHome, logger
	t.Cleanup(func() {
		startHome, logger, initOnce = origHome, origLogger, sync.Once{}
	})
	startHome, logger, initOnce = invoker, nil, sync.Once{}

	t.Setenv("HOME", invoker)
	Info("before isolation", "marker", "invoker")
	if _, err := os.Stat(filepath.Join(invoker, ".fleet")); !os.IsNotExist(err) {
		t.Fatalf("a test without a HOME of its own wrote the invoking user's ~/.fleet (stat: %v)", err)
	}

	t.Setenv("HOME", isolated)
	Info("after isolation", "marker", "isolated")
	data, err := os.ReadFile(filepath.Join(isolated, ".fleet", logFileName))
	if err != nil {
		t.Fatalf("an isolated test's record was not written: %v", err)
	}
	if !strings.Contains(string(data), "marker=isolated") || strings.Contains(string(data), "marker=invoker") {
		t.Fatalf("fleet.log under the isolated HOME:\n%s", data)
	}

	t.Setenv("HOME", invoker)
	Warn("handed back", "marker", "returned")
	if _, err := os.Stat(filepath.Join(invoker, ".fleet")); !os.IsNotExist(err) {
		t.Fatalf("a record logged after HOME was handed back reached the invoking user's ~/.fleet (stat: %v)", err)
	}
	if data, _ := os.ReadFile(filepath.Join(isolated, ".fleet", logFileName)); strings.Contains(string(data), "marker=returned") {
		t.Fatalf("a record logged under the invoking user's HOME went to a test's log:\n%s", data)
	}
}
