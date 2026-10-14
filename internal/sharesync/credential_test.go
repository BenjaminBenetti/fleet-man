package sharesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// claudeLogin is a Claude Code .credentials.json holding token tok.
func claudeLogin(tok string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at-%s","refreshToken":"rt-%s","expiresAt":1760000000000,"scopes":["user:inference"]}}`, tok, tok)
}

// clearedClaudeLogin is what Claude Code writes after a refresh fails with
// invalid_grant: the same file with its tokens emptied.
const clearedClaudeLogin = `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":["user:inference"]}}`

// loginCase is a change to a login meeting the hub's state.
type loginCase struct {
	name string
	// cur is the current login, "-" for a logout, "" for nothing.
	cur string
	// inc is the change, "-" for a delete.
	inc      string
	fast     bool
	baseRev  uint64
	curNewer bool
	path     string
}

func (c loginCase) input() *MergeInput {
	curT, incT := int64(100), int64(200)
	if c.curNewer {
		curT = 300
	}
	name := c.path
	if name == "" {
		name = ".credentials.json"
	}
	in := &MergeInput{Root: ".claude", Path: name, Side: SideInfo{ID: "a2", Location: "k1"}, Fast: c.fast, BaseRev: c.baseRev}
	switch c.cur {
	case "":
	case "-":
		in.Current = &Version{Entry: Entry{Kind: Gone}, Rev: 7}
	default:
		v := version(c.cur, curT, "a1")
		v.Rev = 7
		in.Current = &v
	}
	if c.inc == "-" {
		in.Incoming = Version{Entry: Entry{Kind: Gone}, Side: "a2"}
	} else {
		in.Incoming = version(c.inc, incT, "a2")
	}
	return in
}

// TestCredentialDecisions pins the credential policy's table: logouts made
// on the current login go (tombstones) and stale ones are refused; a stale
// copy's refresh never undoes a logout, a login made after one does; a
// cleared login never replaces a live one; two concurrent logins are
// decided by recency; and no conflict copy is ever kept of a login.
func TestCredentialDecisions(t *testing.T) {
	l1, l2 := claudeLogin("1"), claudeLogin("2")
	cases := []struct {
		c        loginCase
		decision Decision
	}{
		{loginCase{name: "a first login", inc: l1}, Accept},
		{loginCase{name: "a refresh on the current login", cur: l1, inc: l2, fast: true, baseRev: 7}, Accept},
		{loginCase{name: "a logout on the current login", cur: l1, inc: "-", fast: true, baseRev: 7}, Accept},
		{loginCase{name: "a stale logout", cur: l2, inc: "-", baseRev: 3}, Keep},
		{loginCase{name: "a stale copy's refresh after a logout", cur: "-", inc: l2, baseRev: 3}, Keep},
		{loginCase{name: "a login made after a logout", cur: "-", inc: l2}, Accept},
		{loginCase{name: "a cleared login over a live one (fast)", cur: l1, inc: clearedClaudeLogin, fast: true, baseRev: 7}, Keep},
		{loginCase{name: "a cleared login over a live one", cur: l2, inc: clearedClaudeLogin, baseRev: 3}, Keep},
		{loginCase{name: "a cleared login where none is", inc: clearedClaudeLogin}, Accept},
		{loginCase{name: "a live login over a cleared one, though older", cur: clearedClaudeLogin, inc: l2, baseRev: 3, curNewer: true}, Accept},
		{loginCase{name: "two logins at once: the newer incoming", cur: l1, inc: l2, baseRev: 3}, Accept},
		{loginCase{name: "two logins at once: the newer current", cur: l1, inc: l2, baseRev: 3, curNewer: true}, Keep},
		{loginCase{name: "gh hosts.yml refreshed", path: "hosts.yml", cur: "github.com:\n  oauth_token: a\n", inc: "github.com:\n  oauth_token: b\n", fast: true, baseRev: 7}, Accept},
		{loginCase{name: "a login that is not Claude's", path: "auth.json", cur: `{"tokens":{"refresh_token":"a"}}`, inc: `{"tokens":{"refresh_token":"b"}}`, baseRev: 3}, Accept},
	}
	ctx := context.Background()
	for _, tc := range cases {
		res, err := Credential{}.Merge(ctx, tc.c.input())
		if err != nil || res.Decision != tc.decision || res.Conflict {
			t.Errorf("%s: %+v, %v; want decision %d and no conflict copy", tc.c.name, res, err, tc.decision)
		}
	}
}

