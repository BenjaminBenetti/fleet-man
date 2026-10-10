package kubeshim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/redact"
)

// CaptureFormat is the version of the Capture JSON layout.
const CaptureFormat = 1

// Capture is the `docker run` the devcontainer CLI asked the capture-mode shim
// for, with what docker would have read on the daemon host for it: the env
// files and host variables the container environment draws on, and the label
// files. It holds every value as given, secrets included. Redacted is the copy
// that may be stored or shown.
type Capture struct {
	// Format is CaptureFormat.
	Format int `json:"format"`
	// Verb is "run" or "create".
	Verb string `json:"verb"`
	// Globals are the docker options before the verb (--context, -H, ...).
	Globals []string `json:"globals,omitempty"`
	// Args is the argv after the verb, as the CLI passed it.
	Args []string `json:"args"`
	// Env is the container environment the run sets, resolved as docker
	// resolves it: each --env-file in order, then each -e, later entries
	// winning over earlier ones of the same name.
	Env []EnvVar `json:"env"`
	// Labels are the labels the run sets: each --label-file, then each -l.
	// They do not include the image's labels (see ContainerFor).
	Labels map[string]string `json:"labels"`
}

// EnvVar is one container environment entry a run sets.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Unset marks a bare name ("-e NAME", or a name-only env-file line) whose
	// variable is not set on the daemon host: docker then removes NAME from the
	// image's environment instead of setting it.
	Unset bool `json:"unset,omitempty"`
	// Source says where the entry came from: "flag" (-e NAME=VALUE), "host"
	// (a bare name whose value docker took from its own environment), or
	// "file:<path>" (an --env-file line).
	Source string `json:"source"`
}

// Run parses the captured argv.
func (c *Capture) Run() (*RunArgs, error) {
	return ParseRun(c.Args)
}

// Redacted returns a copy of c that can be stored or logged: every
// environment value hidden (redact.Placeholder), in Env, in the -e options of
// Args (redact.EnvArgs) and in the containerEnv and remoteEnv objects of the
// devcontainer.metadata label (redact.EnvObjects), which repeat them. Names
// stay, so a reader still sees what was set.
func (c *Capture) Redacted() *Capture {
	out := *c
	out.Globals = append([]string(nil), c.Globals...)
	out.Args = redact.EnvArgs(c.Args)
	out.Env = make([]EnvVar, len(c.Env))
	for i, e := range c.Env {
		if !e.Unset {
			e.Value = redact.Placeholder
		}
		out.Env[i] = e
	}
	out.Labels = make(map[string]string, len(c.Labels))
	for k, v := range c.Labels {
		out.Labels[k] = redact.EnvObjects(v)
	}
	return &out
}

// EnvList returns the environment as docker passes it to the engine:
// "NAME=VALUE", or a bare "NAME" for an Unset entry.
func (c *Capture) EnvList() []string {
	out := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		if e.Unset {
			out = append(out, e.Name)
			continue
		}
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

// newCapture builds the Capture of `docker <globals> <verb> <args>` run in
// dir cwd, reading the env and label files the run names and the host
// variables it draws on (getenv), as docker would.
func newCapture(globals []string, verb string, args []string, cwd string, getenv func(string) (string, bool)) (*Capture, error) {
	run, err := ParseRun(args)
	if err != nil {
		return nil, runParseError(err)
	}
	c := &Capture{Format: CaptureFormat, Verb: verb, Globals: globals, Args: args, Labels: map[string]string{}}

	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cwd, p)
	}
	for _, f := range run.Values("env-file") {
		entries, err := readKVFile(abs(f), getenv)
		if err != nil {
			return nil, fmt.Errorf("reading --env-file: %w", err)
		}
		for _, e := range entries {
			c.addEnv(EnvVar{Name: e.name, Value: e.value, Unset: !e.set, Source: "file:" + f})
		}
	}
	for _, v := range run.Values("env") {
		name, value, hasValue := strings.Cut(v, "=")
		if name == "" {
			return nil, fmt.Errorf("invalid environment variable: %s", v)
		}
		if hasValue {
			c.addEnv(EnvVar{Name: name, Value: value, Source: "flag"})
			continue
		}
		hostValue, ok := getenv(name)
		c.addEnv(EnvVar{Name: name, Value: hostValue, Unset: !ok, Source: "host"})
	}

	for _, f := range run.Values("label-file") {
		entries, err := readKVFile(abs(f), func(string) (string, bool) { return "", true })
		if err != nil {
			return nil, fmt.Errorf("reading --label-file: %w", err)
		}
		for _, e := range entries {
			c.Labels[e.name] = e.value
		}
	}
	for _, v := range run.Values("label") {
		name, value, _ := strings.Cut(v, "=")
		c.Labels[name] = value
	}
	return c, nil
}

// addEnv adds e, replacing an earlier entry of the same name in place.
func (c *Capture) addEnv(e EnvVar) {
	for i := range c.Env {
		if c.Env[i].Name == e.Name {
			c.Env[i] = e
			return
		}
	}
	c.Env = append(c.Env, e)
}

// ErrNoCapture is a shim directory where no docker run was captured.
var ErrNoCapture = errors.New("the devcontainer CLI never reached docker run")

// readCapture loads the Capture a capture-mode shim wrote at path.
func readCapture(path string) (*Capture, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCapture
	}
	if err != nil {
		return nil, err
	}
	var c Capture
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if c.Format != CaptureFormat {
		return nil, fmt.Errorf("%s has capture format %d, want %d", path, c.Format, CaptureFormat)
	}
	return &c, nil
}
