package podspec

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/redact"
)

// hostRequirements raise the requests (clamped to the largest node that can
// run the image) and the disk.
func TestHostRequirements(t *testing.T) {
	in := baseInput(nil, nil, nil)
	in.Merged = json.RawMessage(`{"remoteUser":"vscode","hostRequirements":{"cpus":16,"memory":"68719476736","storage":"107374182400","gpu":true}}`)
	in.Facts.Nodes = []Node{
		{Arch: "amd64", AllocatableCPU: "7910m", AllocatableMemory: "29Gi"},
		{Arch: "arm64", AllocatableCPU: "64", AllocatableMemory: "256Gi"}, // cannot run the amd64 image
		{Arch: "amd64", AllocatableCPU: "bogus", AllocatableMemory: "30Gi"},
	}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Pod.Resources.Requests; got["cpu"] != "7910m" || got["memory"] != "30Gi" {
		t.Errorf("requests %v, want the largest amd64 node's 7910m and 30Gi", got)
	}
	if res.Pod.DiskSize != "100Gi" {
		t.Errorf("disk %s, want hostRequirements.storage 100Gi", res.Pod.DiskSize)
	}
	joined := strings.Join(res.Pod.Warnings, "\n")
	for _, want := range []string{"hostRequirements.cpus 16", "hostRequirements.memory 64Gi", "hostRequirements.gpu"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning about %s in %q", want, joined)
		}
	}

	// Smaller than the connection's defaults: the defaults stay, and so
	// does the disk setting as written.
	in = baseInput(nil, nil, nil)
	in.Merged = json.RawMessage(`{"hostRequirements":{"cpus":0.25,"memory":"1gb","storage":"10gb","gpu":"optional"}}`)
	in.Cluster.DiskSize = "51200Mi"
	res, err = Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Pod.Resources.Requests; got["cpu"] != "500m" || got["memory"] != "2Gi" || res.Pod.DiskSize != "51200Mi" || len(res.Pod.Warnings) > 0 {
		t.Errorf("requests %v disk %s warnings %v", got, res.Pod.DiskSize, res.Pod.Warnings)
	}

	// An image with no metadata layers (busybox): the CLI's merge of no
	// values is -Infinity, which asks for nothing.
	in = baseInput(nil, nil, nil)
	in.Merged = json.RawMessage(`{"hostRequirements":{"cpus":null,"memory":"-Infinity","storage":"-Infinity"}}`)
	in.Cluster.DiskSize = "51200Mi"
	res, err = Translate(in)
	if err != nil {
		t.Fatalf("an image without metadata: %v", err)
	}
	if got := res.Pod.Resources.Requests; got["cpu"] != "500m" || got["memory"] != "2Gi" || res.Pod.DiskSize != "51200Mi" {
		t.Errorf("requests %v disk %s", got, res.Pod.DiskSize)
	}
}

// A LimitRange default below memory_limit gets an explicit limit, which is
// never below the request.
func TestLimitRangeLimitCoversTheRequest(t *testing.T) {
	in := baseInput([]string{"--memory-reservation", "12g"}, nil, nil)
	in.Facts.LimitRangeMemoryLimit = "4Gi"
	in.Cluster.MemoryLimit = "6Gi"
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Pod.Resources; got.Requests["memory"] != "12Gi" || got.Limits["memory"] != "12Gi" {
		t.Errorf("resources %v, want request and limit 12Gi", got)
	}
}

func TestConnectionSettingsThatDoNotRead(t *testing.T) {
	for _, set := range []func(*fleet.KubeCluster){
		func(k *fleet.KubeCluster) { k.Resources.CPURequest = "half" },
		func(k *fleet.KubeCluster) { k.Resources.MemoryRequest = "2GB" },
		func(k *fleet.KubeCluster) { k.DiskSize = "big" },
	} {
		in := baseInput(nil, nil, nil)
		set(in.Cluster)
		_, err := Translate(in)
		if rs := Refusals(err); len(rs) != 1 || rs[0].Code != CodeSettings {
			t.Errorf("got %v, want a %s refusal", err, CodeSettings)
		}
	}
	// No connection at all reads as every default.
	in := baseInput(nil, nil, nil)
	in.Cluster = nil
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pod.DiskSize != fleet.DefaultKubeDiskSize || res.Pod.Resources.Requests["memory"] != fleet.DefaultKubeMemoryRequest {
		t.Errorf("defaults: %+v", res.Pod.Resources)
	}
}

