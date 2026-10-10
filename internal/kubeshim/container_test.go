package kubeshim

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestContainerForMergesAsDockerDoes(t *testing.T) {
	var img ImageConfig
	img.ID = "sha256:img"
	img.Config.User = "app"
	img.Config.WorkingDir = "/srv"
	img.Config.Env = []string{"PATH=/usr/bin", "KEEP=1", "DROP=1", "REPLACE=old"}
	img.Config.Entrypoint = []string{"/docker-entrypoint.sh"}
	img.Config.Cmd = []string{"serve"}
	img.Config.Labels = map[string]string{"devcontainer.metadata": "[image]", "image.only": "1"}

	capture := func(args ...string) *Capture {
		return &Capture{Format: CaptureFormat, Verb: "run", Args: args,
			Env: []EnvVar{
				{Name: "REPLACE", Value: "new", Source: "flag"},
				{Name: "ADDED", Value: "a", Source: "flag"},
				{Name: "DROP", Unset: true, Source: "host"},
			},
			Labels: map[string]string{"devcontainer.metadata": "[run]", "run.only": "1"},
		}
	}

	c, err := ContainerFor(capture("-u", "vscode", "-w", "/workspaces/x", "-h", "box", "--entrypoint", "/bin/sh", "img", "-c", "echo"), img)
	if err != nil {
		t.Fatal(err)
	}
	if c.Image != "img" || c.ImageID != "sha256:img" || c.User != "vscode" || c.WorkingDir != "/workspaces/x" || c.Hostname != "box" {
		t.Errorf("container %+v", c)
	}
	if want := []string{"PATH=/usr/bin", "KEEP=1", "REPLACE=new", "ADDED=a"}; !reflect.DeepEqual(c.Env, want) {
		t.Errorf("env %q, want %q", c.Env, want)
	}
	if want := map[string]string{"devcontainer.metadata": "[run]", "image.only": "1", "run.only": "1"}; !reflect.DeepEqual(c.Labels, want) {
		t.Errorf("labels %v", c.Labels)
	}
	if !reflect.DeepEqual(c.Entrypoint, []string{"/bin/sh"}) || !reflect.DeepEqual(c.Cmd, []string{"-c", "echo"}) {
		t.Errorf("--entrypoint resets the image's: entrypoint %q cmd %q", c.Entrypoint, c.Cmd)
	}

	// No -u, -w or --entrypoint: the image's, and its command when the run
	// has none.
	c, err = ContainerFor(capture("img"), img)
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "app" || c.WorkingDir != "/srv" || !reflect.DeepEqual(c.Entrypoint, img.Config.Entrypoint) || !reflect.DeepEqual(c.Cmd, img.Config.Cmd) {
		t.Errorf("image defaults: %+v", c)
	}
	// --entrypoint with no command drops the image's command.
	if c, _ := ContainerFor(capture("--entrypoint", "/bin/sh", "img"), img); len(c.Cmd) != 0 {
		t.Errorf("cmd %q, want none", c.Cmd)
	}
}

func TestMergeEnv(t *testing.T) {
	for _, tc := range []struct {
		defaults, overrides, want []string
	}{
		{nil, nil, nil},
		{[]string{"A=1"}, []string{"B=2"}, []string{"A=1", "B=2"}},
		{[]string{"A=1", "B=1"}, []string{"A=2"}, []string{"A=2", "B=1"}},
		{[]string{"A=1", "B=1"}, []string{"A"}, []string{"B=1"}},
		{[]string{"A=1"}, []string{"A", "A=3"}, []string{"A=3"}},
		{[]string{"A=1"}, []string{"Z"}, []string{"A=1"}},
		{[]string{"A=1"}, []string{"B=1", "B"}, []string{"A=1"}},
	} {
		if got := mergeEnv(tc.defaults, tc.overrides); !reflect.DeepEqual(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
			t.Errorf("mergeEnv(%q, %q) = %q, want %q", tc.defaults, tc.overrides, got, tc.want)
		}
	}
}

