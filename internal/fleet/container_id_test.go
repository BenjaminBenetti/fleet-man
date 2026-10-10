package fleet

import "testing"

func TestShortContainerID(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"short docker id", "abc123", "abc123"},
		{"docker id", "4f3c2a1b9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b", "4f3c2a1b9e8d"},
		{"exactly 12", "abcdef123456", "abcdef123456"},
		{"coder workspace", "fleet-alpha-agent-1", "fleet-alpha-"},
		{"kubernetes", "k8s:c-7f3a9b/fleet/fi-alpha-agent-1-0a1b2c3d@g1", "fleet/fi-alpha-agent-1-0a1b2c3d"},
		{"kubernetes after rebuilds", "k8s:c-7f3a9b/team-ns/fi-x-y-12345678@g42", "team-ns/fi-x-y-12345678"},
		{"kubernetes cluster id with slash", "k8s:arn:aws:eks:us-east-1:1:cluster/prod/ns/sts@g3", "ns/sts"},
		{"malformed kubernetes falls back", "k8s:ns/sts@g1", "k8s:ns/sts@g"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortContainerID(tc.in); got != tc.want {
				t.Errorf("ShortContainerID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestKubeContainerIDRoundTrip(t *testing.T) {
	ids := []KubeContainerID{
		{Cluster: "c-7f3a9b", Namespace: "fleet", StatefulSet: "fi-alpha-agent-1-0a1b2c3d", Generation: 1},
		{Cluster: "c", Namespace: "n", StatefulSet: "s", Generation: 1234567},
		{Cluster: "arn:aws:eks:us-east-1:1:cluster/prod", Namespace: "fleet", StatefulSet: "fi-a-b-00000000", Generation: 2},
		{Cluster: "a@g1", Namespace: "fleet", StatefulSet: "sts", Generation: 5},
	}
	for _, id := range ids {
		got, ok := ParseKubeContainerID(id.String())
		if !ok || got != id {
			t.Errorf("ParseKubeContainerID(%q) = %+v, %v; want %+v", id.String(), got, ok, id)
		}
	}
}

func TestParseKubeContainerIDRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"4f3c2a1b9e8d",
		"fleet-alpha-agent-1",
		"k8s:",
		"k8s:c/ns/sts",
		"k8s:c/ns/sts@g",
		"k8s:c/ns/sts@g0",
		"k8s:c/ns/sts@g01",
		"k8s:c/ns/sts@g-1",
		"k8s:c/ns/sts@g+1",
		"k8s:c/ns/sts@gx",
		"k8s:ns/sts@g1",
		"k8s:/ns/sts@g1",
		"k8s:c//sts@g1",
		"k8s:c/ns/@g1",
		"k8s:c/ns/st@s@g1",
		"k8s:c/n@s/sts@g1",
		"K8S:c/ns/sts@g1",
	} {
		if id, ok := ParseKubeContainerID(in); ok {
			t.Errorf("ParseKubeContainerID(%q) = %+v, want rejected", in, id)
		}
	}
}

// FuzzParseKubeContainerID checks that only the canonical form parses: any
// accepted string formats back to itself.
func FuzzParseKubeContainerID(f *testing.F) {
	f.Add("k8s:c-7f3a9b/fleet/fi-alpha-agent-1-0a1b2c3d@g1")
	f.Add("k8s:a/b/c/d@g10")
	f.Add("k8s:c/ns/sts@g01")
	f.Fuzz(func(t *testing.T, in string) {
		id, ok := ParseKubeContainerID(in)
		if ok && id.String() != in {
			t.Fatalf("ParseKubeContainerID(%q) = %+v, which formats as %q", in, id, id.String())
		}
	})
}
