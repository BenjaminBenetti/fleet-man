package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// webhook.go is fleetd's side of the automation webhook (issue #193 — the
// delivery the #188 framework deferred). The gateway accepts a POST at
// <public-url>/webhook/<name> and reverse-proxies it down a TagWebhook tunnel
// stream to the http.Server this handler backs. Each request is one inbound
// event: its path names the webhook, its body is matched against every webhook
// trigger (across all fleets) carrying that name, and matching triggers fire
// their agents via the scheduler.
//
// There is NO auth here, by design: the gateway only forwards events for a
// session whose unguessable public URL the user chose to share, and that URL IS
// the capability — the same security model as the gateway's MCP proxy.

// maxWebhookBodySize bounds the event body fleetd reads for filter matching, so a
// huge or hostile POST can't exhaust memory. 1 MiB comfortably covers real
// webhook payloads — only the small routing fields a regex / json path inspects
// matter, not the whole delivery.
const maxWebhookBodySize = 1 << 20

// webhookEnqueueTimeout bounds how long the receiver waits to hand a matched
// event to the scheduler before shedding it (503). The scheduler drains between
// ticks, so this only trips under sustained overload.
const webhookEnqueueTimeout = 5 * time.Second

// webhookHandler returns the http.Handler fleetd serves over the tunnel's
// TagWebhook streams.
func (s *service) webhookHandler() http.Handler {
	return http.HandlerFunc(s.serveWebhook)
}

