package cli

import (
	"bytes"
	"net"
	"testing"
)

// `fleet net-probe` prints the first URL that answers, else DIRECT (apt's
// proxy detector reads one line), and stays out of fleet --help.
func TestNetProbePrintsTheFirstURLThatAnswers(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "http://" + gone.Addr().String()
	_ = gone.Close()
	up := "http://" + l.Addr().String()

	run := func(args ...string) (string, error) {
		root := NewRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"net-probe", "--timeout", "1s"}, args...))
		err := root.Execute()
		return out.String(), err
	}
	if got, err := run(down, up); err != nil || got != up+"\n" {
		t.Fatalf("net-probe = %q, %v; want %q", got, err, up)
	}
	if got, err := run(down); err != nil || got != "DIRECT\n" {
		t.Fatalf("net-probe of a refusing URL = %q, %v; want DIRECT", got, err)
	}
	if _, err := run("not a url"); err == nil {
		t.Fatal("net-probe of a malformed URL succeeded")
	}
	cmd, _, err := NewRootCmd().Find([]string{"net-probe"})
	if err != nil || !cmd.Hidden {
		t.Fatalf("net-probe %v, hidden %v: it is in-instance plumbing", err, cmd != nil && cmd.Hidden)
	}
}
