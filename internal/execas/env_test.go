package execas

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvListKeepsOrderAndReplacesInPlace(t *testing.T) {
	e := newEnvList([]string{"A=1", "B=2", "noequals", "=nameless", "A=3", "C="})
	e.set("B", "4")
	e.set("D", "5")
	e.setDefault("A", "ignored")
	e.setDefault("E", "6")
	e.unset("C")
	e.unset("missing")
	e.merge(map[string]string{"Z": "z", "B": "7", "Y": "y"})
	want := []string{"A=3", "B=7", "D=5", "E=6", "Y=y", "Z=z"}
	if got := e.entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	c := e.clone()
	c.set("A", "changed")
	if v, _ := e.get("A"); v != "3" {
		t.Fatal("a clone shares state with its original")
	}
}

func TestContainerEnvDropsOnlyTheRuntimesHome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      []string
		selfHome string
		wantHome string
		kept     bool
	}{
		{"runc set root's home", []string{"PATH=/bin", "HOME=/root"}, "/root", "", false},
		{"the image set its own", []string{"HOME=/opt/app"}, "/root", "/opt/app", true},
		{"tier C pod uid's home", []string{"HOME=/home/fleetpod"}, "/home/fleetpod", "", false},
		{"no HOME at all", []string{"PATH=/bin"}, "/root", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := containerEnv(tc.env, tc.selfHome).get("HOME")
			if ok != tc.kept || got != tc.wantHome {
				t.Fatalf("HOME = %q (set %v), want %q (set %v)", got, ok, tc.wantHome, tc.kept)
			}
		})
	}
}

func TestSubstituteContainerEnvMatchesTheCLI(t *testing.T) {
	env := newEnvList([]string{"PATH=/usr/bin:/bin", "EMPTY=", "NAME=fleet"})
	for _, tc := range []struct{ in, want string }{
		{"${containerEnv:PATH}:/extra", "/usr/bin:/bin:/extra"},
		{"${containerEnv:MISSING}", ""},
		{"${containerEnv:MISSING:fallback}", "fallback"},
		{"${containerEnv:MISSING:a:b}", "a"},   // the CLI splits the default on ':' too
		{"${containerEnv:EMPTY:fallback}", ""}, // set but empty is set
		{"${containerEnv:}", ""},
		{"a ${containerEnv:NAME} b ${containerEnv:NAME}", "a fleet b fleet"},
		{"${localEnv:HOME} ${containerWorkspaceFolder}", "${localEnv:HOME} ${containerWorkspaceFolder}"},
		{"$NAME ${NAME}", "$NAME ${NAME}"},
		{"no refs", "no refs"},
	} {
		got, err := substituteContainerEnv(tc.in, env)
		if err != nil {
			t.Errorf("substitute(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("substitute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := substituteContainerEnv("x${containerEnv}y", env); err == nil || !strings.Contains(err.Error(), "no environment variable name") {
		t.Fatalf("${containerEnv} without a name: err = %v", err)
	}
}