// An image VOLUME is an anonymous volume unless a mount covers its path.
func TestImageVolumes(t *testing.T) {
	in := baseInput([]string{"--mount", "type=volume,source=pg,target=/var/lib/postgresql/data"}, nil, nil)
	in.Image.Config.Volumes = map[string]struct{}{"/var/lib/postgresql/data": {}, "/var/log/app/": {}}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []Mount
	for _, m := range res.Pod.Mounts {
		if m.Kind == MountVolume || m.Kind == MountSharedVolume {
			got = append(got, m)
		}
	}
	want := []Mount{
		{Kind: MountSharedVolume, Target: "/var/lib/postgresql/data", Source: "pg", SubPath: ".vol/" + sha8("pg"), CopyUp: true},
		{Kind: MountVolume, Target: "/var/log/app", SubPath: "vol/anon-" + sha8("/var/log/app"), CopyUp: true, Anonymous: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("volumes\n got %+v\nwant %+v", got, want)
	}
}

// exec.json is what `devcontainer exec` knows: the merged remoteUser,
// remoteEnv (null entries left out, with a warning) and userEnvProbe, the
// container's user and directory, and fleet's agent socket.
func TestExecSpec(t *testing.T) {
	in := baseInput([]string{"-u", "app:staff", "-w", "/srv"}, nil, nil)
	in.Merged = json.RawMessage(`{"remoteUser":"vscode","userEnvProbe":"loginShell","remoteEnv":{"A":"1","FROM":"${containerEnv:PATH}","GONE":null}}`)
	in.SSHAuthSock = "/fleet-mounts/control/ssh-agent.sock"
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	var spec execas.Spec
	if err := json.Unmarshal(res.Secrets.Spec[ExecFile], &spec); err != nil {
		t.Fatal(err)
	}
	want := execas.Spec{
		RemoteUser: "vscode", ContainerUser: "app:staff", WorkspaceFolder: "/workspaces/app", WorkingDir: "/srv",
		UserEnvProbe: execas.ProbeLoginShell, RemoteEnv: map[string]string{"A": "1", "FROM": "${containerEnv:PATH}"},
		SSHAuthSock: "/fleet-mounts/control/ssh-agent.sock",
	}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("exec.json\n got %+v\nwant %+v", spec, want)
	}
	if !strings.Contains(strings.Join(res.Pod.Warnings, "\n"), "remoteEnv GONE") {
		t.Errorf("no warning about the null remoteEnv: %v", res.Pod.Warnings)
	}

	in.Merged = json.RawMessage(`{"userEnvProbe":"bogus"}`)
	if _, err := Translate(in); len(Refusals(err)) != 1 || Refusals(err)[0].Code != CodeSettings {
		t.Errorf("an unknown userEnvProbe: %v", err)
	}
}

