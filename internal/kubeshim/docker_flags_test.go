package kubeshim

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// helpFlagRE reads one option line of docker's --help: "  -e, --env list" or
// "      --rm". A word after the name is the value's type.
var helpFlagRE = regexp.MustCompile(`^\s+(?:-([a-zA-Z]), )?--([a-z0-9-]+)(?: ([a-z0-9-]+))?\s`)

// TestFlagTablesMatchDocker checks the shim's option tables against the
// docker CLI on this host: every option `docker run`, `docker create`, `docker
// exec` and `docker inspect` list must be in the table with the same shorthand
// and the same need for a value, or the shim misreads the argv. Docker's
// hidden options are in the tables too, so only this direction is checked. It
// runs with FLEET_KUBESHIM_CLI=1 (the kubeshim-cli CI job), since a docker
// upgrade on a runner should not fail the unit tests.
func TestFlagTablesMatchDocker(t *testing.T) {
	if os.Getenv("FLEET_KUBESHIM_CLI") != "1" {
		t.Skip("set FLEET_KUBESHIM_CLI=1 to check the option tables against docker")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("FLEET_KUBESHIM_CLI=1 needs docker on PATH")
	}
	for _, tc := range []struct {
		cmd []string
		fs  *flagSet
	}{
		{[]string{"run", "--help"}, runFlags},
		{[]string{"create", "--help"}, runFlags},
		{[]string{"exec", "--help"}, execFlags},
		{[]string{"inspect", "--help"}, inspectFlags},
		{[]string{"--help"}, globalFlags},
	} {
		out, err := exec.Command(docker, tc.cmd...).Output()
		if err != nil {
			t.Fatalf("docker %v: %v", tc.cmd, err)
		}
		n := 0
		for _, line := range splitLines(string(out)) {
			m := helpFlagRE.FindStringSubmatch(line + " ")
			if m == nil {
				continue
			}
			n++
			short, name, typ := m[1], m[2], m[3]
			spec, ok := tc.fs.long[name]
			if !ok {
				t.Errorf("docker %v has --%s, which the shim's table lacks", tc.cmd[0], name)
				continue
			}
			if spec.short != short {
				t.Errorf("docker %v --%s: shorthand %q, table %q", tc.cmd[0], name, short, spec.short)
			}
			if takesValue := typ != ""; takesValue != spec.value {
				t.Errorf("docker %v --%s: takes a value %v, table %v", tc.cmd[0], name, takesValue, spec.value)
			}
		}
		if n == 0 {
			t.Errorf("read no options from docker %v --help", tc.cmd)
		}
		t.Logf("docker %v: %d options checked", tc.cmd, n)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	return append(lines, s[start:])
}
