package shellquote

import "testing"

func TestSingle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"/tmp/a b.md", "'/tmp/a b.md'"},
		{"it's", `'it'\''s'`},
		{"a'b'c", `'a'\''b'\''c'`},
		{"$HOME `id` \"x\"", "'$HOME `id` \"x\"'"}, // expansion characters stay inert
	}
	for _, c := range cases {
		if got := Single(c.in); got != c.want {
			t.Errorf("Single(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEscapeSingle(t *testing.T) {
	if got := EscapeSingle("it's"); got != `it'\''s` {
		t.Errorf("EscapeSingle: %q", got)
	}
	if got := EscapeSingle("plain"); got != "plain" {
		t.Errorf("EscapeSingle should pass quotes-free input through, got %q", got)
	}
}