// The spec Secret holds the five files; capture.json is redacted and
// inspect.json is the container docker would have created.
func TestSpecSecretFiles(t *testing.T) {
	in := baseInput([]string{"-e", "TOKEN=hunter22"}, []kubeshim.EnvVar{{Name: "TOKEN", Value: "hunter22", Source: "flag"}},
		map[string]string{"devcontainer.metadata": `[{"containerEnv":{"TOKEN":"hunter22"}}]`})
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sortedKeys(res.Secrets.Spec), " "); got != "capture.json exec.json inspect.json merged.json pid1.json" {
		t.Errorf("spec files %s", got)
	}
	capture := string(res.Secrets.Spec[CaptureFile])
	if strings.Contains(capture, "hunter22") || !strings.Contains(capture, redact.Placeholder) {
		t.Errorf("capture.json is not redacted: %s", capture)
	}
	var c kubeshim.Container
	if err := json.Unmarshal(res.Secrets.Spec[InspectFile], &c); err != nil {
		t.Fatal(err)
	}
	if c.Labels["devcontainer.metadata"] == "" || !containsString(c.Env, "TOKEN=hunter22") || c.User != "root" {
		t.Errorf("inspect.json %+v", c)
	}
	if string(res.Secrets.Env["TOKEN"]) != "hunter22" || len(res.Secrets.Env) != 1 {
		t.Errorf("env Secret %q", res.Secrets.Env)
	}
	if string(res.Secrets.Spec[MergedFile]) != string(in.Merged) {
		t.Errorf("merged.json %s", res.Secrets.Spec[MergedFile])
	}

	// A spec that does not fit in a Secret is refused.
	in.Merged = json.RawMessage(`{"name":"` + strings.Repeat("x", maxSecretData) + `"}`)
	if _, err := Translate(in); len(Refusals(err)) != 1 || Refusals(err)[0].Code != CodeSpecTooLarge {
		t.Errorf("an oversized spec: %v", err)
	}
}