// serveWebhook routes one inbound webhook event to the matching triggers.
func (s *service) serveWebhook(w http.ResponseWriter, r *http.Request) {
	name := firstPathSegment(r.URL.Path)
	if name == "" {
		flog.Warn("webhook: rejected", "reason", "empty-name", "status", 400)
		http.Error(w, "webhook name required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodySize))
	if err != nil {
		// Not logged: this fires on the unauthenticated path for ANY name, so a
		// prober could otherwise grow fleet.log one line per request. The sender
		// still gets the 413 (surfaced in its delivery dashboard).
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	st, err := scheduleLoadState()
	if err != nil {
		flog.Warn("webhook: load state failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	fires, nameFound, jsonPathActive := collectWebhookFires(st, name, body)
	if !nameFound {
		// No trigger anywhere carries this name. Same 404 the gateway gives an
		// unknown id, so a probe can't enumerate configured webhook names. Not
		// logged: an unknown name is attacker-controllable probe noise on this
		// unauthenticated path, so recording it would let a prober grow fleet.log.
		http.Error(w, "no webhook trigger with that name", http.StatusNotFound)
		return
	}

	// A json-path filter can only evaluate a JSON body. The classic trap is
	// GitHub's webhook default content type, application/x-www-form-urlencoded,
	// which delivers the event as a `payload=<url-encoded-json>` form field
	// rather than raw JSON — so json.Decode fails and the filter silently never
	// matches (issue #207). We don't try to unwrap form encodings (their shape is
	// sender-specific and not reliably recoverable); instead, when an active
	// json-path trigger carries this name but the body isn't JSON, reject the
	// delivery with 400. fleetd's response is reverse-proxied straight back to the
	// sender, so the failure shows up in the sender's delivery dashboard (e.g.
	// GitHub's "Recent Deliveries") instead of vanishing. Regex triggers match raw
	// bytes and accept any body, so they are unaffected by this check.
	//
	// If a name carries BOTH a regex and a json-path trigger and a non-JSON body
	// arrives, the 400 wins (nothing fires): returning 200 would hide the
	// misconfiguration, and firing the regex trigger then returning 400 would
	// double-fire it on the sender's retry. Note this is keyed on the webhook NAME
	// globally: jsonPathActive is OR'd across EVERY fleet (collectWebhookFires
	// scans them all, since one name can drive triggers in several fleets), so a
	// json-path trigger for this name in ANY fleet makes a non-JSON delivery 400 —
	// even for a regex trigger of the same name in a DIFFERENT fleet. That cross-
	// fleet blast radius is intended: a non-JSON delivery to a name any fleet
	// treats as json-path is a misconfiguration worth surfacing loudly, not
	// silently half-firing whichever same-named regex triggers happen to match.
	if jsonPathActive && !bodyIsJSON(body) {
		flog.Warn("webhook: rejected", "reason", "non-json", "name", clipName(name), "status", 400)
		http.Error(w, "json-path webhook filter requires a JSON body; set the webhook content type to application/json", http.StatusBadRequest)
		return
	}

	if len(fires) == 0 {
		// Name found, but no trigger's filter matched this event — accepted, nothing
		// to do. Returning 200 (not 404) tells the sender the webhook is wired.
		flog.Info("webhook: received", "name", name, "fired", 0)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "received: no trigger matched the event\n")
		return
	}

	// Plan every matched firing before replying: the limiter reserves each
	// one's runs (to start now, or in its trigger's queue), so the reply says
	// what became of the delivery. A firing whose trigger's queue is full is
	// dropped; when every one is, nothing is enqueued and the sender gets a
	// 429 to retry later.
	items := make([]firingItem, len(fires))
	for i, f := range fires {
		items[i] = firingItem{fleet: f.fleet, trigger: f.trigger}
	}
	lim := s.limiter()
	plans := lim.plan(s.runChecks(), loadScheduleState, items, time.Now())
	var fired, queued int
	var dropped []string
	retryAfter := time.Duration(0)
	for i, p := range plans {
		fires[i].plan = p
		switch {
		case p.dropped != "":
			dropped = append(dropped, p.trigger.Name)
			if retryAfter == 0 || p.retryAfter < retryAfter {
				retryAfter = p.retryAfter
			}
		case p.fired():
			fired++
		default:
			queued++
		}
	}
	if len(dropped) == len(plans) {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		flog.Warn("webhook: every matching trigger's queue is full", "name", name, "triggers", len(plans), "status", 429, "retry_after", secs)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		http.Error(w, fmt.Sprintf("the queue of every matching trigger is full (%s); retry later", strings.Join(dropped, ", ")), http.StatusTooManyRequests)
		return
	}

	// Hand the WHOLE matched set to the scheduler as one batch — an all-or-nothing
	// enqueue. Webhook senders retry on a non-2xx, so a partial enqueue (some fires
	// in, then a 503) would re-fire the already-queued triggers on retry; a single
	// send makes the request atomic, so a 503 means nothing fired (its
	// reservations given back) and the retry is clean.
	if !s.enqueueWebhookFires(r.Context(), fires) {
		lim.discard(plans)
		flog.Warn("webhook: enqueue shed", "name", name, "triggers", len(fires))
		http.Error(w, "fleet scheduler busy, retry shortly", http.StatusServiceUnavailable)
		return
	}

	flog.Info("webhook: received", "name", name, "fired", fired, "queued", queued, "dropped", len(dropped))
	if queued == 0 && len(dropped) == 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "received: %d trigger(s) fired\n", fired)
		return
	}
	// Accepted, but not all of it runs now: 202, naming the dropped triggers
	// (each drop is in that trigger's log too).
	w.WriteHeader(http.StatusAccepted)
	msg := fmt.Sprintf("received: %d fired, %d queued", fired, queued)
	if len(dropped) > 0 {
		msg += fmt.Sprintf("; dropped, their queues full: %s", strings.Join(dropped, ", "))
	}
	_, _ = io.WriteString(w, msg+"\n")
}

// collectWebhookFires scans every fleet for webhook triggers named name and
// returns the subset whose filter matches body, whether ANY trigger with that
// name exists at all (to distinguish 404 from "matched nothing"), and whether any
// ACTIVE (non-disabled) trigger with that name uses the json-path filter (so the
// caller can reject a non-JSON body with 400 — see serveWebhook). Firing all
// matches across all fleets is intentional: webhook names are per-fleet in the
// model, so the same name can legitimately drive triggers in several fleets, and
// the common single-match case degenerates to exactly one fire.
func collectWebhookFires(st *state.State, name string, body []byte) (fires []triggerFire, nameFound, jsonPathActive bool) {
	for fleetName, f := range st.Fleets {
		if f == nil {
			continue
		}
		for _, t := range f.Settings.Triggers {
			if t.Type != fleet.TriggerWebhook || t.WebhookName != name {
				continue
			}
			nameFound = true
			if t.Disabled {
				continue // a disabled trigger still exists (200, not 404) but never fires
			}
			if t.FilterType == fleet.WebhookFilterJSONPath {
				jsonPathActive = true
			}
			if t.MatchesWebhook(body) {
				fires = append(fires, triggerFire{fleet: fleetName, trigger: t, body: body})
			}
		}
	}
	return fires, nameFound, jsonPathActive
}

// bodyIsJSON reports whether body decodes as a single JSON value — the same parse
// MatchesWebhook's json-path filter performs (json.Decoder, so trailing
// whitespace is fine and trailing junk is ignored, matching the matcher exactly).
// It is used to reject a json-path delivery whose body isn't JSON, rather than let
// the filter silently never match. The body is already size-bounded by the
// MaxBytesReader in serveWebhook.
func bodyIsJSON(body []byte) bool {
	var v any
	return json.NewDecoder(bytes.NewReader(body)).Decode(&v) == nil
}

// enqueueWebhookFires hands a request's matched events to the scheduler goroutine
// as ONE batch (so the enqueue is atomic — see serveWebhook), bounded by
// webhookEnqueueTimeout and the request's own context. It returns false when the
// scheduler can't accept the batch in time (overloaded), has stopped, or the
// request was cancelled — the caller turns that into a 503.
func (s *service) enqueueWebhookFires(ctx context.Context, fires []triggerFire) bool {
	if s.triggerFires == nil || len(fires) == 0 {
		return false
	}
	return s.handFires(ctx, fires, webhookEnqueueTimeout)
}

// clipName bounds an externally-supplied webhook name before it is written to
// the append-only event log, so a prober hitting this unauthenticated endpoint
// with unknown / oversized names can't emit arbitrarily long log lines. slog
// already escapes the value, so only the length needs bounding.
func clipName(name string) string {
	const maxRunes = 128
	if len(name) <= maxRunes { // bytes <= maxRunes implies runes <= maxRunes
		return name
	}
	runes := []rune(name)
	if len(runes) <= maxRunes {
		return name
	}
	// Cut on a rune boundary so a multi-byte rune is never split.
	return string(runes[:maxRunes]) + "...(clipped)"
}

// firstPathSegment returns the first path segment (the webhook name), trimming
// the leading slash and dropping any extra path the sender appended. The path is
// already percent-decoded by net/http, so a name with spaces/specials matches the
// trigger's raw WebhookName.
func firstPathSegment(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}
