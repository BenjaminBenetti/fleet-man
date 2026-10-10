package kubeshim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
)

// Container is what the pod-mode shim answers `docker inspect` with: the
// container docker would have created from the captured run, placed in the
// instance's pod. `devcontainer set-up` reads its labels (devcontainer.metadata
// holds the lifecycle commands and remoteUser), its environment
// (${containerEnv:...}), its user, and Created and StartedAt, which decide
// whether the marker-gated hooks run: onCreate, updateContent and postCreate
// run when Created differs from the marker they left, postStart when StartedAt
// does.
type Container struct {
	// ContainerID is the id fleet records for the instance and passes to
	// set-up as --container-id. Inspect reports sha256(ContainerID) as Id.
	ContainerID string `json:"containerId"`
	// Name is the pod's name (docker reports it as "/<name>").
	Name string `json:"name,omitempty"`
	// Image is the image reference the pod runs, ImageID its id.
	Image   string `json:"image"`
	ImageID string `json:"imageId,omitempty"`
	// Created is when the instance's container was created: the
	// container-created annotation, which survives stop and start.
	Created time.Time `json:"created"`
	// StartedAt is when the current run of the dev container started; see
	// StartedAt.
	StartedAt  time.Time         `json:"startedAt"`
	Hostname   string            `json:"hostname,omitempty"`
	User       string            `json:"user,omitempty"`
	Env        []string          `json:"env,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
}

// ID is the docker-style container id inspect reports: the hex sha256 of
// ContainerID, 64 characters like a docker id.
func (c Container) ID() string {
	sum := sha256.Sum256([]byte(c.ContainerID))
	return hex.EncodeToString(sum[:])
}

// RemoteUser is the user `devcontainer exec` and the lifecycle hooks run as,
// decided as the CLI decides it: the devcontainer.metadata label's remoteUser
// (devmeta.RemoteUser), else the container's user, else root.
func (c Container) RemoteUser() string {
	if u := devmeta.RemoteUser(c.Labels[devmeta.MetadataLabel]); devmeta.LooksLikeUsername(u) {
		return u
	}
	if c.User != "" {
		return c.User
	}
	return "root"
}

// minIDPrefix is the shortest id prefix the shim takes for the container;
// docker prints ids at 12 characters.
const minIDPrefix = 12

// Matches reports whether ref names c the way the devcontainer CLI may name
// it: the ContainerID it was given, the Id inspect returned (or a prefix of at
// least 12 characters), or the name.
func (c Container) Matches(ref string) bool {
	if ref == "" {
		return false
	}
	if ref == c.ContainerID {
		return true
	}
	id := c.ID()
	if len(ref) >= minIDPrefix && strings.HasPrefix(id, ref) {
		return true
	}
	return c.Name != "" && strings.TrimPrefix(ref, "/") == c.Name
}

// ContainerFor builds the Container docker would create from the captured
// run c of image img (ContainerID, Name, Created and StartedAt are left for
// the caller, who knows the pod). It merges as docker's engine does: the
// image's labels, then the run's; the image's environment with the run's
// entries replacing, adding or (bare names) removing variables; -u, -w and -h
// over the image's user and working directory; and --entrypoint resetting the
// image's command.
func ContainerFor(c *Capture, img ImageConfig) (Container, error) {
	run, err := c.Run()
	if err != nil {
		return Container{}, err
	}
	out := Container{
		Image:      run.Image,
		ImageID:    img.ID,
		User:       img.Config.User,
		WorkingDir: img.Config.WorkingDir,
		Env:        mergeEnv(img.Config.Env, c.EnvList()),
		Labels:     map[string]string{},
	}
	for k, v := range img.Config.Labels {
		out.Labels[k] = v
	}
	for k, v := range c.Labels {
		out.Labels[k] = v
	}
	if v, ok := run.Value("user"); ok {
		out.User = v
	}
	if v, ok := run.Value("workdir"); ok {
		out.WorkingDir = v
	}
	if v, ok := run.Value("hostname"); ok {
		out.Hostname = v
	}
	if ep, ok := run.Value("entrypoint"); ok {
		out.Entrypoint = []string{ep}
		out.Cmd = append([]string(nil), run.Cmd...)
	} else {
		out.Entrypoint = append([]string(nil), img.Config.Entrypoint...)
		out.Cmd = append([]string(nil), run.Cmd...)
		if len(out.Cmd) == 0 {
			out.Cmd = append([]string(nil), img.Config.Cmd...)
		}
	}
	return out, nil
}

// mergeEnv applies overrides to defaults as the docker engine does
// (ReplaceOrAppendEnvValues): NAME=VALUE replaces NAME in place or is
// appended, a bare NAME removes it.
func mergeEnv(defaults, overrides []string) []string {
	out := append([]string(nil), defaults...)
	index := make(map[string]int, len(out))
	for i, e := range out {
		name, _, _ := strings.Cut(e, "=")
		index[name] = i
	}
	removed := map[int]bool{}
	for _, e := range overrides {
		name, _, hasValue := strings.Cut(e, "=")
		i, exists := index[name]
		switch {
		case !hasValue:
			if exists {
				removed[i] = true
			}
		case exists:
			out[i] = e
			delete(removed, i)
		default:
			index[name] = len(out)
			out = append(out, e)
		}
	}
	if len(removed) == 0 {
		return out
	}
	kept := out[:0]
	for i, e := range out {
		if !removed[i] {
			kept = append(kept, e)
		}
	}
	return kept
}

// StartedAt is the docker-style start time of one run of a pod's dev
// container. The kubelet reports a container's startedAt to the second;
// docker's carries nanoseconds, and set-up compares it with the postStart
// marker as a string. So the nanoseconds are derived from the pod UID and the
// container's restart count: each run (a new pod on start, or a container
// restart inside one) reads differently even within the same second, so
// postStart runs once per start, and the same run always reads the same, so a
// repeated set-up of one run does not run it again.
func StartedAt(started time.Time, podUID string, restartCount int32) time.Time {
	sum := sha256.Sum256([]byte(podUID + "/" + strconv.FormatInt(int64(restartCount), 10)))
	ns := binary.BigEndian.Uint32(sum[:4]) % uint32(time.Second)
	return started.UTC().Truncate(time.Second).Add(time.Duration(ns))
}

// dockerTime formats t as docker's inspect does (RFC 3339, nanoseconds, UTC).
// The zero time is docker's "0001-01-01T00:00:00Z".
func dockerTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// inspectDoc is one element of `docker inspect`'s JSON array, as far as the
// shim fills it in: the fields the devcontainer CLI reads (Id, Created,
// State.StartedAt, State.Status, Config.User, Config.Env, Config.Labels,
// NetworkSettings.Ports, which it walks unconditionally) and the ones a reader
// of the output would expect beside them.
type inspectDoc struct {
	ID              string            `json:"Id"`
	Created         string            `json:"Created"`
	Path            string            `json:"Path"`
	Args            []string          `json:"Args"`
	State           inspectState      `json:"State"`
	Image           string            `json:"Image"`
	Name            string            `json:"Name"`
	RestartCount    int               `json:"RestartCount"`
	Platform        string            `json:"Platform"`
	Mounts          []struct{}        `json:"Mounts"`
	Config          inspectConfig     `json:"Config"`
	NetworkSettings inspectNetworking `json:"NetworkSettings"`
}

type inspectState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

type inspectConfig struct {
	Hostname   string            `json:"Hostname"`
	User       string            `json:"User"`
	Env        []string          `json:"Env"`
	Cmd        []string          `json:"Cmd"`
	Image      string            `json:"Image"`
	WorkingDir string            `json:"WorkingDir"`
	Entrypoint []string          `json:"Entrypoint"`
	Labels     map[string]string `json:"Labels"`
}

type inspectNetworking struct {
	Ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	} `json:"Ports"`
}

// inspect renders c as docker inspect would show a running container.
func (c Container) inspect() inspectDoc {
	argv := append(append([]string(nil), c.Entrypoint...), c.Cmd...)
	doc := inspectDoc{
		ID:      c.ID(),
		Created: dockerTime(c.Created),
		Args:    []string{},
		State: inspectState{
			Status:     "running",
			Running:    true,
			StartedAt:  dockerTime(c.StartedAt),
			FinishedAt: dockerTime(time.Time{}),
		},
		Image:    c.ImageID,
		Name:     "/" + c.Name,
		Platform: "linux",
		Mounts:   []struct{}{},
		Config: inspectConfig{
			Hostname:   c.Hostname,
			User:       c.User,
			Env:        nonNil(c.Env),
			Cmd:        c.Cmd,
			Image:      c.Image,
			WorkingDir: c.WorkingDir,
			Entrypoint: c.Entrypoint,
			Labels:     c.Labels,
		},
	}
	doc.NetworkSettings.Ports = map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}{}
	if doc.Config.Labels == nil {
		doc.Config.Labels = map[string]string{}
	}
	if doc.Image == "" {
		doc.Image = c.Image
	}
	if len(argv) > 0 {
		doc.Path = argv[0]
		doc.Args = argv[1:]
	}
	return doc
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
