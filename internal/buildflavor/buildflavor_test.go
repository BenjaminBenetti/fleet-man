package buildflavor

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestOf(t *testing.T) {
	settings := func(kv ...string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		for i := 0; i < len(kv); i += 2 {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return info
	}
	cases := []struct {
		name string
		info *debug.BuildInfo
		want Flavor
	}{
		{"nil info", nil, Full},
		{"no settings", settings(), Full},
		{"no tags", settings("CGO_ENABLED", "0", "GOOS", "linux"), Full},
		{"pod tag", settings("-tags", "nok8s"), Pod},
		{"pod tag among others", settings("-tags", "netgo,nok8s,osusergo"), Pod},
		{"space-separated tags", settings("-tags", "netgo nok8s"), Pod},
		{"other tags only", settings("-tags", "netgo,osusergo"), Full},
		{"tag prefix is not the tag", settings("-tags", "nok8sx,xnok8s"), Full},
		{"nok8s outside -tags", settings("-ldflags", "-X main.tag=nok8s"), Full},
	}
	for _, c := range cases {
		if got := Of(c.info); got != c.want {
			t.Errorf("%s: Of = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestOfFileMatchesCurrent reads this test binary's own build info, so it
// checks the tag wiring end to end: `go test` gives Full, `go test -tags nok8s`
// gives Pod.
func TestOfFileMatchesCurrent(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := OfFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if got != Current {
		t.Errorf("OfFile(test binary) = %q, want Current = %q", got, Current)
	}
}

func TestOfFileRejectsNonGoFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-go")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OfFile(path); err == nil {
		t.Error("OfFile(shell script) succeeded, want an error")
	}
}
