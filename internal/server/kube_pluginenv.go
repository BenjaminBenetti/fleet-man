//go:build !nok8s

package server

import (
	"path/filepath"
	"sort"
	"strings"
)

// kube_pluginenv.go picks the plugin_env a new connection starts with. A
// credential plugin sees only the process basics of fleetd's environment
// (kubeexec), so the variables a cloud CLI reads (AWS_PROFILE, CLOUDSDK_*,
// AZURE_*) would reach it empty and it would fail; a connection added
// without plugin_env passes through, by name, the ones fleetd has for its
// plugin's family, plus the CA bundles and the session bus keyring caches
// use. Names only: the values stay in fleetd's environment.

// pluginEnvFamilies maps a plugin (its file name) to the variable prefixes
// its CLI reads.
var pluginEnvFamilies = map[string][]string{
	"aws":                    {"AWS_"},
	"aws-iam-authenticator":  {"AWS_"},
	"gke-gcloud-auth-plugin": {"CLOUDSDK_", "GOOGLE_"},
	"gcloud":                 {"CLOUDSDK_", "GOOGLE_"},
	"kubelogin":              {"AZURE_", "AAD_"},
	"az":                     {"AZURE_", "AAD_"},
}

// pluginEnvAlways are passed through for any plugin, when fleetd has them.
var pluginEnvAlways = []string{"REQUESTS_CA_BUNDLE", "AWS_CA_BUNDLE", "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", "NODE_EXTRA_CA_CERTS", "DBUS_SESSION_BUS_ADDRESS"}

// defaultPluginEnv is the plugin_env for a new connection whose context runs
// plugin, from fleetd's environment (sorted names).
func defaultPluginEnv(plugin string, environ []string) []string {
	if plugin == "" {
		return nil
	}
	prefixes := pluginEnvFamilies[filepath.Base(plugin)]
	have := map[string]bool{}
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				have[name] = true
			}
		}
	}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		for _, always := range pluginEnvAlways {
			if name == always {
				have[name] = true
			}
		}
	}
	out := make([]string, 0, len(have))
	for name := range have {
		if isEnvVarName(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func isEnvVarName(s string) bool {
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
