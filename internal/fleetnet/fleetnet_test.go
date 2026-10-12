package fleetnet

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"
)

// stubDocker replaces runDocker with handler for the duration of the test.
func stubDocker(t *testing.T, handler func(args []string) (string, error)) {
	t.Helper()
	orig := runDocker
	runDocker = func(args ...string) (string, error) { return handler(args) }
	t.Cleanup(func() { runDocker = orig })
}

// stubHost replaces hostNetworks with nets (CIDRs) for the duration of the test.
func stubHost(t *testing.T, nets ...string) {
	t.Helper()
	orig := hostNetworks
	var prefixes []netip.Prefix
	for _, n := range nets {
		prefixes = append(prefixes, netip.MustParsePrefix(n))
	}
	hostNetworks = func() []netip.Prefix { return prefixes }
	t.Cleanup(func() { hostNetworks = orig })
}

// creates are the `docker network create` calls among calls.
func creates(calls [][]string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "network" && c[1] == "create" {
			out = append(out, c)
		}
	}
	return out
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
	stubHost(t)
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

// docker would give a new network the /16 a docker-in-docker instance's own
// dockerd gives its docker0 (172.18.0.0/16 next to a default bridge on
// 172.17), and the instance could then not join it: the network takes a
// subnet no docker engine hands out itself.
func TestEnsureNetworkTakesASubnetOutsideDockersPools(t *testing.T) {
	stubHost(t, "172.17.0.0/16", "172.18.0.0/16", "10.1.0.0/16")
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[1] == "inspect" {
			return "", fmt.Errorf("Error: No such network")
		}
		return "", nil
	})
	if _, err := EnsureNetwork("alpha"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	want := []string{"network", "create", "--subnet", "172.16.0.0/24", NetworkName("alpha")}
	if got := creates(calls); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("creates = %v, want [%v]", got, want)
	}
}

// A subnet the host reaches (an interface's, a route's) is never asked for,
// and one docker gave another network is passed over.
func TestEnsureNetworkSkipsSubnetsInUse(t *testing.T) {
	stubHost(t, "172.16.0.0/24")
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[1] == "inspect" {
			return "", fmt.Errorf("Error: No such network")
		}
		if slices.Contains(args, "172.16.1.0/24") {
			return "Error response from daemon: invalid pool request: Pool overlaps with other one on this address space", fmt.Errorf("exit status 1")
		}
		return "", nil
	})
	if _, err := EnsureNetwork("alpha"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	got := creates(calls)
	if len(got) != 2 || !slices.Contains(got[0], "172.16.1.0/24") || !slices.Contains(got[1], "172.16.2.0/24") {
		t.Fatalf("creates = %v, want 172.16.1.0/24 then 172.16.2.0/24", got)
	}
}

// With every subnet of the base in use, docker chooses, as before.
func TestEnsureNetworkFallsBackToDockersChoice(t *testing.T) {
	stubHost(t, "172.16.0.0/12")
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[1] == "inspect" {
			return "", fmt.Errorf("Error: No such network")
		}
		return "", nil
	})
	if _, err := EnsureNetwork("alpha"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	want := []string{"network", "create", NetworkName("alpha")}
	if got := creates(calls); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("creates = %v, want [%v]", got, want)
	}
}

// A create that fails for another reason is not tried on the next subnet.
func TestEnsureNetworkSurfacesACreateFailure(t *testing.T) {
	stubHost(t)
	var calls [][]string
	stubDocker(t, func(args []string) (string, error) {
		calls = append(calls, args)
		if args[1] == "inspect" {
			return "", fmt.Errorf("Error: No such network")
		}
		return "permission denied while trying to connect to the Docker daemon socket", fmt.Errorf("exit status 1")
	})
	if _, err := EnsureNetwork("alpha"); err == nil {
		t.Fatal("EnsureNetwork should surface docker's failure")
	}
	if got := creates(calls); len(got) != 1 {
		t.Fatalf("creates = %v, want one", got)
	}
}

func TestRoutedNetworks(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"tun0\t000010AC\t00000000\t0001\t0\t0\t0\t0000F0FF\t0\t0\t0\n" +
		"wg0\t0002A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
	got := routedNetworks([]byte(table))
	want := []netip.Prefix{
		netip.MustParsePrefix("172.17.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.2.0/24"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routedNetworks = %v, want %v", got, want)
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
