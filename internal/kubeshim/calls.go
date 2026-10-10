package kubeshim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/redact"
)

// Outcome is what the shim did with one docker call.
type Outcome string

const (
	// OutcomePass: handed to the real docker (capture mode).
	OutcomePass Outcome = "pass"
	// OutcomeEmpty: answered as an engine with no containers (ps).
	OutcomeEmpty Outcome = "empty"
	// OutcomeMissing: answered "no such container" (inspect).
	OutcomeMissing Outcome = "missing"
	// OutcomeBlock: an events stream that never reports anything.
	OutcomeBlock Outcome = "block"
	// OutcomeCaptured: the docker run, recorded instead of run (exit 97).
	OutcomeCaptured Outcome = "captured"
	// OutcomeInspect: answered from the pod's Container (pod mode).
	OutcomeInspect Outcome = "inspect"
	// OutcomeExec: run in the pod through the exec socket (pod mode).
	OutcomeExec Outcome = "exec"
	// OutcomeNoEngine: a docker engine query pod mode has no engine for.
	OutcomeNoEngine Outcome = "no-engine"
	// OutcomeCompose: a compose call, refused (UnsupportedCompose).
	OutcomeCompose Outcome = "compose"
	// OutcomeRefused: anything else, refused (exit 125).
	OutcomeRefused Outcome = "refused"
)

// Call is one entry of a shim directory's journal.
type Call struct {
	Mode Mode `json:"mode"`
	// Binary is "docker" or "docker-compose".
	Binary string `json:"binary"`
	// Args is the call's argv after the binary, environment values redacted.
	Args    []string `json:"args"`
	Outcome Outcome  `json:"outcome"`
	// Detail says why a call was refused.
	Detail string `json:"detail,omitempty"`
}

// Verb is the call's signature for a reader: the verb with its subcommand
// ("buildx build", "image inspect"), "inspect --type <t>" for inspect, and
// "-v" for the version flag. Options and operands are left out.
func (c Call) Verb() string {
	if c.Binary == composeLink {
		return composeLink
	}
	globals, verb, rest, err := splitGlobals(c.Args)
	if err != nil {
		return "?"
	}
	if verb == "" {
		if _, ok := lastValue(globals, "version"); ok {
			return "-v"
		}
		return ""
	}
	switch verb {
	case "buildx", "image", "container", "compose", "builder":
		if sub := firstOperand(rest); sub != "" {
			return verb + " " + sub
		}
	case "inspect":
		if t := inspectType(rest); t != "" {
			return "inspect --type " + t
		}
	}
	return verb
}

// firstOperand is the first argument of args that is not an option.
func firstOperand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// appendCall adds c to the journal at path. Each entry is one write to a file
// opened for appending, so concurrent shims (the CLI runs `docker events`
// beside its other calls) never interleave within an entry.
func appendCall(path string, c Call) {
	c.Args = redactArgv(c.Args)
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// redactArgv returns a copy of a docker argv (after the binary) with its
// environment values hidden: the -e options of run, create and exec read with
// their grammar (flagSet.redactEnv), and redact.EnvArgs over the whole argv
// for the rest and for the devcontainer.metadata label's env objects.
func redactArgv(args []string) []string {
	out := append([]string(nil), args...)
	_, verb, rest, err := splitGlobals(args)
	if err == nil && verb == "container" {
		verb, rest = subcommand(rest)
	}
	if err == nil {
		var fs *flagSet
		switch verb {
		case "run", "create":
			fs = runFlags
		case "exec":
			fs = execFlags
		}
		if fs != nil {
			copy(out[len(out)-len(rest):], fs.redactEnv(rest))
		}
	}
	return redact.EnvArgs(out)
}

// readCalls reads the journal at path; a missing journal is an empty one.
func readCalls(path string) ([]Call, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var calls []Call
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for scanner.Scan() {
		var c Call
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		calls = append(calls, c)
	}
	return calls, scanner.Err()
}
