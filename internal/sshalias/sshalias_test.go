package sshalias

import (
	"strings"
	"testing"
)

func TestAliasRoundTrips(t *testing.T) {
	cases := []struct {
		fleet, inst, remote, want string
	}{
		{"fleet-man", "alpha", "", "alpha.fleet-man.fleet"},
		{"repo", "my_box-2", "rabc123", "my_box-2.repo.rabc123.fleet"},
		// Upper case, dots and other bytes are hex, so a lowercasing editor
		// or ssh's case-insensitive Host match never changes the target.
		{"Fleet.Man", "Alpha", "", "x416c706861.x466c6565742e4d616e.fleet"},
		// A plain name that reads as hex is hex-encoded itself.
		{"cafe", "xab", "", "x786162.cafe.fleet"},
		{"repo", "x", "", "x.repo.fleet"},
		{"repo", "xa", "", "xa.repo.fleet"},
		{"répo", "été", "r000000", "xc3a974c3a9.x72c3a9706f.r000000.fleet"},
	}
	for _, c := range cases {
		got := Alias(c.fleet, c.inst, c.remote)
		want := strings.ReplaceAll(c.want, " ", "")
		if got != want {
			t.Errorf("Alias(%q, %q, %q) = %q, want %q", c.fleet, c.inst, c.remote, got, want)
		}
		parsed, err := Parse(got)
		if err != nil {
			t.Errorf("Parse(%q): %v", got, err)
			continue
		}
		if parsed != (Target{Fleet: c.fleet, Instance: c.inst, Remote: c.remote}) {
			t.Errorf("Parse(%q) = %+v", got, parsed)
		}
	}
}

func TestParseRefusesAnythingElse(t *testing.T) {
	bad := []string{
		"",
		"alpha.fleet",                  // one name part
		"a.b.c.fleet",                  // three name parts, no remote
		"alpha.repo.r12345.fleet",      // a five-digit remote
		"alpha.repo.rABCDEF.fleet",     // upper-case hex
		"alpha.repo.fleet.",            // trailing dot
		"alpha.repo.FLEET",             // suffix case
		"alpha.repo.fleet.example.com", // not ours
		"al pha.repo.fleet",            // space
		"alpha;id.repo.fleet",          // shell metacharacters
		"$(id).repo.fleet",             // command substitution
		"-oProxyCommand=x.repo.fleet",  // option-looking
		"alpha.repo.rabc123.rabc123.fleet",
		"x20.repo.fleet",    // decodes to a space
		"x0a.repo.fleet",    // decodes to a newline
		"xff.repo.fleet",    // not UTF-8
		"alpha.x2e2e.fleet", // the fleet ".."
		"alpha.x2f.fleet",   // a fleet with a separator
	}
	for _, alias := range bad {
		if got, err := Parse(alias); err == nil {
			t.Errorf("Parse(%q) = %+v, want a refusal", alias, got)
		}
	}
}

func TestRemoteHash(t *testing.T) {
	a := RemoteHash("https://gw.example.com/grpc/abc")
	if !ValidRemote(a) {
		t.Fatalf("RemoteHash gave %q, not a remote part", a)
	}
	if a != RemoteHash("https://gw.example.com/grpc/abc") {
		t.Fatal("RemoteHash is not stable")
	}
	if a == RemoteHash("ssh://u@host") || a == RemoteHash("server:host:1") {
		t.Fatal("different remotes share a hash")
	}
	if _, err := Parse(Alias("repo", "alpha", a)); err != nil {
		t.Fatalf("an alias with a remote hash does not parse: %v", err)
	}
}

func TestURI(t *testing.T) {
	if got, want := URI("alpha.repo.fleet", "/workspaces/repo"), "vscode-remote://ssh-remote+alpha.repo.fleet/workspaces/repo"; got != want {
		t.Errorf("URI = %q, want %q", got, want)
	}
	if got, want := URI("alpha.repo.fleet", "/workspaces/my repo"), "vscode-remote://ssh-remote+alpha.repo.fleet/workspaces/my%20repo"; got != want {
		t.Errorf("URI with a space = %q, want %q", got, want)
	}
}

func TestConfig(t *testing.T) {
	cfg := Config("/opt/my fleet/100%/fleet")
	for _, want := range []string{
		"Host *.fleet\n",
		"  ProxyCommand '/opt/my fleet/100%%/fleet' ssh-proxy %n\n",
		"  StrictHostKeyChecking no\n",
		"  UserKnownHostsFile /dev/null\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("fleet_config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(strings.ToLower(cfg), "token") || strings.Contains(cfg, "Bearer") {
		t.Errorf("fleet_config mentions a token:\n%s", cfg)
	}
}

func TestInclude(t *testing.T) {
	if HasInclude("") || HasInclude("Host foo\n  HostName bar\n") || HasInclude("# Include fleet_config\n") {
		t.Fatal("HasInclude found an Include that is not there")
	}
	for _, cfg := range []string{
		"Include fleet_config\n",
		"include ~/.ssh/fleet_config\n",
		"  Include=\"fleet_config\"\n",
		"Include other_config fleet_config\n",
	} {
		if !HasInclude(cfg) {
			t.Errorf("HasInclude(%q) = false", cfg)
		}
	}
	if got := WithInclude(""); got != "Include fleet_config\n" {
		t.Errorf("WithInclude of an empty config = %q", got)
	}
	got := WithInclude("Host foo\n  User bar\n")
	if !strings.HasPrefix(got, "Include fleet_config\n\nHost foo\n") {
		t.Errorf("WithInclude did not put the Include first: %q", got)
	}
	if WithInclude(got) != got {
		t.Error("WithInclude added a second Include")
	}
}

func TestRemoteKey(t *testing.T) {
	if RemoteKey(false, "https://gw/grpc/x") != "https://gw/grpc/x" || RemoteKey(true, "host:1") != "server:host:1" {
		t.Fatal("RemoteKey")
	}
}
