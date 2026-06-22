package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// automation_match.go decides whether an incoming webhook event fires a webhook
// Trigger (issue #193 — the delivery side of the automation framework). The
// gateway proxies a POST at <public-url>/webhook/<name> down to fleetd, which
// finds the matching webhook triggers by name and asks each one — via
// MatchesWebhook — whether THIS event body should activate its agents.
//
// The matcher is pure (body in, bool out) so it lives with the model and is
// unit-tested without any tunnel/server machinery. It assumes the trigger was
// already validated by NormalizeTrigger (regex compiles, json path non-empty),
// but stays defensive anyway.

// MatchesWebhook reports whether a webhook event with the given raw body should
// fire this trigger. It is only meaningful for TriggerWebhook triggers; any
// other type returns false.
//
//   - regex filter: the raw body must match the (already-validated) expression.
//     An empty expression matches everything (treated as "no filter").
//   - json-path filter: the body is parsed as JSON and the value at JSONPath is
//     compared to JSONValue. An empty JSONValue is a presence check (fires when
//     the path resolves to a non-null value). A body that is not valid JSON, or a
//     path that does not resolve, never matches.
func (t Trigger) MatchesWebhook(body []byte) bool {
	if t.Type != TriggerWebhook {
		return false
	}
	switch t.FilterType {
	case WebhookFilterRegex:
		re, err := regexp.Compile(t.Regex)
		if err != nil {
			return false
		}
		return re.Match(body)
	case WebhookFilterJSONPath:
		// UseNumber so a JSON number compares by its literal text ("5", "1.5")
		// rather than float64's lossy/normalized formatting.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var data any
		if err := dec.Decode(&data); err != nil {
			return false
		}
		v, ok := jsonPathLookup(data, t.JSONPath)
		if !ok {
			return false
		}
		if t.JSONValue == "" {
			return v != nil // presence check
		}
		return jsonValueEquals(v, t.JSONValue)
	default:
		return false
	}
}

// jsonPathLookup resolves a minimal JSONPath against a decoded JSON value and
// returns the value at that path. The supported subset is intentionally small —
// the cases real webhook routing needs:
//
//	$.a.b        dotted object keys (leading $ optional)
//	a.b[0].c     array indices
//	$['a']['b']  bracketed/quoted object keys
//	$[0].name    a leading array index
//
// Wildcards, filter expressions ([?(...)]), recursive descent (..) and slices
// are NOT supported; a path using them simply does not resolve (returns false),
// so a trigger relying on them never fires rather than firing on the wrong event.
func jsonPathLookup(data any, path string) (any, bool) {
	toks, ok := tokenizeJSONPath(path)
	if !ok {
		return nil, false
	}
	cur := data
	for _, tok := range toks {
		switch key := tok.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			v, ok := m[key]
			if !ok {
				return nil, false
			}
			cur = v
		case int:
			a, ok := cur.([]any)
			if !ok || key < 0 || key >= len(a) {
				return nil, false
			}
			cur = a[key]
		}
	}
	return cur, true
}

// tokenizeJSONPath parses the supported JSONPath subset into a sequence of
// object-key (string) and array-index (int) tokens. It is STRICT: malformed
// syntax — consecutive dots (`a..b`, recursive descent), a trailing dot (`a.`),
// or a dot before a bracket (`a.[0]`) — returns false rather than being silently
// normalized, so a misconfigured path fails CLOSED (no match, no spurious fire)
// instead of matching the wrong JSON structure.
func tokenizeJSONPath(path string) ([]any, bool) {
	s := strings.TrimSpace(path)
	s = strings.TrimPrefix(s, "$")
	var toks []any
	// expectKey is set right after a '.' separator: a key MUST follow it (not
	// another '.', a '[', or end of input). A leading "$." is fine because
	// expectKey starts false, so the first dot just enables the first key.
	expectKey := false
	for i := 0; i < len(s); {
		switch s[i] {
		case '.':
			if expectKey {
				return nil, false // ".." — empty path segment
			}
			expectKey = true
			i++
		case '[':
			if expectKey {
				return nil, false // ".[" — a dot must be followed by a key
			}
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, false
			}
			inner := strings.TrimSpace(s[i+1 : i+end])
			i += end + 1
			if inner == "" {
				return nil, false
			}
			if q := inner[0]; q == '\'' || q == '"' {
				if len(inner) < 2 || inner[len(inner)-1] != q {
					return nil, false
				}
				toks = append(toks, inner[1:len(inner)-1])
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil {
				return nil, false
			}
			toks = append(toks, n)
		default:
			// A bare object key runs until the next separator.
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			toks = append(toks, s[i:j])
			expectKey = false
			i = j
		}
	}
	if expectKey {
		return nil, false // trailing '.'
	}
	if len(toks) == 0 {
		return nil, false
	}
	return toks, true
}

// ValidateJSONPath reports whether a JSONPath is well-formed for the supported
// subset (see jsonPathLookup). NormalizeTrigger uses it to reject a malformed
// path at configuration time, so the user gets immediate feedback instead of a
// trigger that silently never fires.
func ValidateJSONPath(path string) error {
	if _, ok := tokenizeJSONPath(path); !ok {
		return fmt.Errorf("invalid json path %q", path)
	}
	return nil
}

// jsonValueEquals reports whether a decoded JSON value equals the expected text.
// Strings compare directly; numbers compare by their literal JSON text (because
// the body was decoded with UseNumber); booleans compare as "true"/"false".
// Containers and null never equal a scalar expectation.
func jsonValueEquals(v any, expected string) bool {
	switch x := v.(type) {
	case string:
		return x == expected
	case bool:
		return strconv.FormatBool(x) == expected
	case json.Number:
		return x.String() == expected
	default:
		return false
	}
}
