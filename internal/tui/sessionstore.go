package tui

import (
	"strings"
	"time"
)

// SessionStore owns per-instance session state. Every method requires
// an InstanceRef so callers cannot accidentally read or write state
// belonging to a different instance with the same session or group
// name. Not goroutine-safe — all access happens on the bubbletea
// message loop.
type SessionStore struct {
	discoveries map[InstanceRef]*sessionDiscovery
	expanded    map[InstanceRef]bool
	lastActive  map[InstanceRef]lastSession
}

// NewSessionStore returns an initialised, empty store.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		discoveries: make(map[InstanceRef]*sessionDiscovery),
		expanded:    make(map[InstanceRef]bool),
		lastActive:  make(map[InstanceRef]lastSession),
	}
}

// --- Discovery ---

// Discovery returns the most recent discovery for ref, if any.
func (s *SessionStore) Discovery(ref InstanceRef) (*sessionDiscovery, bool) {
	if s == nil || !ref.Valid() {
		return nil, false
	}
	d, ok := s.discoveries[ref]
	return d, ok
}

// HasDiscovery reports whether any discovery (success or error) has
// been recorded for ref.
func (s *SessionStore) HasDiscovery(ref InstanceRef) bool {
	if s == nil || !ref.Valid() {
		return false
	}
	_, ok := s.discoveries[ref]
	return ok
}

// SetDiscovery records a successful discovery for ref.
func (s *SessionStore) SetDiscovery(ref InstanceRef, sessions []tmuxSession) {
	if s == nil || !ref.Valid() {
		return
	}
	s.discoveries[ref] = &sessionDiscovery{
		sessions:  sessions,
		fetchedAt: time.Now(),
	}
}

// SetDiscoveryError records that the most recent discovery failed.
func (s *SessionStore) SetDiscoveryError(ref InstanceRef, err error) {
	if s == nil || !ref.Valid() {
		return
	}
	s.discoveries[ref] = &sessionDiscovery{
		err:       err,
		fetchedAt: time.Now(),
	}
}

// ForgetDiscovery drops the discovery record for ref.
func (s *SessionStore) ForgetDiscovery(ref InstanceRef) {
	if s == nil {
		return
	}
	delete(s.discoveries, ref)
}

// Sessions returns the session list from the most recent successful
// discovery for ref, or nil if absent or errored.
func (s *SessionStore) Sessions(ref InstanceRef) []tmuxSession {
	d, ok := s.Discovery(ref)
	if !ok || d == nil || d.err != nil {
		return nil
	}
	return d.sessions
}

// Groups returns the session groups for ref, derived from its current
// discovery and instance-name prefix. Sessions explicitly tagged for a
// different instance (sanitized name + groupSep + ...) are filtered out
// before grouping so two instances sharing a tmux server (e.g. via a
// shared workspace mount) cannot leak each other's sessions into the
// UI. Untagged sessions — those without a groupSep — are kept since
// they may be ad-hoc sessions a user created outside fleet.
func (s *SessionStore) Groups(ref InstanceRef) []sessionGroup {
	sessions := s.Sessions(ref)
	if len(sessions) == 0 {
		return nil
	}
	sanitized := SanitizeSessionName(ref.Instance)
	prefix := sanitized + groupSep
	own := make([]tmuxSession, 0, len(sessions))
	for _, sess := range sessions {
		if strings.Contains(sess.Name, groupSep) && !strings.HasPrefix(sess.Name, prefix) {
			continue
		}
		own = append(own, sess)
	}
	return groupSessions(sanitized, own)
}

// --- Expansion ---

// IsExpanded reports whether ref's row is currently expanded.
func (s *SessionStore) IsExpanded(ref InstanceRef) bool {
	if s == nil || !ref.Valid() {
		return false
	}
	return s.expanded[ref]
}

// SetExpanded toggles ref's expansion state.
func (s *SessionStore) SetExpanded(ref InstanceRef, on bool) {
	if s == nil || !ref.Valid() {
		return
	}
	if on {
		s.expanded[ref] = true
		return
	}
	delete(s.expanded, ref)
}

// ExpandedRefs returns a snapshot of all currently-expanded refs.
func (s *SessionStore) ExpandedRefs() []InstanceRef {
	if s == nil {
		return nil
	}
	out := make([]InstanceRef, 0, len(s.expanded))
	for ref, on := range s.expanded {
		if on {
			out = append(out, ref)
		}
	}
	return out
}

// CollapseAndForgetSessions drops the expansion and discovery records
// for ref but preserves any last-active entry — that survives across
// stop/start cycles so the instance can re-attach on next run.
func (s *SessionStore) CollapseAndForgetSessions(ref InstanceRef) {
	if s == nil {
		return
	}
	delete(s.expanded, ref)
	delete(s.discoveries, ref)
}

// --- Last active ---

// LastActive returns the most recently used session for ref.
func (s *SessionStore) LastActive(ref InstanceRef) (lastSession, bool) {
	if s == nil || !ref.Valid() {
		return lastSession{}, false
	}
	last, ok := s.lastActive[ref]
	return last, ok
}

// SetLastActive records the most recently used session for ref.
func (s *SessionStore) SetLastActive(ref InstanceRef, last lastSession) {
	if s == nil || !ref.Valid() {
		return
	}
	s.lastActive[ref] = last
}

// ClearLastActive removes the last-active entry for ref.
func (s *SessionStore) ClearLastActive(ref InstanceRef) {
	if s == nil {
		return
	}
	delete(s.lastActive, ref)
}

// PruneStaleLastActive iterates every last-active entry and clears any
// whose recorded session no longer appears in the current discovery.
// Called after each discovery refresh so deletions don't leave stale
// pointers behind.
func (s *SessionStore) PruneStaleLastActive() {
	if s == nil {
		return
	}
	for ref, last := range s.lastActive {
		d, ok := s.discoveries[ref]
		if !ok || d == nil || d.err != nil {
			delete(s.lastActive, ref)
			continue
		}
		if !sessionStillExists(SanitizeSessionName(ref.Instance), last, d.sessions) {
			delete(s.lastActive, ref)
		}
	}
}
