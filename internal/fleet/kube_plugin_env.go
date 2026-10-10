package fleet

import (
	"fmt"
	"path/filepath"
	"strings"
)

// The rules for the two KubeCluster settings that shape how a kubeconfig's
// exec credential plugin runs on the daemon host. A plugin gets the daemon's
// basic process environment (HOME, locale, proxies, ...), the kubeconfig's own
// env entries and PluginEnv, and nothing else of the daemon's environment;
// PATH is ExtraPath in front of the daemon's PATH. internal/kubeexec applies
// them; the daemon validates records with these functions, and a client may
// too before it sends one.

// pluginEnvReserved are the names PluginEnv may not set: fleet owns them.
var pluginEnvReserved = map[string]string{
	"PATH":                 "use extra_path to add directories to the plugin's PATH",
	"KUBERNETES_EXEC_INFO": "it is set for each run of the plugin",
}

// secretEnvNameParts are the name fragments that mark a variable as holding a
// credential, whose value a record (which every client can read) must not hold.
var secretEnvNameParts = []string{"SECRET", "TOKEN", "PASSWORD", "KEY"}

// ParsePluginEnv splits a PluginEnv entry: "NAME" passes the daemon's own value
// of NAME through, "NAME=value" sets it.
func ParsePluginEnv(entry string) (name, value string, hasValue bool) {
	return strings.Cut(entry, "=")
}

// IsSecretEnvName reports whether a variable name looks like it holds a
// credential (it contains SECRET, TOKEN, PASSWORD or KEY, in any case).
func IsSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range secretEnvNameParts {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}

// ValidatePluginEnv reports the first PluginEnv entry the daemon refuses: a
// name that is not a shell variable name, a name fleet sets itself, a name
// given twice, or a value for a name that looks secret (pass such a variable
// by name, so its value stays in the daemon's environment).
func ValidatePluginEnv(entries []string) error {
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name, value, hasValue := ParsePluginEnv(entry)
		switch {
		case !isEnvName(name):
			return fmt.Errorf("plugin_env %q: %q is not a variable name (letters, digits and _, not starting with a digit)", entry, name)
		case pluginEnvReserved[name] != "":
			return fmt.Errorf("plugin_env %q: %s cannot be set here; %s", entry, name, pluginEnvReserved[name])
		case seen[name]:
			return fmt.Errorf("plugin_env: %s is listed twice", name)
		case hasValue && IsSecretEnvName(name):
			return fmt.Errorf("plugin_env: %s looks like a secret, so it cannot carry a value; list it as %q to pass the daemon's own %s", name, name, name)
		case strings.ContainsRune(value, 0):
			return fmt.Errorf("plugin_env: the value of %s contains a NUL byte", name)
		}
		seen[name] = true
	}
	return nil
}

// ValidateExtraPath reports the first ExtraPath entry the daemon refuses: each
// must be an absolute directory path on the daemon host, without the PATH list
// separator.
func ValidateExtraPath(dirs []string) error {
	for _, dir := range dirs {
		switch {
		case !filepath.IsAbs(dir):
			return fmt.Errorf("extra_path %q: must be an absolute path on the daemon host", dir)
		case strings.ContainsRune(dir, filepath.ListSeparator) || strings.ContainsRune(dir, 0):
			return fmt.Errorf("extra_path %q: one directory per entry (no %q)", dir, filepath.ListSeparator)
		}
	}
	return nil
}

func isEnvName(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
