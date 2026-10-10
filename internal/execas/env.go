package execas

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// DefaultPath is the PATH docker gives a container whose image sets none.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// envList is an environment that keeps its order: setting a variable that is
// already there replaces it in place, as docker merges -e over the container
// environment, and a new one goes at the end.
type envList struct {
	names  []string
	values map[string]string
}

// newEnvList takes "NAME=value" entries; one without '=' or a name is dropped,
// and a later duplicate wins.
func newEnvList(entries []string) *envList {
	e := &envList{values: make(map[string]string, len(entries))}
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			e.set(name, value)
		}
	}
	return e
}

func (e *envList) clone() *envList {
	return &envList{names: slices.Clone(e.names), values: maps.Clone(e.values)}
}

func (e *envList) get(name string) (string, bool) {
	v, ok := e.values[name]
	return v, ok
}

func (e *envList) set(name, value string) {
	if _, ok := e.values[name]; !ok {
		e.names = append(e.names, name)
	}
	e.values[name] = value
}

func (e *envList) unset(name string) {
	if _, ok := e.values[name]; !ok {
		return
	}
	delete(e.values, name)
	e.names = slices.DeleteFunc(e.names, func(n string) bool { return n == name })
}

// setDefault sets name only when it is not set.
func (e *envList) setDefault(name, value string) {
	if _, ok := e.values[name]; !ok {
		e.set(name, value)
	}
}

// merge sets every variable of vars, in name order so the result does not
// depend on map iteration.
func (e *envList) merge(vars map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		e.set(name, vars[name])
	}
}

func (e *envList) entries() []string {
	out := make([]string, 0, len(e.names))
	for _, name := range e.names {
		out = append(out, name+"="+e.values[name])
	}
	return out
}

// containerEnv is the container's own environment, as `docker exec` starts
// from it, taken from the environment this process got from the container
// runtime. The runtime adds one variable the container does not define: HOME,
// set to the exec user's home when the container leaves it unset (runc does
// this for docker and for CRI exec alike). That HOME is this process's own
// (root's, as the agent runs), so it is dropped when it is exactly selfHome and
// derived per user later; a HOME the image or pod set to anything else stays,
// as docker keeps it.
func containerEnv(processEnv []string, selfHome string) *envList {
	env := newEnvList(processEnv)
	if home, ok := env.get("HOME"); ok && home == selfHome {
		env.unset("HOME")
	}
	return env
}

// variableRef is the CLI's ${...} pattern, within which ${containerEnv:...}
// references are matched.
var variableRef = regexp.MustCompile(`\$\{(.*?)\}`)

// substituteContainerEnv resolves ${containerEnv:NAME} and
// ${containerEnv:NAME:default} in value against env, exactly as the CLI's
// containerSubstitute does at exec time: an unset variable is its default, else
// empty; any other ${...} is left as written. A ${containerEnv} naming no
// variable is the CLI's error too.
func substituteContainerEnv(value string, env *envList) (string, error) {
	var err error
	out := variableRef.ReplaceAllStringFunc(value, func(match string) string {
		inner := match[2 : len(match)-1]
		kind, rest, hasArgs := strings.Cut(inner, ":")
		if kind != "containerEnv" {
			return match
		}
		if !hasArgs {
			err = fmt.Errorf("%s can not be resolved because no environment variable name is given", match)
			return match
		}
		args := strings.Split(rest, ":")
		if v, ok := env.get(args[0]); ok {
			return v
		}
		if len(args) > 1 {
			return args[1]
		}
		return ""
	})
	return out, err
}
