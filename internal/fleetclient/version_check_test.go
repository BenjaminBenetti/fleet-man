package fleetclient

import "testing"

// TestDecideReconcile encodes the full version/freshness policy table. Each row
// is (client version, server version, isLocal, spawned, stale) -> action.
func TestDecideReconcile(t *testing.T) {
	const (
		dev  = "" // a dev build reports no version
		beta = "v1.0.0-beta"
		v100 = "v1.0.0"
		v101 = "v1.0.1"
	)
	cases := []struct {
		name                   string
		cv, sv                 string
		isLocal, spawned, stale bool
		want                   reconcileAction
	}{
		// --- DEV client: versions ignored, decided by binary freshness ---
		{"dev: just spawned", dev, dev, true, true, true, actionNone},
		{"dev: remote", dev, dev, false, false, true, actionNone},
		{"dev: local, not stale", dev, dev, true, false, false, actionNone},
		{"dev: local, stale", dev, dev, true, false, true, actionRestart},
		// A dev client restarts even a VERSIONED server when its binary is newer
		// (one machine both tests dev builds and runs the release).
		{"dev: local, stale, versioned server", dev, v100, true, false, true, actionRestart},
		{"dev: local, not stale, versioned server", dev, v100, true, false, false, actionNone},

		// --- VERSIONED client ---
		// The fix: a release reclaims a leftover LOCAL dev (no-version) server.
		{"versioned: dev server, local -> restart", v100, dev, true, false, false, actionRestart},
		// A remote dev server can't be relaunched -> error.
		{"versioned: dev server, remote -> error", v100, dev, false, false, false, actionError},
		// Same numeric core (pre-release counts as its release) -> leave it.
		{"versioned: same core (stable vs beta) -> none", v100, beta, true, false, false, actionNone},
		{"versioned: identical -> none", v100, v100, true, false, false, actionNone},
		{"versioned: beta client vs beta server -> none", beta, beta, true, false, false, actionNone},
		// Older local server -> replace.
		{"versioned: older server, local -> restart", v101, v100, true, false, false, actionRestart},
		{"versioned: older server core via beta -> restart", v101, beta, true, false, false, actionRestart},
		// Newer server -> client must upgrade (error), never restart.
		{"versioned: newer server -> error", v100, v101, true, false, false, actionError},
		// Any version mismatch on a remote server -> error.
		{"versioned: older server but remote -> error", v101, v100, false, false, false, actionError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideReconcile(tc.cv, tc.sv, tc.isLocal, tc.spawned, tc.stale)
			if got != tc.want {
				t.Fatalf("decideReconcile(%q,%q,local=%v,spawned=%v,stale=%v) action = %v, want %v",
					tc.cv, tc.sv, tc.isLocal, tc.spawned, tc.stale, got, tc.want)
			}
			if (err != nil) != (tc.want == actionError) {
				t.Fatalf("error presence = %v (err=%v), want error==%v", err != nil, err, tc.want == actionError)
			}
		})
	}
}

func TestVersionCore(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":        "1.2.3",
		"1.2.3":         "1.2.3",
		"v1.2.3-beta":   "1.2.3",
		"v1.0.0-rc.1":   "1.0.0",
		"v1.0.0-beta-2": "1.0.0", // trims at the FIRST '-'
		"":              "",
	}
	for in, want := range cases {
		if got := versionCore(in); got != want {
			t.Errorf("versionCore(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.2.0", "v1.10.0", true}, // numeric, not lexical: 2 < 10
		{"v2.0.0", "v1.9.9", false},
		// Pre-release suffixes are ignored: same core is never "less".
		{"v1.0.0-beta", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-beta", false},
		// Core ordering still applies across a pre-release.
		{"v1.0.0-beta", "v1.0.1", true},
		{"v1.0.1", "v1.0.0-beta", false},
	}
	for _, tc := range cases {
		if got := versionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
