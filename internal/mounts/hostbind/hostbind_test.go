package hostbind

import "testing"

func TestNames(t *testing.T) {
	a := Name("/home/u/.aws")
	if a != Name("/home/u/.aws/") || a != Name("/home/u/./.aws") {
		t.Errorf("one host path has one copy whatever its spelling: %s", a)
	}
	if a == Name("/home/u/.gitconfig") {
		t.Errorf("two host paths share a copy: %s", a)
	}
	if !Is(a) || len(a) != len(Root)+1+8 {
		t.Errorf("Name(...) = %q, not .hostbind/<8 hex>", a)
	}
	for _, bad := range []string{".hostbind", ".hostbind/", ".hostbind/1a2b3c4", ".hostbind/1a2b3c4dz", ".hostbind/1A2B3C4D", ".mnt/1a2b3c4d", ".hostbind/1a2b3c4d/x"} {
		if Is(bad) {
			t.Errorf("Is(%q) = true", bad)
		}
	}
	if got := FileDir(a); got != ContainerDir+"/"+ID("/home/u/.aws") {
		t.Errorf("FileDir(%s) = %s", a, got)
	}
}

func TestOverlaps(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"/home/u/.fleet", "/home/u/.fleet", true},
		{"/home/u", "/home/u/.fleet", true},
		{"/home/u/.fleet/workspaces/f", "/home/u/.fleet", true},
		{"/", "/home/u/.fleet", true},
		{"/home/u/.fleet2", "/home/u/.fleet", false},
		{"/home/u/.aws", "/home/u/.fleet", false},
		{"/home/u/.fleet/", "/home/u/.fleet", true},
	} {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