func TestStartedAt(t *testing.T) {
	start := time.Date(2026, 10, 2, 8, 30, 15, 0, time.UTC)
	a := StartedAt(start, "uid-1", 0)
	if !a.Equal(StartedAt(start, "uid-1", 0)) {
		t.Error("the same run must read the same")
	}
	if a.Truncate(time.Second) != start {
		t.Errorf("%v must stay in the kubelet's second", a)
	}
	seen := map[string]bool{a.Format(time.RFC3339Nano): true}
	for _, other := range []time.Time{StartedAt(start, "uid-1", 1), StartedAt(start, "uid-2", 0), StartedAt(start, "uid-2", 1)} {
		s := other.Format(time.RFC3339Nano)
		if seen[s] {
			t.Errorf("two runs read %s", s)
		}
		seen[s] = true
	}
	local := time.Date(2026, 10, 2, 10, 30, 15, 500, time.FixedZone("x", 2*3600))
	if got := StartedAt(local, "uid-1", 0); got.Location() != time.UTC || !got.Truncate(time.Second).Equal(start) {
		t.Errorf("StartedAt of a zoned time = %v", got)
	}
}

func TestContainerMatchesAndRemoteUser(t *testing.T) {
	c := testContainer()
	for ref, want := range map[string]bool{
		c.ContainerID:     true,
		c.ID():            true,
		c.ID()[:12]:       true,
		c.ID()[:11]:       false,
		c.Name:            true,
		"/" + c.Name:      true,
		"":                false,
		"fi-other-0":      false,
		c.ContainerID[:5]: false,
	} {
		if got := c.Matches(ref); got != want {
			t.Errorf("Matches(%q) = %v", ref, got)
		}
	}
	if c.RemoteUser() != "vscode" {
		t.Errorf("RemoteUser = %q, want the metadata's", c.RemoteUser())
	}
	c.Labels = map[string]string{"devcontainer.metadata": `[{"remoteUser":"${localEnv:USER}"}]`}
	if c.RemoteUser() != "root" {
		t.Errorf("an unresolved remoteUser falls back to the container's user, got %q", c.RemoteUser())
	}
	c.User = ""
	if c.RemoteUser() != "root" {
		t.Errorf("no user at all is root, got %q", c.RemoteUser())
	}
}

func TestNewDir(t *testing.T) {
	d, err := NewDir("/usr/bin/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove()
	for _, link := range []string{d.Docker(), d.Compose()} {
		target, err := os.Readlink(link)
		if err != nil || target != "/usr/bin/fleet" {
			t.Errorf("%s -> %q %v", link, target, err)
		}
	}
	info, err := os.Stat(d.Path())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v %v", info.Mode(), err)
	}
	if len(d.ExecSocket()) > maxSocketPath {
		t.Errorf("exec socket path is %d bytes", len(d.ExecSocket()))
	}
	if !strings.HasPrefix(filepath.Base(d.Path()), "fleet-kubeshim-") {
		t.Errorf("dir %s", d.Path())
	}
	if _, err := NewDir("fleet"); err == nil {
		t.Error("a relative executable must be refused")
	}
	for _, bad := range []Config{
		{Mode: "other"},
		{Mode: ModeCapture, Docker: "docker"},
		{Mode: ModePod},
		{Mode: ModePod, Pod: &PodConfig{ExecSocket: "/x"}},
	} {
		if err := d.Configure(bad); err == nil {
			t.Errorf("Configure(%+v) accepted", bad)
		}
	}
	if err := d.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path()); !os.IsNotExist(err) {
		t.Errorf("Remove left %s", d.Path())
	}
}

func TestNewDirUnderADeepTempDir(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deep)
	d, err := NewDir("/usr/bin/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove()
	if !strings.HasPrefix(d.Path(), "/tmp/") || len(d.ExecSocket()) > maxSocketPath {
		t.Errorf("a deep TMPDIR should fall back to /tmp, got %s", d.Path())
	}
}
