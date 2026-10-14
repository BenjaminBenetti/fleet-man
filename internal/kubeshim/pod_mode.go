package kubeshim

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// podMode serves one call of `devcontainer set-up` for an instance's pod:
//
//   - inspect (of a container, or untyped) answers with the pod's Container;
//   - exec runs in the pod through the exec socket;
//   - `-v`, version and `buildx version`, which the CLI probes at startup
//     and tolerates failing, fail: there is no docker engine behind a pod;
//   - compose fails with UnsupportedCompose;
//   - anything else is refused (exit 125).
func (s *shim) podMode() int {
	switch s.verb {
	case "":
		if _, ok := lastValue(s.globals, "version"); ok {
			return s.noEngine()
		}
		return s.refuse("no docker command")
	case "version", "buildx":
		return s.noEngine()
	case "inspect":
		return s.inspect(s.rest, "")
	case "exec":
		return s.exec(s.rest)
	case "compose":
		return s.compose()
	case "container":
		sub, args := subcommand(s.rest)
		switch sub {
		case "inspect":
			return s.inspect(args, "container")
		case "exec":
			return s.exec(args)
		}
	}
	return s.refuse("devcontainer set-up's inspect and exec are all pod mode serves")
}

// noEngine fails a docker engine query.
func (s *shim) noEngine() int {
	s.record(OutcomeNoEngine, "")
	fmt.Fprintln(s.inv.stderr, "kubeshim: no docker engine behind a Kubernetes pod")
	return 1
}

// inspect answers `docker inspect` from the pod's Container. kind is the
// object type the command implies ("container" for `container inspect`).
func (s *shim) inspect(args []string, kind string) int {
	flags, names, err := inspectFlags.parseInterspersed(args)
	if err != nil {
		return s.refuse(err.Error())
	}
	if _, ok := lastValue(flags, "format"); ok {
		return s.refuse("inspect --format is not served; the devcontainer CLI reads the JSON")
	}
	if t, ok := lastValue(flags, "type"); ok {
		kind = t
	}
	if len(names) == 0 {
		return s.refuse("inspect without a name")
	}
	c := s.cfg.Pod.Container
	var docs []inspectDoc
	var missing []string
	for _, n := range names {
		if (kind == "" || kind == "container") && c.Matches(n) {
			docs = append(docs, c.inspect())
			continue
		}
		missing = append(missing, n)
	}
	s.record(OutcomeInspect, "")
	if docs == nil {
		docs = []inspectDoc{}
	}
	out, err := json.MarshalIndent(docs, "", "    ")
	if err != nil {
		return s.refuse(err.Error())
	}
	fmt.Fprintln(s.inv.stdout, string(out))
	if len(missing) == 0 {
		return 0
	}
	what := "object"
	switch kind {
	case "container", "image":
		what = kind
	}
	for _, n := range missing {
		fmt.Fprintf(s.inv.stderr, "Error: No such %s: %s\n", what, n)
	}
	return 1
}

// execFlags are `docker exec`'s options.
var execFlags = newFlagSet([]flagSpec{
	{name: "detach", short: "d"},
	{name: "detach-keys", value: true},
	{name: "env", short: "e", value: true},
	{name: "env-file", value: true},
	{name: "interactive", short: "i"},
	{name: "privileged"},
	{name: "tty", short: "t"},
	{name: "user", short: "u", value: true},
	{name: "workdir", short: "w", value: true},
})

// exec runs `docker exec [options] <container> <argv...>` in the pod.
func (s *shim) exec(args []string) int {
	flags, rest, err := execFlags.parse(args)
	if err != nil {
		return s.refuse(err.Error())
	}
	for _, f := range flags {
		switch f.Name {
		case "detach", "privileged":
			if f.Value == "true" {
				return s.refuse("exec --" + f.Name + " is not served in a pod")
			}
		case "detach-keys":
			return s.refuse("exec --detach-keys is not served in a pod")
		}
	}
	if len(rest) == 0 {
		return s.refuse("exec without a container")
	}
	target, argv := rest[0], rest[1:]
	if !s.cfg.Pod.Container.Matches(target) {
		s.record(OutcomeMissing, "")
		fmt.Fprintf(s.inv.stderr, "Error response from daemon: No such container: %s\n", target)
		return 1
	}
	if len(argv) == 0 {
		return s.refuse("exec without a command")
	}

	req := ExecRequest{Argv: argv}
	req.User, _ = lastValue(flags, "user")
	req.Dir, _ = lastValue(flags, "workdir")
	req.Stdin = boolFlag(flags, "interactive")
	req.TTY = boolFlag(flags, "tty")
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(s.inv.cwd, p)
	}
	for _, f := range values(flags, "env-file") {
		entries, err := readKVFile(abs(f), s.inv.getenv)
		if err != nil {
			return s.refuse(fmt.Sprintf("reading --env-file: %v", err))
		}
		for _, e := range entries {
			if e.set {
				req.Env = append(req.Env, e.name+"="+e.value)
			}
		}
	}
	for _, v := range values(flags, "env") {
		name, _, hasValue := strings.Cut(v, "=")
		if name == "" {
			return s.refuse("invalid environment variable: " + v)
		}
		if hasValue {
			req.Env = append(req.Env, v)
		} else if value, ok := s.inv.getenv(name); ok {
			req.Env = append(req.Env, name+"="+value)
		}
	}
	s.record(OutcomeExec, "")
	return runExecClient(s.inv, s.cfg.Pod.ExecSocket, req)
}

// boolFlag is a boolean option's last value (false when not given).
func boolFlag(flags []Flag, name string) bool {
	v, _ := lastValue(flags, name)
	return v == "true"
}
