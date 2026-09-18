package sshtunnel

import (
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	cases := []struct {
		raw     string
		want    Target
		wantErr string
	}{
		{raw: "ssh://ben@desktop", want: Target{User: "ben", Host: "desktop"}},
		{raw: "ssh://desktop", want: Target{Host: "desktop"}},
		{raw: "SSH://Ben@Desktop.Local:2222", want: Target{User: "Ben", Host: "desktop.local", Port: "2222"}},
		{raw: "  ssh://ben@10.0.0.5/  ", want: Target{User: "ben", Host: "10.0.0.5"}},
		{raw: "ssh://ben@[::1]:22", want: Target{User: "ben", Host: "::1", Port: "22"}},
		{raw: "https://gw.example.com/abc", wantErr: "not an ssh:// URL"},
		{raw: "ssh://", wantErr: "no host"},
		{raw: "ssh://ben:pw@host", wantErr: "password"},
		{raw: "ssh://-oProxyCommand=evil@host", wantErr: "'-'"},
		{raw: "ssh://host/path", wantErr: "no path"},
		{raw: "ssh://host?x=1", wantErr: "no path"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseURL(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseURL(%q) err = %v, want containing %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseURL(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTargetStringCanonical pins the dedupe key: two spellings of one remote
// (case, trailing slash, whitespace) share a tunnel; user/port differences don't.
func TestTargetStringCanonical(t *testing.T) {
	a, _ := ParseURL("ssh://ben@Desktop/")
	b, _ := ParseURL(" SSH://ben@desktop ")
	if a.String() != b.String() || a.String() != "ssh://ben@desktop" {
		t.Fatalf("canonical mismatch: %q vs %q", a.String(), b.String())
	}
	c, _ := ParseURL("ssh://ben@desktop:2222")
	if c.String() != "ssh://ben@desktop:2222" {
		t.Fatalf("port dropped from canonical form: %q", c.String())
	}
	v6, _ := ParseURL("ssh://[::1]:22")
	if v6.String() != "ssh://[::1]:22" {
		t.Fatalf("ipv6 canonical form: %q", v6.String())
	}
}

// TestSSHArgs pins the batch-mode flags (a daemon can never answer a prompt),
// the -p placement, and that the destination is LAST (after any extra flags).
func TestSSHArgs(t *testing.T) {
	tgt := Target{User: "ben", Host: "desktop", Port: "2222"}
	args := tgt.sshArgs("-N", "-L", "127.0.0.1:1:127.0.0.1:2")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-o BatchMode=yes", "-o ConnectTimeout=15", "-p 2222", "-N -L 127.0.0.1:1:127.0.0.1:2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if args[len(args)-1] != "ben@desktop" {
		t.Errorf("destination must be the last arg, got %q", args[len(args)-1])
	}
	if got := (Target{Host: "h"}).sshArgs(); got[len(got)-1] != "h" {
		t.Errorf("userless destination = %q, want h", got[len(got)-1])
	}
}
