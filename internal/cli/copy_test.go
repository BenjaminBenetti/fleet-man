package cli

import (
	"testing"
)

func TestSplitCopySource(t *testing.T) {
	cases := []struct {
		arg        string
		wantTarget string
		wantPath   string
		wantOK     bool
	}{
		{"alpha:bin/tool", "alpha", "bin/tool", true},
		{"myfleet/alpha:/abs/path", "myfleet/alpha", "/abs/path", true},
		{"alpha:/path/with:colon", "alpha", "/path/with:colon", true},
		// Plain paths — the in-instance form.
		{"bin/tool", "", "", false},
		{"/abs/path", "", "", false},
		{"./weird:name", "", "", false},
		{"../up:name", "", "", false},
		{"~/home:file", "", "", false},
		// Malformed instance references.
		{":path", "", "", false},
		{"alpha:", "", "", false},
		{"a/b/c:path", "", "", false},
		{"/alpha:path", "", "", false},
		{"alpha/:path", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			target, path, ok := splitCopySource(tc.arg)
			if target != tc.wantTarget || path != tc.wantPath || ok != tc.wantOK {
				t.Errorf("splitCopySource(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.arg, target, path, ok, tc.wantTarget, tc.wantPath, tc.wantOK)
			}
		})
	}
}