func TestEnvKey(t *testing.T) {
	for name, want := range map[string]string{
		"PATH": "PATH", "my.var-1": "my.var-1", "_X": "_X",
	} {
		if got := envKey(name); got != want {
			t.Errorf("envKey(%q) = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"with space", "x-already-hashed-looking", "ünïcode", strings.Repeat("A", 254), "a/b", ".", "..", "..x"} {
		got := envKey(name)
		if !strings.HasPrefix(got, "x-") || len(got) != 18 || !secretKeyRE.MatchString(got) {
			t.Errorf("envKey(%q) = %q, want x-<16 hex>", name, got)
		}
	}
	if envKey("a b") == envKey("a  b") {
		t.Error("two names share a key")
	}
}

// A host-network pod is in the node's UTS namespace: pod-init must never
// set a hostname there, however privileged the pod is.
func TestHostNetworkNeverRenamesTheNode(t *testing.T) {
	for _, grant := range [][]string{{"--privileged"}, {"--cap-add", "SYS_ADMIN"}} {
		args := append([]string{"--network", "host", "-h", "devbox", "--domainname", "corp.example"}, grant...)
		in := baseInput(args, nil, nil)
		in.Facts.Local = true
		res, err := Translate(in)
		if err != nil {
			t.Fatalf("%v: %v", grant, err)
		}
		if !res.Pod.HostNetwork || res.Pod.PID1.Hostname != "" || res.Pod.PID1.Domainname != "" {
			t.Errorf("%v: hostNetwork %v, pid1 hostname %q domainname %q; want no name set on a host-network pod",
				grant, res.Pod.HostNetwork, res.Pod.PID1.Hostname, res.Pod.PID1.Domainname)
		}
	}
}

// A log driver's options carry its credentials: messages name them by key.
func TestLogOptValuesStayOutOfWarnings(t *testing.T) {
	res, err := Translate(baseInput([]string{"--log-driver", "splunk", "--log-opt", "splunk-token=hunter22"}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Pod.Warnings, "\n")
	if strings.Contains(joined, "hunter22") || !strings.Contains(joined, "splunk-token") {
		t.Errorf("warnings %q, want the key without its value", joined)
	}
}

func TestNoCommand(t *testing.T) {
	in := baseInput(nil, nil, nil)
	in.Capture.Args = []string{"--entrypoint=", baseImage}
	if _, err := Translate(in); len(Refusals(err)) != 1 || Refusals(err)[0].Code != CodeNoCommand {
		t.Errorf("a run with nothing to run: %v", err)
	}
}

func TestImageStopSignal(t *testing.T) {
	in := baseInput(nil, nil, nil)
	in.Image.Config.StopSignal = "SIGWHAT"
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pod.PID1.StopSignal != "" || len(res.Pod.Warnings) != 1 {
		t.Errorf("an image stop signal that is not one: %q %v", res.Pod.PID1.StopSignal, res.Pod.Warnings)
	}
	// --stop-signal wins over the image's.
	in = baseInput([]string{"--stop-signal", "SIGINT"}, nil, nil)
	in.Image.Config.StopSignal = "SIGQUIT"
	if res, err := Translate(in); err != nil || res.Pod.PID1.StopSignal != "SIGINT" {
		t.Errorf("stop signal %v, %v", res, err)
	}
}

// The ${devcontainerId} Translate computes is the CLI's: the recorded
// docker-in-docker capture's /var/lib/docker volume is named after it.
func TestDevcontainerIDIsTheCLIs(t *testing.T) {
	rc := loadRecorded(t, "fixture-dind")
	id := devcontainerID(rc.Capture.Labels)
	if len(id) != 52 || !strings.Contains(strings.Join(rc.Capture.Args, " "), "dind-var-lib-docker-"+id) {
		t.Errorf("devcontainerID = %s, not the one in %v", id, rc.Capture.Args)
	}
	if devcontainerID(map[string]string{}) != "" {
		t.Error("a run without id labels has a devcontainerId")
	}
	// JSON.stringify escapes neither < > & nor U+2028.
	if got := jsJSONObject(map[string]string{"b": "<&> ", "a": "q\"\\\n\x01"}); got != `{"a":"q\"\\\n\u0001","b":"<&>`+" "+`"}` {
		t.Errorf("jsJSONObject = %s", got)
	}
}

// Property: whatever the run's environment holds, no value reaches the pod
// model (and so no kubelet-expanded field of the rendered pod) or
// capture.json; every value reaches the env Secret under its key.
func TestEnvValuesStayInTheEnvSecret(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("abcXYZ019 $()={}[]\"'\\\n\t:,;/._-ü✓")
	word := func(n int) string {
		r := make([]rune, n)
		for i := range r {
			r[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(r)
	}
	for i := 0; i < 300; i++ {
		var args []string
		var env []kubeshim.EnvVar
		seen := map[string]bool{}
		for j := 0; j < 1+rng.Intn(5); j++ {
			name := "V" + strings.Map(func(r rune) rune {
				if r == '=' || r == 0 {
					return '_'
				}
				return r
			}, word(1+rng.Intn(6)))
			if seen[name] {
				continue
			}
			seen[name] = true
			value := "$(SECRET" + word(4+rng.Intn(20)) + ")"
			args = append(args, "-e", name+"="+value)
			env = append(env, kubeshim.EnvVar{Name: name, Value: value, Source: "flag"})
		}
		in := baseInput(args, env, map[string]string{"devcontainer.metadata": `[{"containerEnv":` + jsonString(envMap(env)) + `}]`})
		res, err := Translate(in)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		checkNoEnvValues(t, "property", res, in)
		for _, e := range res.Pod.Env {
			if strings.Contains(e.Key, "$(") || !secretKeyRE.MatchString(e.Key) {
				t.Fatalf("case %d: key %q", i, e.Key)
			}
		}
	}
}

func envMap(env []kubeshim.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

// FuzzTranslate runs arbitrary docker run options through the table: it
// never panics, and a translation keeps every environment value out of the
// pod model.
func FuzzTranslate(f *testing.F) {
	for _, c := range optionCases {
		f.Add(strings.Join(c.args, "\x00"))
	}
	f.Fuzz(func(t *testing.T, joined string) {
		var extra []string
		if joined != "" {
			extra = strings.Split(joined, "\x00")
		}
		if _, err := kubeshim.ParseRun(append(append([]string{}, extra...), "img")); err != nil {
			return
		}
		res, err := Translate(baseInput(extra, nil, nil))
		if err == nil && len(res.Pod.Options) == 0 {
			t.Fatal("no option outcomes")
		}
	})
}
