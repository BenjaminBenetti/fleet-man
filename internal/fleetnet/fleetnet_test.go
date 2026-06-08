package fleetnet

import (
	"fmt"
	"testing"
)

// stubDocker replaces runDocker with handler for the duration of the test.
func stubDocker(t *testing.T, handler func(args []string) (string, error)) {
	t.Helper()
	orig := runDocker
	runDocker = func(args ...string) (string, error) { return handler(args) }
	t.Cleanup(func() { runDocker = orig })
}

func hasCall(calls [][]string, sub ...string) bool {
	for _, c := range calls {
		if len(c) >= len(sub) {
			match := true
			for i, s := range sub {
				if c[i] != s {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func TestNetworkName(t *testing.T) {
	cases := map[string]string{
		"alpha":    "fleet-alpha-net",
		"My Fleet": "fleet-my-fleet-net",
		"a/b:c":    "fleet-a-b-c-net",
	}
	for in, want := range cases {
		if got := NetworkName(in); got != want {
			t.Errorf("NetworkName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureNetworkCreatesWhenAbsent(t *testing.T) {
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "network" && args[1] == "inspect" {
			return "", fmt.Errorf("Error: No such network") // absent
		}
		return "", nil
	})

	name, err := EnsureNetwork("alpha")
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if name != NetworkName("alpha") {
		t.Fatalf("name = %q, want %q", name, NetworkName("alpha"))
	}
	if !hasCall(calls, "network", "create") {
		t.Fatalf("expected a network create, calls=%v", calls)
	}
}

func TestEnsureNetworkReusesWhenPresent(t *testing.T) {
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[0] == "network" && args[1] == "inspect" {
			return "abc123", nil // present
		}
		return "", nil
	})

	if _, err := EnsureNetwork("alpha"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if hasCall(calls, "network", "create") {
		t.Fatalf("present network must not be re-created, calls=%v", calls)
	}
}

func TestConnectInstanceIdempotent(t *testing.T) {
	stubDocker(t, func(args []string) (string, error) {
		if args[0] == "network" && args[1] == "inspect" {
			return "abc123", nil // present so EnsureNetwork is a no-op
		}
		if args[0] == "network" && args[1] == "connect" {
			// Simulate docker's already-attached error.
			return "Error response from daemon: endpoint with name x already exists in network fleet-alpha-net",
				fmt.Errorf("exit status 1")
		}
		return "", nil
	})
	if err := ConnectInstance("alpha", "container123"); err != nil {
		t.Fatalf("ConnectInstance must treat an already-attached endpoint as success, got %v", err)
	}
}

func TestConnectInstanceBlankContainerNoop(t *testing.T) {
	called := false
	stubDocker(t, func([]string) (string, error) { called = true; return "", nil })
	if err := ConnectInstance("alpha", ""); err != nil {
		t.Fatalf("blank container: %v", err)
	}
	if called {
		t.Fatal("ConnectInstance with a blank container id must not call docker")
	}
}

func TestRemoveNetworkAbsentIsSuccess(t *testing.T) {
	stubDocker(t, func([]string) (string, error) {
		return "Error: No such network: fleet-alpha-net", fmt.Errorf("exit status 1")
	})
	if err := RemoveNetwork("alpha"); err != nil {
		t.Fatalf("RemoveNetwork on an absent network should be nil, got %v", err)
	}
}

func TestRemoveNetworkActiveEndpointsErrors(t *testing.T) {
	stubDocker(t, func([]string) (string, error) {
		return "error while removing network: has active endpoints", fmt.Errorf("exit status 1")
	})
	if err := RemoveNetwork("alpha"); err == nil {
		t.Fatal("RemoveNetwork should surface a genuine (active-endpoints) failure")
	}
}
