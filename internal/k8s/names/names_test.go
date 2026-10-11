package names

import (
	"strings"
	"testing"
	"time"
)

// crossCheck validates a generated value against the API server's own
// grammar (k8s.io/apimachinery); names_apimachinery_test.go installs it in
// builds that may link apimachinery, so the -tags nok8s test run still passes.
var crossCheck = func(t *testing.T, kind, value string) {}

const testDaemonID = "0123456789abcdef0123456789abcdef"

var testCreated = time.Date(2026, 10, 10, 12, 0, 0, 123456789, time.UTC)

func TestSlug(t *testing.T) {
	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{"fleet-man", 16, "fleet-man"},
		{"Fleet-Man", 16, "fleet-man"},
		{"my_repo.v2", 16, "my-repo-v2"},
		{"--a--b--", 16, "a-b"},
		{"a  b", 16, "a-b"},
		{"élan", 16, "lan"},
		{"日本", 16, "x"},
		{"", 16, "x"},
		{"---", 16, "x"},
		{"abcdefghij-klmnop", 11, "abcdefghij"}, // a cut that ends on '-' drops it
		{"abcdef", 0, "a"},                      // the limit is at least 1
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 36, "abcdefghijklmnopqrstuvwxyz0123456789"},
	}
	for _, tt := range tests {
		if got := Slug(tt.in, tt.limit); got != tt.want {
			t.Errorf("Slug(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}

func TestInstanceKey(t *testing.T) {
	key := InstanceKey(testDaemonID, "fleet-man", "web", testCreated)
	if !ValidHash(key) {
		t.Fatalf("InstanceKey = %q, not a valid hash", key)
	}
	// The key is persisted (Instance.KubeKey) and carried by live objects, so
	// its derivation must never change: this pins it.
	if key != "mrm5ihjk" {
		t.Errorf("InstanceKey = %q, want mrm5ihjk (the derivation changed)", key)
	}
	// The same instant in another zone is the same key.
	if got := InstanceKey(testDaemonID, "fleet-man", "web", testCreated.In(time.FixedZone("x", 5*3600))); got != key {
		t.Errorf("zone changed the key: %q vs %q", got, key)
	}
	for name, other := range map[string]string{
		"daemon":     InstanceKey("ffff456789abcdef0123456789abcdef", "fleet-man", "web", testCreated),
		"fleet":      InstanceKey(testDaemonID, "fleet-mam", "web", testCreated),
		"instance":   InstanceKey(testDaemonID, "fleet-man", "wec", testCreated),
		"created":    InstanceKey(testDaemonID, "fleet-man", "web", testCreated.Add(time.Nanosecond)),
		"field join": InstanceKey(testDaemonID, "fleet-ma", "nweb", testCreated),
	} {
		if other == key {
			t.Errorf("changing the %s kept the key %q", name, key)
		}
	}
}

func TestFleetHash(t *testing.T) {
	h := FleetHash(testDaemonID, "minikube-abc234", "fleet-man")
	if !ValidHash(h) {
		t.Fatalf("FleetHash = %q, not a valid hash", h)
	}
	if h != "xlm3fdvd" {
		t.Errorf("FleetHash = %q, want xlm3fdvd (the derivation changed)", h)
	}
	// Two connections to one namespace get distinct per-fleet objects.
	if other := FleetHash(testDaemonID, "minikube-xyz234", "fleet-man"); other == h {
		t.Errorf("FleetHash ignores the cluster id: %q", h)
	}
}

func TestValidHash(t *testing.T) {
	for s, want := range map[string]bool{
		"abcd2345":  true,
		"abcd234":   false, // short
		"abcd23456": false,
		"abcd2341":  false, // 1 is not base32
		"ABCD2345":  false,
		"":          false,
	} {
		if got := ValidHash(s); got != want {
			t.Errorf("ValidHash(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestOwner(t *testing.T) {
	if got := Owner("0123456789ABCDEF0123"); got != "0123456789ab" {
		t.Errorf("Owner = %q, want 0123456789ab", got)
	}
	if got := Owner("abc"); got != "abc" {
		t.Errorf("Owner of a short id = %q", got)
	}
}

func TestInstance(t *testing.T) {
	key := InstanceKey(testDaemonID, "f", "i", testCreated)
	tests := []struct {
		fleet, instance, want string
	}{
		{"fleet-man", "web", "fi-fleet-man-web-" + key},
		{"Fleet_Man", "Web.1", "fi-fleet-man-web-1-" + key},
		// A long fleet keeps 16 characters, so the instance gets 23.
		{"a-very-long-fleet-name-indeed", "an-even-longer-instance-name-here", "fi-a-very-long-flee-an-even-longer-instance-" + key},
		// A short fleet leaves the instance its full 24.
		{"f", "an-even-longer-instance-name-here", "fi-f-an-even-longer-instance-" + key},
		{"日本", "😀", "fi-x-x-" + key},
	}
	for _, tt := range tests {
		got := Instance(tt.fleet, tt.instance, key)
		if got != tt.want {
			t.Errorf("Instance(%q, %q) = %q, want %q", tt.fleet, tt.instance, got, tt.want)
		}
		if len(got) > MaxInstanceName {
			t.Errorf("Instance(%q, %q) is %d characters", tt.fleet, tt.instance, len(got))
		}
	}
	long := Instance(strings.Repeat("f", 80), strings.Repeat("i", 80), key)
	if len(long) != MaxInstanceName {
		t.Errorf("two long names give %d characters, want exactly %d", len(long), MaxInstanceName)
	}
	if got := Pod("fi-a-b-" + key); got != "fi-a-b-"+key+"-0" {
		t.Errorf("Pod = %q", got)
	}
}

func TestGenerationNames(t *testing.T) {
	sts := "fi-a-b-" + InstanceKey(testDaemonID, "f", "i", testCreated)
	for _, tt := range []struct{ got, want string }{
		{SpecSecret(sts, 1), sts + "-spec-g1"},
		{EnvSecret(sts, 12), sts + "-env-g12"},
		{SeedJob(sts, 9999), sts + "-seed-g9999"},
		{ExportJob(sts, 3), sts + "-xprt-g3"},
		{RollbackJob(sts, 3), sts + "-rlbk-g3"},
		{CommitJob(sts, 4), sts + "-cmit-g4"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
	for gen, want := range map[int]bool{0: false, 1: true, MaxGeneration: true, MaxGeneration + 1: false, -1: false} {
		if ValidGeneration(gen) != want {
			t.Errorf("ValidGeneration(%d) = %v", gen, !want)
		}
	}
	// The longest StatefulSet name with the highest generation still gives a
	// Job name whose pods' job-name label is a label value.
	long := Instance(strings.Repeat("f", 80), strings.Repeat("i", 80), InstanceKey(testDaemonID, "f", "i", testCreated))
	for _, job := range []string{SeedJob(long, MaxGeneration), ExportJob(long, MaxGeneration), RollbackJob(long, MaxGeneration), CommitJob(long, MaxGeneration)} {
		if len(job) != 63 || !IsDNS1123Label(job) || !IsLabelValue(job) {
			t.Errorf("Job of the longest name = %q (%d)", job, len(job))
		}
	}
}

// A seed cache's id is its inputs' (each field counted on its own), its
// disk name a DNS label, and its trim Job's name fits the longest instance.
func TestSeedCacheNames(t *testing.T) {
	id := SeedCacheID(testDaemonID, "c", "sha256:abc", "/workspaces/x")
	if len(id) != SeedCacheIDLen || SeedCacheID(testDaemonID, "c", "sha256:abc", "/workspaces/x") != id {
		t.Fatalf("SeedCacheID = %q, want %d stable characters", id, SeedCacheIDLen)
	}
	for _, other := range []string{
		SeedCacheID(testDaemonID, "c", "sha256:abd", "/workspaces/x"),
		SeedCacheID(testDaemonID, "d", "sha256:abc", "/workspaces/x"),
		SeedCacheID("other-daemon", "c", "sha256:abc", "/workspaces/x"),
		SeedCacheID(testDaemonID, "c", "sha256:abc/workspaces/x"),
	} {
		if other == id {
			t.Errorf("another input gave the same id %q", id)
		}
	}
	if name := SeedCache(id); name != "fi-seedcache-"+id || !IsDNS1123Label(name) || !IsLabelValue(name) {
		t.Errorf("SeedCache = %q", name)
	}
	long := Instance(strings.Repeat("f", 80), strings.Repeat("i", 80), InstanceKey(testDaemonID, "f", "i", testCreated))
	if job := SeedCacheJob(long); !IsDNS1123Label(job) || !IsLabelValue(job) {
		t.Errorf("SeedCacheJob of the longest name = %q (%d)", job, len(job))
	}
}

func TestFleetObject(t *testing.T) {
	h := FleetHash(testDaemonID, "c", "fleet-man")
	if got := FleetObject("fleet-man", h, SuffixShare); got != "fs-fleet-man-"+h+"-share" {
		t.Errorf("FleetObject = %q", got)
	}
	long := FleetObject(strings.Repeat("F", 99), h, strings.Repeat("s", MaxFleetObjectSuffix))
	if len(long) != 63 || !IsDNS1035Label(long) {
		t.Errorf("the longest fleet object name %q (%d) is not a 63-character RFC 1035 label", long, len(long))
	}
}

func TestOwnerLease(t *testing.T) {
	got := OwnerLease(testDaemonID, "minikube-abc234")
	if got != "fleet-owner-0123456789ab-minikube-abc234" {
		t.Errorf("OwnerLease = %q", got)
	}
	if !IsDNS1123Label(got) {
		t.Errorf("OwnerLease %q is not a DNS label", got)
	}
	if OwnerLease(testDaemonID, "a") == OwnerLease(testDaemonID, "b") {
		t.Error("two connections share an owner Lease")
	}
}

func TestSelectorMatchesBase(t *testing.T) {
	base := Base("0123456789ab", "minikube-abc234")
	sel := Selector("0123456789ab", "minikube-abc234")
	parsed := map[string]string{}
	for _, term := range strings.Split(sel, ",") {
		k, v, ok := strings.Cut(term, "=")
		if !ok {
			t.Fatalf("selector term %q has no '='", term)
		}
		parsed[k] = v
	}
	if len(parsed) != len(base) {
		t.Fatalf("selector %q has %d terms, Base has %d labels", sel, len(parsed), len(base))
	}
	for k, v := range base {
		if parsed[k] != v {
			t.Errorf("selector %s=%q, Base %s=%q", k, parsed[k], k, v)
		}
	}
	if base[LabelCluster] != "minikube-abc234" {
		t.Error("Base lacks the cluster label")
	}
}

func TestNewClusterID(t *testing.T) {
	tests := []struct{ hint, prefix string }{
		{"minikube", "minikube-"},
		{"arn:aws:eks:us-east-1:123456789012:cluster/prod", "arn-aws-eks-us-east-"},
		{"kind-kind", "kind-kind-"},
		{"1st-cluster", "k-1st-cluster-"},
		{"", "cluster-"},
		{"日本", "cluster-"},
	}
	for _, tt := range tests {
		id := NewClusterID(tt.hint, nil)
		if !strings.HasPrefix(id, tt.prefix) {
			t.Errorf("NewClusterID(%q) = %q, want prefix %q", tt.hint, id, tt.prefix)
		}
		if err := ValidateClusterID(id); err != nil {
			t.Errorf("NewClusterID(%q) = %q, invalid: %v", tt.hint, id, err)
		}
	}
	// A taken id is never returned.
	var seen []string
	calls := 0
	id := NewClusterID("minikube", func(c string) bool {
		calls++
		seen = append(seen, c)
		return calls < 3
	})
	if calls != 3 || id != seen[2] {
		t.Errorf("NewClusterID returned %q after %d calls (%v), want the third candidate", id, calls, seen)
	}
}

func TestValidateClusterID(t *testing.T) {
	for id, ok := range map[string]bool{
		"minikube-abc234":       true,
		"a":                     true,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"":                      false,
		"Minikube":              false,
		"1abc":                  false,
		"abc-":                  false,
		"a/b":                   false,
		"a.b":                   false,
		"a_b":                   false,
		"jobs":                  false, // ~/.fleet/kube/jobs
		"images":                false, // ~/.fleet/kube/images
		"..":                    false,
	} {
		if err := ValidateClusterID(id); (err == nil) != ok {
			t.Errorf("ValidateClusterID(%q) = %v, want ok=%v", id, err, ok)
		}
	}
}

func TestGrammar(t *testing.T) {
	for s, want := range map[string][3]bool{ // 1123, 1035, label value
		"abc":                   {true, true, true},
		"a-b":                   {true, true, true},
		"1ab":                   {true, false, true},
		"-ab":                   {false, false, false},
		"ab-":                   {false, false, false},
		"A":                     {false, false, true},
		"a_b.c":                 {false, false, true},
		"":                      {false, false, true},
		strings.Repeat("a", 63): {true, true, true},
		strings.Repeat("a", 64): {false, false, false},
	} {
		got := [3]bool{IsDNS1123Label(s), IsDNS1035Label(s), IsLabelValue(s)}
		if got != want {
			t.Errorf("grammar(%q) = %v, want %v", s, got, want)
		}
		crossCheck(t, "grammar", s)
	}
}

// FuzzK8sName holds every generated name and label value to the Kubernetes
// grammar of the object kind it names, for any fleet, instance, daemon and
// time, and checks the derivations stay deterministic.
func FuzzK8sName(f *testing.F) {
	for _, seed := range []struct {
		fleet, instance, daemon string
		nanos                   int64
	}{
		{"fleet-man", "web", testDaemonID, testCreated.UnixNano()},
		{"", "", "", 0},
		{"Fleet_Man.v2", "WEB 1", "ABC", -1},
		{strings.Repeat("ab-", 40), strings.Repeat("-z", 50), strings.Repeat("f", 64), 1 << 62},
		{"日本-fleet", "инстанс", "\x00\xff", 42},
		{"a.b.c", "x__y", "0", 7},
	} {
		f.Add(seed.fleet, seed.instance, seed.daemon, seed.nanos)
	}
	f.Fuzz(func(t *testing.T, fleet, instance, daemonID string, nanos int64) {
		created := time.Unix(0, nanos)
		key := InstanceKey(daemonID, fleet, instance, created)
		if !ValidHash(key) || key != InstanceKey(daemonID, fleet, instance, created) {
			t.Fatalf("InstanceKey %q invalid or unstable", key)
		}
		h8 := FleetHash(daemonID, "cluster-abc234", fleet)
		if !ValidHash(h8) {
			t.Fatalf("FleetHash %q invalid", h8)
		}

		sts := Instance(fleet, instance, key)
		if len(sts) > MaxInstanceName || !IsDNS1035Label(sts) || !strings.HasPrefix(sts, "fi-") || !strings.HasSuffix(sts, "-"+key) {
			t.Fatalf("Instance(%q, %q) = %q (%d): not a valid StatefulSet name", fleet, instance, sts, len(sts))
		}
		if strings.Contains(sts, "--") {
			t.Fatalf("Instance(%q, %q) = %q has an empty part", fleet, instance, sts)
		}
		crossCheck(t, "statefulset", sts)
		pod := Pod(sts)
		if !IsDNS1123Label(pod) {
			t.Fatalf("Pod(%q) = %q: not a DNS label", sts, pod)
		}
		crossCheck(t, "pod", pod)
		for _, gen := range []int{1, int(uint64(nanos)%MaxGeneration) + 1, MaxGeneration} {
			for _, job := range []string{SeedJob(sts, gen), ExportJob(sts, gen), RollbackJob(sts, gen), CommitJob(sts, gen)} {
				if !IsDNS1123Label(job) || !IsLabelValue(job) {
					t.Fatalf("Job name %q (gen %d): not a Job name", job, gen)
				}
				crossCheck(t, "job", job)
			}
			for _, secret := range []string{SpecSecret(sts, gen), EnvSecret(sts, gen)} {
				if !IsDNS1123Label(secret) {
					t.Fatalf("Secret name %q is not a DNS label", secret)
				}
				crossCheck(t, "pod", secret)
			}
		}
		// The pod's controller-revision-hash label: the name, '-', 10 characters.
		if rev := sts + "-" + strings.Repeat("a", 10); !IsLabelValue(rev) {
			t.Fatalf("controller-revision-hash %q is not a label value", rev)
		}

		for _, suffix := range []string{SuffixAnchor, SuffixShare, SuffixNet, SuffixAptCache, SuffixImageCache, SuffixBuildkit, SuffixInstancesPolicy, SuffixCachesPolicy, SuffixEgressPolicy} {
			name := FleetObject(fleet, h8, suffix)
			if !IsDNS1035Label(name) {
				t.Fatalf("FleetObject(%q, %q) = %q: not an RFC 1035 label", fleet, suffix, name)
			}
			crossCheck(t, "service", name)
		}

		owner := Owner(daemonID)
		labels := Base(owner, "cluster-abc234")
		labels[LabelFleet], labels[LabelKey], labels[LabelShareGroup], labels[LabelRole] = h8, key, h8, string(RoleInstance)
		for k, v := range labels {
			if IsLabelValue(v) {
				crossCheck(t, "label value", v)
				continue
			}
			// The owner label comes from the daemon id, which the daemon
			// generates as hex (owner.LoadIdentity); only fuzzed garbage ids
			// fail here.
			if k == LabelOwner {
				continue
			}
			t.Fatalf("label %s=%q is not a valid label value", k, v)
		}

		id := NewClusterID(fleet, nil)
		if err := ValidateClusterID(id); err != nil {
			t.Fatalf("NewClusterID(%q) = %q: %v", fleet, id, err)
		}
		crossCheck(t, "label value", id)
		if lease := OwnerLease(testDaemonID, id); !IsDNS1123Label(lease) {
			t.Fatalf("OwnerLease(%q) = %q: not a DNS label", id, lease)
		}
	})
}
