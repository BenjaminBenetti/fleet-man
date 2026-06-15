package fleetclient

import "testing"

func TestParseCopyEndpoint(t *testing.T) {
	cases := []struct {
		arg  string
		want CopyEndpoint
	}{
		// Instance references.
		{"alpha:bin/tool", CopyEndpoint{Kind: CopyInstance, Instance: "alpha", Path: "bin/tool"}},
		{"myfleet/alpha:/abs/path", CopyEndpoint{Kind: CopyInstance, Fleet: "myfleet", Instance: "alpha", Path: "/abs/path"}},
		{"alpha:/path/with:colon", CopyEndpoint{Kind: CopyInstance, Instance: "alpha", Path: "/path/with:colon"}},
		// Self references (the current instance).
		{":path", CopyEndpoint{Kind: CopySelf, Path: "path"}},
		{":bin/tool", CopyEndpoint{Kind: CopySelf, Path: "bin/tool"}},
		// Plain local paths — a leading /, . or ~ always forces local.
		{"bin/tool", CopyEndpoint{Kind: CopyLocal, Path: "bin/tool"}},
		{"/abs/path", CopyEndpoint{Kind: CopyLocal, Path: "/abs/path"}},
		{"./weird:name", CopyEndpoint{Kind: CopyLocal, Path: "./weird:name"}},
		{"../up:name", CopyEndpoint{Kind: CopyLocal, Path: "../up:name"}},
		{"~/home:file", CopyEndpoint{Kind: CopyLocal, Path: "~/home:file"}},
		{"", CopyEndpoint{Kind: CopyLocal, Path: ""}},
		// Malformed instance references fall back to local (the caller's
		// existence check then yields a plain "no such file").
		{"alpha:", CopyEndpoint{Kind: CopyLocal, Path: "alpha:"}},
		{":", CopyEndpoint{Kind: CopyLocal, Path: ":"}},
		{"a/b/c:path", CopyEndpoint{Kind: CopyLocal, Path: "a/b/c:path"}},
		{"/alpha:path", CopyEndpoint{Kind: CopyLocal, Path: "/alpha:path"}},
		{"alpha/:path", CopyEndpoint{Kind: CopyLocal, Path: "alpha/:path"}},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got := ParseCopyEndpoint(tc.arg)
			if got != tc.want {
				t.Errorf("ParseCopyEndpoint(%q) = %+v, want %+v", tc.arg, got, tc.want)
			}
		})
	}
}
