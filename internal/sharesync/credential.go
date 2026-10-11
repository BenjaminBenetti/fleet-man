package sharesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// Credential is the credential policy, for agent login files (Claude Code's
// .credentials.json, Codex's auth.json, gh's hosts.yml, Auggie's
// session.json). A login is kept per location (Scope) and travels on the
// priority lane, so a refreshed token reaches the location's other copies
// before they need it.
//
// What it decides, beyond mirror:
//   - a delete made on the current login is a logout: it goes everywhere in
//     the location as a tombstone. A delete that had not seen the current
//     login (a stale copy) is refused and the copy gets the login back;
//   - a change to a login that was logged out since (a stale copy
//     refreshing a token the user signed out of) never brings it back; a
//     login made where none was held (base 0) does;
//   - Claude Code's answer to a refresh it lost (invalid_grant: it clears
//     its copy's tokens) never replaces a live login: the copy that lost is
//     given the winner's token instead;
//   - a login file that is empty, or a JSON one that does not parse, is a
//     write under way (Codex rewrites auth.json in place): it waits on its
//     side (ErrIncomplete), so only whole logins ever travel;
//   - two logins changed at once: the newer change wins, with no conflict
//     copy (a superseded token is no one's content, and is a secret).
//
// The hub keeps each location's current login (History 1, in its 0600
// blob store beside the per-location login store), so it can always give
// back what a refused write replaced, even when the copy that wrote it is
// the location's only one (the daemon host) or no other is attached; a
// logout drops it.
type Credential struct{}

// Scope is per location.
func (Credential) Scope() Scope { return ScopeLocation }

// Lane is priority.
func (Credential) Lane() Lane { return LanePriority }

// History keeps the current login, none before it.
func (Credential) History() int { return 1 }

// Merge decides one change to a login.
func (Credential) Merge(ctx context.Context, in *MergeInput) (MergeResult, error) {
	inc := in.Incoming.Entry
	if !inc.Live() || inc.Kind != File {
		res := newestWins(in)
		if !inc.Live() && res.Decision == Accept {
			res.Reason = fmt.Sprintf("a logout on %s", in.Side.ID)
		}
		res.Conflict = false
		return res, nil
	}
	data, err := in.Incoming.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		res := newestWins(in)
		res.Conflict = false
		return res, nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	if err := wholeLogin(in.Path, data); err != nil {
		return MergeResult{}, fmt.Errorf("%s from %s: %w (%v)", in.Path, in.Side.ID, ErrIncomplete, err)
	}
	cur := in.Current
	live := cur != nil && cur.Entry.Kind == File
	if live && clearedLogin(data) {
		return MergeResult{Decision: Keep, Reason: fmt.Sprintf("the login %s cleared after losing a token refresh never signs the location's other copies out", in.Side.ID)}, nil
	}
	switch {
	case in.Fast || cur == nil:
		return MergeResult{Decision: Accept}, nil
	case !cur.Entry.Live():
		if in.BaseRev > 0 {
			return MergeResult{Decision: Keep, Reason: fmt.Sprintf("a change on %s to a login logged out since", in.Side.ID)}, nil
		}
		return MergeResult{Decision: Accept, Reason: fmt.Sprintf("a login on %s after a logout", in.Side.ID)}, nil
	case !live:
		res := newestWins(in)
		res.Conflict = false
		return res, nil
	case newer(inc, cur.Entry):
		return MergeResult{Decision: Accept, Reason: fmt.Sprintf("a newer login from %s", in.Side.ID)}, nil
	}
	// The current login is the newer: kept, unless it is no login at all.
	curData, err := cur.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		return MergeResult{Decision: Keep, Reason: fmt.Sprintf("an older login from %s", in.Side.ID)}, nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	if clearedLogin(curData) || wholeLogin(in.Path, curData) != nil {
		return MergeResult{Decision: Accept, Reason: fmt.Sprintf("a login from %s replaces a cleared one", in.Side.ID)}, nil
	}
	return MergeResult{Decision: Keep, Reason: fmt.Sprintf("an older login from %s", in.Side.ID)}, nil
}

// wholeLogin checks a login file is a whole one: not empty, and a JSON
// object when it is a .json file.
func wholeLogin(name string, data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("an empty file")
	}
	if strings.HasSuffix(name, ".json") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
			return errors.New("not a JSON object")
		}
	}
	return nil
}

// clearedLogin reports whether data is Claude Code's login after it lost a
// refresh (invalid_grant): its claudeAiOauth tokens emptied in place, which
// is no login.
func clearedLogin(data []byte) bool {
	var doc struct {
		OAuth *struct {
			AccessToken  *string `json:"accessToken"`
			RefreshToken *string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.OAuth == nil {
		return false
	}
	o := doc.OAuth
	return o.AccessToken != nil && *o.AccessToken == "" && o.RefreshToken != nil && *o.RefreshToken == ""
}

// LoginPaths returns a SideOptions.Urgent that marks the table's login
// files (its credential paths): a side scans and reports a refreshed token
// at once, without the debounce. NewSide uses it when Urgent is nil.
func LoginPaths(table syncpolicy.Table) func(root, name string) bool {
	if len(table.Roots()) == 0 {
		table = syncpolicy.Defaults()
	}
	return func(root, name string) bool {
		policy, ok := table.PolicyFor(path.Join(root, name))
		return ok && policy == syncpolicy.Credential
	}
}
