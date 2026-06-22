package fleet

import "testing"

func TestMatchesWebhook_Regex(t *testing.T) {
	tr := Trigger{Type: TriggerWebhook, FilterType: WebhookFilterRegex, Regex: `"action"\s*:\s*"opened"`}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"match", `{"action": "opened", "number": 5}`, true},
		{"no spaces", `{"action":"opened"}`, true},
		{"different value", `{"action": "closed"}`, false},
		{"absent", `{"number": 5}`, false},
		{"empty body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tr.MatchesWebhook([]byte(c.body)); got != c.want {
				t.Fatalf("MatchesWebhook(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestMatchesWebhook_RegexEmptyMatchesAll(t *testing.T) {
	// An empty expression is a no-op filter (matches everything). NormalizeTrigger
	// forbids empty regex, but the matcher must stay well-defined anyway.
	tr := Trigger{Type: TriggerWebhook, FilterType: WebhookFilterRegex, Regex: ""}
	if !tr.MatchesWebhook([]byte("anything at all")) {
		t.Fatal("empty regex should match any body")
	}
}

func TestMatchesWebhook_JSONPath(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		value string
		body  string
		want  bool
	}{
		{"string equal", "$.action", "opened", `{"action":"opened"}`, true},
		{"string not equal", "$.action", "opened", `{"action":"closed"}`, false},
		{"no leading dollar", "action", "opened", `{"action":"opened"}`, true},
		{"nested", "$.repository.name", "fleet-man", `{"repository":{"name":"fleet-man"}}`, true},
		{"array index", "$.commits[0].id", "abc", `{"commits":[{"id":"abc"},{"id":"def"}]}`, true},
		{"array index second", "$.commits[1].id", "def", `{"commits":[{"id":"abc"},{"id":"def"}]}`, true},
		{"bracket key", "$['action']", "opened", `{"action":"opened"}`, true},
		{"top-level array", "$[0].id", "abc", `[{"id":"abc"}]`, true},
		{"number literal", "$.number", "5", `{"number":5}`, true},
		{"number not equal", "$.number", "6", `{"number":5}`, false},
		{"bool true", "$.draft", "true", `{"draft":true}`, true},
		{"bool false", "$.draft", "false", `{"draft":false}`, true},
		{"presence hit", "$.action", "", `{"action":"opened"}`, true},
		{"presence miss", "$.action", "", `{"other":1}`, false},
		{"presence null is absent", "$.action", "", `{"action":null}`, false},
		{"path missing", "$.action", "opened", `{"other":"x"}`, false},
		{"index out of range", "$.commits[5].id", "abc", `{"commits":[{"id":"abc"}]}`, false},
		{"not json", "$.action", "opened", `not json at all`, false},
		{"wrong container type", "$.action.deep", "x", `{"action":"opened"}`, false},
		{"unsupported wildcard", "$.commits[*].id", "abc", `{"commits":[{"id":"abc"}]}`, false},
		// Malformed paths fail CLOSED (never silently normalized to a valid path).
		{"double dot rejected", "$.a..b", "x", `{"a":{"b":"x"}}`, false},
		{"trailing dot rejected", "$.a.", "x", `{"a":"x"}`, false},
		{"dot-bracket rejected", "$.a.[0]", "x", `{"a":["x"]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := Trigger{Type: TriggerWebhook, FilterType: WebhookFilterJSONPath, JSONPath: c.path, JSONValue: c.value}
			if got := tr.MatchesWebhook([]byte(c.body)); got != c.want {
				t.Fatalf("MatchesWebhook path=%q value=%q body=%q = %v, want %v", c.path, c.value, c.body, got, c.want)
			}
		})
	}
}

func TestMatchesWebhook_NonWebhookType(t *testing.T) {
	tr := Trigger{Type: TriggerSchedule, FilterType: WebhookFilterRegex, Regex: ".*"}
	if tr.MatchesWebhook([]byte("anything")) {
		t.Fatal("a non-webhook trigger must never match a webhook event")
	}
}

func TestValidateJSONPath(t *testing.T) {
	valid := []string{"$.a", "a", "$.a.b", "a.b[0].c", "$['a']['b']", "$[0].name", "a[0][1]"}
	for _, p := range valid {
		if err := ValidateJSONPath(p); err != nil {
			t.Errorf("ValidateJSONPath(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{"", "$", "$.", "a..b", "a.", "$.a.[0]", "a[", "a[]", "a['x]"}
	for _, p := range invalid {
		if err := ValidateJSONPath(p); err == nil {
			t.Errorf("ValidateJSONPath(%q) = nil, want an error", p)
		}
	}
}