// TestCredentialLeavesAWriteUnderWayWaiting checks a login file empty, or a
// JSON one that does not parse (Codex writes auth.json in place), is never
// taken: it waits on its side (ErrIncomplete).
func TestCredentialLeavesAWriteUnderWayWaiting(t *testing.T) {
	for _, c := range []loginCase{
		{name: "an empty .credentials.json", cur: claudeLogin("1"), inc: "", fast: true, baseRev: 7},
		{name: "auth.json cut short", path: "auth.json", cur: `{"tokens":{}}`, inc: `{"tokens":{"refre`, fast: true, baseRev: 7},
		{name: "auth.json cut short, first copy", path: "auth.json", inc: `{"tok`},
		{name: "a JSON login that is not an object", path: "session.json", cur: `{}`, inc: `null`, baseRev: 3},
		{name: "an empty hosts.yml", path: "hosts.yml", cur: "github.com:\n", inc: "\n", fast: true, baseRev: 7},
	} {
		if _, err := (Credential{}).Merge(context.Background(), c.input()); !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v, want ErrIncomplete", c.name, err)
		}
	}
}

// TestCredentialWaitsForTheNewerLogin checks a concurrent older login whose
// newer rival cannot be read now waits for it (the newer may be a cleared
// login, which it would replace).
func TestCredentialWaitsForTheNewerLogin(t *testing.T) {
	in := loginCase{cur: claudeLogin("1"), inc: claudeLogin("2"), baseRev: 3, curNewer: true}.input()
	in.Current.open = func(context.Context) (io.ReadCloser, error) { return nil, ErrNoSource }
	if _, err := (Credential{}).Merge(context.Background(), in); !errors.Is(err, ErrNoSource) {
		t.Errorf("%v, want ErrNoSource", err)
	}
}

// TestClearedLogin pins the shape of Claude Code's cleared login.
func TestClearedLogin(t *testing.T) {
	for doc, want := range map[string]bool{
		clearedClaudeLogin: true,
		`{"claudeAiOauth":{"accessToken":"","refreshToken":""},"mcpOAuth":{}}`: true,
		claudeLogin("1"): false,
		`{"claudeAiOauth":{"accessToken":"x","refreshToken":""}}`: false,
		`{"claudeAiOauth":{"accessToken":"","refreshToken":"x"}}`: false,
		`{"claudeAiOauth":{}}`:           false,
		`{"tokens":{"access_token":""}}`: false,
		`{"claudeAiOauth":null}`:         false,
		`not json`:                       false,
	} {
		if got := clearedLogin([]byte(doc)); got != want {
			t.Errorf("clearedLogin(%s) = %v", doc, got)
		}
	}
}

// TestLoginPathsAreUrgent checks the login files of the defaults are what a
// side scans at once, and nothing else.
func TestLoginPathsAreUrgent(t *testing.T) {
	urgent := LoginPaths(syncpolicy.Table{})
	for _, p := range [][2]string{{".claude", ".credentials.json"}, {".codex", "auth.json"}, {".config/gh", "hosts.yml"}, {".augment", "session.json"}} {
		if !urgent(p[0], p[1]) {
			t.Errorf("%s/%s is not urgent", p[0], p[1])
		}
	}
	for _, p := range [][2]string{{".claude", "settings.json"}, {"files", ".claude.json"}, {".mnt", "x/auth.json"}, {".claude", "projects/x/.credentials.json"}} {
		if urgent(p[0], p[1]) {
			t.Errorf("%s/%s is urgent", p[0], p[1])
		}
	}
}

// TestSidesScanLoginsAtOnce checks a side without its own Urgent scans its
// tree's login files without the debounce.
func TestSidesScanLoginsAtOnce(t *testing.T) {
	s := NewSide(SideInfo{ID: "a1", Location: "k1"}, &Tree{table: syncpolicy.Defaults()}, nil, SideOptions{})
	if s.opts.Urgent == nil || !s.opts.Urgent(".codex", "auth.json") || s.opts.Urgent(".codex", "config.toml") {
		t.Error("a side does not scan its logins at once by default")
	}
	own := NewSide(SideInfo{ID: "a1"}, &Tree{table: syncpolicy.Defaults()}, nil, SideOptions{Urgent: func(string, string) bool { return false }})
	if own.opts.Urgent(".codex", "auth.json") {
		t.Error("a side's own Urgent was replaced")
	}
}
