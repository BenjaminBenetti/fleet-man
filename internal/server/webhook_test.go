package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// formEncode wraps raw JSON the way GitHub's DEFAULT webhook content type
// (application/x-www-form-urlencoded) delivers it: a single `payload=` form field
// holding the url-encoded JSON. The result is NOT valid JSON.
func formEncode(rawJSON string) string {
	return "payload=" + url.QueryEscape(rawJSON)
}

// webhookState builds a state with the given webhook triggers under fleet
// "alpha", all referencing a single agent "a".
func webhookState(triggers ...fleet.Trigger) *state.State {
	return &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: triggers,
		}},
	}}
}

// postWebhook drives serveWebhook with a POST to /<name> carrying body and
// returns the recorder. The state seam is stubbed for the duration of the call.
func postWebhook(t *testing.T, s *service, st *state.State, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })

	req := httptest.NewRequest(http.MethodPost, "/"+name, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.serveWebhook(w, req)
	return w
}

// drainBatch returns the next queued fire batch (non-blocking), or nil if none.
func drainBatch(s *service) []webhookFire {
	select {
	case b := <-s.webhookFires:
		return b
	default:
		return nil
	}
}

func TestServeWebhook_RegexMatchFires(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: `"action":"opened"`,
	})

	w := postWebhook(t, s, st, "ci", `{"action":"opened"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	b := drainBatch(s)
	if len(b) != 1 {
		t.Fatalf("expected a 1-trigger batch, got %v", b)
	}
	if b[0].fleet != "alpha" || b[0].trigger.Name != "ci" {
		t.Fatalf("unexpected fire: %+v", b[0])
	}
}

func TestServeWebhook_NoFilterMatchNoFire(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: `"action":"opened"`,
	})

	// Name is found, but the body doesn't match the filter → 200, no fire.
	w := postWebhook(t, s, st, "ci", `{"action":"closed"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("no fire expected when the filter doesn't match, got %+v", b)
	}
}

func TestServeWebhook_DisabledTriggerDoesNotFire(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: ".*", Disabled: true,
	})

	// The body matches the filter, but the trigger is disabled: the name still
	// exists (200, not 404) yet nothing fires.
	w := postWebhook(t, s, st, "ci", `{"action":"opened"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a disabled trigger still exists); body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a disabled trigger must not fire, got %+v", b)
	}
}

func TestServeWebhook_UnknownName404(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: ".*",
	})

	w := postWebhook(t, s, st, "nope", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("no fire expected for an unknown name, got %+v", b)
	}
}

func TestServeWebhook_EmptyName400(t *testing.T) {
	s := newService()
	st := webhookState()
	// A bare "/" path → no name.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })
	w := httptest.NewRecorder()
	s.serveWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestServeWebhook_JSONPathMatchFires(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "deploy", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "deploy", FilterType: fleet.WebhookFilterJSONPath,
		JSONPath: "$.ref", JSONValue: "refs/heads/main",
	})

	w := postWebhook(t, s, st, "deploy", `{"ref":"refs/heads/main","after":"abc"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if b := drainBatch(s); len(b) != 1 || b[0].trigger.Name != "deploy" {
		t.Fatalf("expected the deploy trigger to fire, got %+v", b)
	}
}

func TestServeWebhook_FiresAllFleetsWithName(t *testing.T) {
	s := newService()
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterRegex, Regex: ".*"}},
		}},
		"beta": {Name: "beta", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterRegex, Regex: ".*"}},
		}},
	}}

	w := postWebhook(t, s, st, "shared", `anything`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// All matches for one request arrive as a SINGLE batch (atomic enqueue).
	b := drainBatch(s)
	if len(b) != 2 {
		t.Fatalf("expected a 2-trigger batch, got %v", b)
	}
	fleets := map[string]bool{b[0].fleet: true, b[1].fleet: true}
	if !fleets["alpha"] || !fleets["beta"] {
		t.Fatalf("expected fires for both fleets, got %v", fleets)
	}
}

