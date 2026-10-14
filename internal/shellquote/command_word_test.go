package shellquote

import "testing"

func TestCommandWord(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/usr/local/bin/fleet", "/usr/local/bin/fleet"},
		{"/home/jane/.local/bin/fleet-2.13.0+dev_x", "/home/jane/.local/bin/fleet-2.13.0+dev_x"},
		{"", "''"},
		{"/Users/Jane Doe/bin/fleet", "'/Users/Jane Doe/bin/fleet'"},
		{"/opt/it's/fleet", `'/opt/it'\''s/fleet'`},
		{"/opt/$x/fleet", "'/opt/$x/fleet'"},
		{"~/bin/fleet", "'~/bin/fleet'"},
		{"FOO=bar", "'FOO=bar'"},
		{"/opt/flëet", "'/opt/flëet'"},
	}
	for _, c := range cases {
		if got := CommandWord(c.in); got != c.want {
			t.Errorf("CommandWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
