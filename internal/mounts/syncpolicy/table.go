package syncpolicy

import (
	"path"
	"strings"
)

// Root is one folder sharesync keeps converged as a unit, with the rules for
// what is in it.
type Root struct {
	// Path is the folder, slash-separated and relative to the fleet's mount
	// root (".claude", ".config/gh", ".mnt"). The Root covers it and
	// everything below it.
	Path string
	// Tool names the agent tool whose state the folder holds, as the resolver
	// names its mount ("Claude Code"), or is empty for a folder of user
	// content or fleet-made files.
	Tool string
	// Rules are matched in order against a path relative to Path; the first
	// match decides.
	Rules []Rule
	// Default is the policy of the folder itself and of every path no rule
	// matches.
	Default Policy
}

// Rule gives the paths a pattern matches a policy.
type Rule struct {
	// Pattern is slash-separated and relative to the Root's Path. Each
	// element is a path.Match pattern, and an element "**" matches any number
	// of elements, none included: "projects/**" is projects and everything
	// below it, "**/*.lock" any lock file at any depth.
	Pattern string
	Policy  Policy
}

// RuleFor returns the first rule matching name, a slash-separated path
// relative to the Root's Path ("." is the folder itself, which no rule
// matches).
func (r Root) RuleFor(name string) (Rule, bool) {
	if name == "." || name == "" {
		return Rule{}, false
	}
	elems := strings.Split(name, "/")
	for _, rule := range r.Rules {
		if match(strings.Split(rule.Pattern, "/"), elems) {
			return rule, true
		}
	}
	return Rule{}, false
}

// PolicyFor returns the policy of name, relative to the Root's Path.
func (r Root) PolicyFor(name string) Policy {
	if rule, ok := r.RuleFor(name); ok {
		return rule.Policy
	}
	return r.Default
}

// Table is a set of sync roots.
type Table struct {
	roots []Root
}

// Roots returns the table's roots in order.
func (t Table) Roots() []Root {
	return append([]Root(nil), t.roots...)
}

// Root returns the deepest root covering name, a slash-separated path
// relative to the fleet's mount root, and name relative to it. ok is false
// when no root covers name: sharesync does not sync it.
func (t Table) Root(name string) (root Root, inner string, ok bool) {
	for _, candidate := range t.roots {
		rest, covers := within(name, candidate.Path)
		if covers && (!ok || len(candidate.Path) > len(root.Path)) {
			root, inner, ok = candidate, rest, true
		}
	}
	return root, inner, ok
}

// PolicyFor returns the policy of name, relative to the fleet's mount root.
// ok is false when no root covers name.
func (t Table) PolicyFor(name string) (Policy, bool) {
	root, inner, ok := t.Root(name)
	if !ok {
		return Unset, false
	}
	return root.PolicyFor(inner), true
}

// Credentials returns the login files the table names, relative to the
// fleet's mount root: each credential rule whose pattern is a plain path.
// The per-location login store keeps exactly these.
func (t Table) Credentials() []string {
	var files []string
	for _, root := range t.roots {
		for _, rule := range root.Rules {
			if rule.Policy == Credential && !hasMeta(rule.Pattern) {
				files = append(files, path.Join(root.Path, rule.Pattern))
			}
		}
	}
	return files
}

// within reports whether name is folder or below it, and returns name
// relative to folder ("." for folder itself).
func within(name, folder string) (string, bool) {
	if name == folder {
		return ".", true
	}
	rest, ok := strings.CutPrefix(name, folder+"/")
	return rest, ok
}

// match reports whether the pattern elements match the path elements; a "**"
// element matches any number of path elements.
func match(pattern, elems []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for skip := 0; skip <= len(elems); skip++ {
				if match(pattern[1:], elems[skip:]) {
					return true
				}
			}
			return false
		}
		if len(elems) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], elems[0]); err != nil || !ok {
			return false
		}
		pattern, elems = pattern[1:], elems[1:]
	}
	return len(elems) == 0
}

// hasMeta reports whether pattern holds a wildcard.
func hasMeta(pattern string) bool {
	return strings.ContainsAny(pattern, `*?[\`)
}