// TestServeWebhook_MultiFleetSelectiveFilter confirms that when two fleets carry
// the same webhook name but different filters, only the trigger whose filter
// actually matches the event fires (selective routing, not fire-everything).
func TestServeWebhook_MultiFleetSelectiveFilter(t *testing.T) {
	s := newService()
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterJSONPath, JSONPath: "$.env", JSONValue: "prod"}},
		}},
		"beta": {Name: "beta", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterJSONPath, JSONPath: "$.env", JSONValue: "staging"}},
		}},
	}}

	// env=prod matches only alpha's trigger.
	w := postWebhook(t, s, st, "shared", `{"env":"prod"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	b := drainBatch(s)
	if len(b) != 1 || b[0].fleet != "alpha" {
		t.Fatalf("only alpha's prod trigger should fire, got %+v", b)
	}
}

// TestServeWebhook_MultiTriggerShedIsAtomic confirms a request with MULTIPLE
// matching triggers enqueues all-or-nothing: when the scheduler can't accept the
// batch, the handler returns 503 and NOTHING is enqueued — so a sender retry can't
// double-fire a trigger that "partially" went through (the bug the batch design
// prevents).
func TestServeWebhook_MultiTriggerShedIsAtomic(t *testing.T) {
	s := &service{webhookFires: make(chan []webhookFire)} // unbuffered, no drainer
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterRegex, Regex: ".*"}},
		}},
		"beta": {Name: "beta", Settings: fleet.FleetSettings{
			Agents:   []fleet.Agent{{Name: "a"}},
			Triggers: []fleet.Trigger{{Name: "t", Type: fleet.TriggerWebhook, AgentNames: []string{"a"}, WebhookName: "shared", FilterType: fleet.WebhookFilterRegex, Regex: ".*"}},
		}},
	}}
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/shared", strings.NewReader("x")).WithContext(ctx)
	w := httptest.NewRecorder()
	s.serveWebhook(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a shed request must enqueue NOTHING (no partial fire), got %+v", b)
	}
}

func TestServeWebhook_SchedulerBusy503(t *testing.T) {
	// Unbuffered channel with no drainer + an already-cancelled request context:
	// enqueueWebhookFires can't hand off and the ctx.Done branch wins deterministically.
	s := &service{webhookFires: make(chan []webhookFire)}
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: ".*",
	})
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/ci", strings.NewReader("x")).WithContext(ctx)
	w := httptest.NewRecorder()
	s.serveWebhook(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestServeWebhook_JSONPathFormEncodedRejected is the issue #207 regression: a
// json-path trigger receiving GitHub's default form-urlencoded delivery (which is
// not JSON) is rejected with 400 — visible in the sender's delivery dashboard —
// rather than silently never matching. Nothing fires.
func TestServeWebhook_JSONPathFormEncodedRejected(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "pr", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "pr", FilterType: fleet.WebhookFilterJSONPath,
		JSONPath: "$.pull_request.user.login", JSONValue: "BenjaminBenetti",
	})

	// The selected value WOULD match if the body were raw JSON, but GitHub
	// delivers it form-encoded by default.
	body := formEncode(`{"pull_request":{"user":{"login":"BenjaminBenetti"}}}`)
	w := postWebhook(t, s, st, "pr", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (json-path filter needs a JSON body); body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a rejected delivery must enqueue nothing, got %+v", b)
	}
}

// TestServeWebhook_JSONPathNonJSONRejected covers any non-JSON body (not just
// form encoding) hitting a json-path trigger: plain text is also a 400.
func TestServeWebhook_JSONPathNonJSONRejected(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "deploy", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "deploy", FilterType: fleet.WebhookFilterJSONPath,
		JSONPath: "$.ref", JSONValue: "refs/heads/main",
	})

	w := postWebhook(t, s, st, "deploy", "not json at all")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-JSON body; body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a rejected delivery must enqueue nothing, got %+v", b)
	}
}

// TestServeWebhook_RegexFormEncodedStillFires confirms regex filters are
// unaffected by the json-body check: a regex matches the raw bytes of even a
// form-encoded body and fires normally (200), because the json-path string
// "BenjaminBenetti" survives url-encoding verbatim.
func TestServeWebhook_RegexFormEncodedStillFires(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "pr", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "pr", FilterType: fleet.WebhookFilterRegex, Regex: "BenjaminBenetti",
	})

	body := formEncode(`{"pull_request":{"user":{"login":"BenjaminBenetti"}}}`)
	w := postWebhook(t, s, st, "pr", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (regex accepts any body); body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); len(b) != 1 || b[0].trigger.Name != "pr" {
		t.Fatalf("expected the regex trigger to fire, got %+v", b)
	}
}

// TestServeWebhook_MixedRegexAndJSONPathNonJSONRejected covers the documented
// edge case: when one webhook name carries BOTH a regex and a json-path trigger
// and a non-JSON body arrives, the 400 wins (the regex trigger does NOT fire) so
// the json-path misconfiguration stays visible and a retry can't double-fire.
func TestServeWebhook_MixedRegexAndJSONPathNonJSONRejected(t *testing.T) {
	s := newService()
	st := webhookState(
		fleet.Trigger{
			Name: "rx", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
			WebhookName: "pr", FilterType: fleet.WebhookFilterRegex, Regex: ".*",
		},
		fleet.Trigger{
			Name: "jp", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
			WebhookName: "pr", FilterType: fleet.WebhookFilterJSONPath,
			JSONPath: "$.action", JSONValue: "opened",
		},
	)

	w := postWebhook(t, s, st, "pr", formEncode(`{"action":"opened"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when a json-path trigger shares the name; body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("the 400 must win — nothing fires, got %+v", b)
	}
}

// TestServeWebhook_DisabledJSONPathDoesNotReject confirms a DISABLED json-path
// trigger doesn't arm the json-body check: a non-JSON body is accepted (200, no
// fire) because the disabled trigger isn't active.
func TestServeWebhook_DisabledJSONPathDoesNotReject(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "pr", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "pr", FilterType: fleet.WebhookFilterJSONPath,
		JSONPath: "$.action", JSONValue: "opened", Disabled: true,
	})

	w := postWebhook(t, s, st, "pr", formEncode(`{"action":"opened"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a disabled json-path trigger doesn't reject); body=%q", w.Code, w.Body.String())
	}
	if b := drainBatch(s); b != nil {
		t.Fatalf("a disabled trigger must not fire, got %+v", b)
	}
}

func TestServeWebhook_BodyTooLarge(t *testing.T) {
	s := newService()
	st := webhookState(fleet.Trigger{
		Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
		WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: ".*",
	})
	orig := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) { return st, nil }
	t.Cleanup(func() { scheduleLoadState = orig })

	big := strings.Repeat("x", maxWebhookBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/ci", strings.NewReader(big))
	w := httptest.NewRecorder()
	s.serveWebhook(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}
