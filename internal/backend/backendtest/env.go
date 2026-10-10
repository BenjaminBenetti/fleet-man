package backendtest

import (
	"fmt"
	"os"
	"strings"
)

// EnvVar selects the real backends the conformance suite runs against.
const EnvVar = "FLEET_CONFORMANCE"

// Backends the variable may name. kubernetes takes a qualifier (the kube
// context; none means the current one); devcontainer takes none.
const (
	BackendDevcontainer = "devcontainer"
	BackendKubernetes   = "kubernetes"
)

// Selection is one FLEET_CONFORMANCE entry: a backend, and for kubernetes the
// kube context after the colon.
type Selection struct {
	Backend   string
	Qualifier string
}

// String is the entry as FLEET_CONFORMANCE spells it, which is also its
// subtest name.
func (s Selection) String() string {
	if s.Qualifier == "" {
		return s.Backend
	}
	return s.Backend + ":" + s.Qualifier
}

// ParseSelections parses a FLEET_CONFORMANCE value: comma-separated
// backend[:qualifier] entries, blanks around entries ignored. An unknown
// backend, a qualifier on a backend that takes none, an empty entry or a
// repeated one is an error, so a typo fails the run instead of silently
// selecting nothing.
func ParseSelections(value string) ([]Selection, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var out []Selection
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("%s=%q: empty entry", EnvVar, value)
		}
		name, qualifier, hasQualifier := strings.Cut(entry, ":")
		sel := Selection{Backend: name, Qualifier: qualifier}
		switch name {
		case BackendDevcontainer:
			if hasQualifier {
				return nil, fmt.Errorf("%s: %q: devcontainer takes no qualifier", EnvVar, entry)
			}
		case BackendKubernetes:
			if hasQualifier && qualifier == "" {
				return nil, fmt.Errorf("%s: %q: empty kube context after the colon", EnvVar, entry)
			}
		default:
			return nil, fmt.Errorf("%s: %q: unknown backend %q (valid: %s, %s[:<kube context>])", EnvVar, entry, name, BackendDevcontainer, BackendKubernetes)
		}
		if seen[sel.String()] {
			return nil, fmt.Errorf("%s: %q listed twice", EnvVar, entry)
		}
		seen[sel.String()] = true
		out = append(out, sel)
	}
	return out, nil
}

// Selected parses FLEET_CONFORMANCE from the environment.
func Selected() ([]Selection, error) {
	return ParseSelections(os.Getenv(EnvVar))
}
