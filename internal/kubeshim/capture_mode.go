package kubeshim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// captureMode serves one call of `devcontainer up` (or `read-configuration`)
// on the daemon host:
//
//   - build, buildx, builder, image, pull, tag, login, version, `-v`, and
//     inspect of an image go to the real docker;
//   - ps lists nothing and inspect of a container finds nothing, so the CLI
//     always creates a fresh container instead of adopting one;
//   - events blocks, reporting nothing, until the shim directory goes;
//   - run and create are captured (ExitCaptured);
//   - compose fails with UnsupportedCompose;
//   - anything else is refused (exit 125).
func (s *shim) captureMode() int {
	switch s.verb {
	case "":
		if _, ok := lastValue(s.globals, "version"); ok {
			return s.pass()
		}
		return s.refuse("no docker command")
	case "build", "buildx", "builder", "image", "pull", "tag", "login", "version":
		return s.pass()
	case "inspect":
		switch inspectType(s.rest) {
		case "image":
			return s.pass()
		case "container":
			return s.missing(inspectOperands(s.rest))
		}
		return s.refuse("inspect without --type image or --type container")
	case "ps":
		s.record(OutcomeEmpty, "")
		return 0
	case "events":
		return s.blockEvents()
	case "run", "create":
		return s.capture(s.verb, s.rest)
	case "compose":
		return s.compose()
	case "container":
		sub, args := subcommand(s.rest)
		switch sub {
		case "inspect":
			return s.missing(inspectOperands(args))
		case "ls", "list", "ps":
			s.record(OutcomeEmpty, "")
			return 0
		case "run", "create":
			return s.capture(sub, args)
		}
	}
	return s.refuse("the devcontainer CLI's build calls are all capture mode serves")
}

// pass hands the call to the real docker, replacing the shim's process so
// the CLI sees docker's own exit code, output and signals.
func (s *shim) pass() int {
	if s.cfg.Docker == "" {
		return s.refuse("no real docker configured")
	}
	s.record(OutcomePass, "")
	argv := append([]string{s.cfg.Docker}, s.args...)
	err := s.inv.exec(s.cfg.Docker, argv, s.inv.environ())
	fmt.Fprintf(s.inv.stderr, "kubeshim: running %s: %v\n", s.cfg.Docker, err)
	return exitShim
}

// missing answers an inspect of containers as an engine without any: an
// empty JSON array, and docker's error for each name.
func (s *shim) missing(names []string) int {
	s.record(OutcomeMissing, "")
	fmt.Fprintln(s.inv.stdout, "[]")
	for _, n := range names {
		fmt.Fprintf(s.inv.stderr, "Error: No such container: %s\n", n)
	}
	return 1
}

// eventsPoll is how often a blocked `docker events` checks whether its shim
// directory is still there.
const eventsPoll = 250 * time.Millisecond

// blockEvents is a `docker events` that never reports an event. The CLI
// watches for its container's start event alongside `docker run`, and kills
// the watcher when the run fails; should it not, the shim ends by itself once
// the shim directory is removed (CaptureUp removes it when the CLI exits) or
// its parent is gone (it was reparented, possibly before it looked: the
// devcontainer CLI is never pid 1).
func (s *shim) blockEvents() int {
	s.record(OutcomeBlock, "")
	poll := s.inv.poll
	if poll <= 0 {
		poll = eventsPoll
	}
	parent := s.inv.getppid()
	if parent == 1 {
		return 0
	}
	for {
		time.Sleep(poll)
		if _, err := os.Stat(filepath.Join(s.dir, ConfigFile)); err != nil {
			return 0
		}
		if s.inv.getppid() != parent {
			return 0
		}
	}
}

// capture records the docker run instead of running it, and fails the call
// with ExitCaptured.
func (s *shim) capture(verb string, args []string) int {
	path := filepath.Join(s.dir, captureFile)
	if _, err := os.Stat(path); err == nil {
		return s.refuse("a docker run was already captured")
	}
	c, err := newCapture(s.globalsArgs(), verb, args, s.inv.cwd, s.inv.getenv)
	if err != nil {
		return s.refuse(err.Error())
	}
	data, err := json.Marshal(c)
	if err != nil {
		return s.refuse(err.Error())
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return s.refuse(fmt.Sprintf("writing the capture: %v", err))
	}
	s.record(OutcomeCaptured, "")
	fmt.Fprintf(s.inv.stderr, "kubeshim: captured docker %s for the Kubernetes backend (exit %d)\n", verb, ExitCaptured)
	return ExitCaptured
}

// inspectFlags are `docker inspect`'s options.
var inspectFlags = newFlagSet([]flagSpec{
	{name: "format", short: "f", value: true},
	{name: "size", short: "s"},
	{name: "type", value: true},
})

// inspectType is the --type an inspect asks for, "" when none (or when its
// options do not parse).
func inspectType(args []string) string {
	flags, _, err := inspectFlags.parseInterspersed(args)
	if err != nil {
		return ""
	}
	t, _ := lastValue(flags, "type")
	return t
}

// inspectOperands are the names an inspect asks about.
func inspectOperands(args []string) []string {
	_, operands, _ := inspectFlags.parseInterspersed(args)
	return operands
}

// subcommand splits `docker container <sub> ...`.
func subcommand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}
