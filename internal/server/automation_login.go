package server

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// automation_login.go is the automation login check. Logins are per
// location (the daemon host, or one cluster), so an agent fired at a cluster
// where its tool was never logged in for the fleet would sit at a login
// prompt until it is reaped. Each location's
// sync keeps the fleet's copy of that location's login files on the daemon
// host (state.SyncLocationDir), so the check reads only local files and
// needs no cluster call at fire time.
//
// A Kubernetes agent whose command starts claude, codex or auggie
// (fleet.AgentLoginTool) is refused with "log in to Claude once for this
// fleet on ☸ prod-eu" unless the command or the fleet's devcontainer env
// sets the tool's API-key variable, or the location's copy holds the tool's
// login file. Other commands, and agents on the other backends (whose
// location is the daemon host, which the fleet's own folders log in), are
// never checked. FLEET_AUTOMATION_LOGIN_CHECK=off turns it off.

// envAutomationLoginCheck turns the login check off when "off".
const envAutomationLoginCheck = "FLEET_AUTOMATION_LOGIN_CHECK"

// loginCheckOff reports whether the login check is turned off.
func loginCheckOff() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(envAutomationLoginCheck)), "off")
}

// loginRefusal is why the login check refuses ag's firings in fleetName,
// running on the connection named clusterName ("" when it lets them run),
// and the tool to log in to.
func loginRefusal(st *state.State, fleetName string, ag fleet.Agent, clusterName string) (string, fleet.LoginTool) {
	target := ag.DeployTarget()
	if target.Backend != fleet.BackendKubernetes || loginCheckOff() {
		return "", fleet.LoginTool{}
	}
	tool, ok := fleet.AgentLoginTool(ag.Command)
	if !ok || tool.SetBy(ag.Command) {
		return "", fleet.LoginTool{}
	}
	if locationLoggedIn(fleetName, target.KubeCluster, tool) {
		return "", fleet.LoginTool{}
	}
	if tool.SetIn(fleetDevcontainerEnv(st, fleetName)) {
		return "", fleet.LoginTool{}
	}
	if clusterName == "" {
		clusterName = target.KubeCluster
	}
	return "log in to " + tool.Name + " once for this fleet on " + kubeGlyph + " " + clusterName, tool
}

// loginCheck is loginRefusal as the limiter asks it.
func loginCheck(st *state.State, fleetName string, ag fleet.Agent, clusterName string) string {
	why, _ := loginRefusal(st, fleetName, ag, clusterName)
	return why
}

// credentialFile is the login file of tool, relative to the fleet's mount
// root, as the sync policy table names it (".claude/.credentials.json").
func credentialFile(tool fleet.LoginTool) (string, bool) {
	for _, root := range syncpolicy.Defaults().Roots() {
		if root.Tool != tool.MountName {
			continue
		}
		for _, rule := range root.Rules {
			if rule.Policy == syncpolicy.Credential {
				return path.Join(root.Path, rule.Pattern), true
			}
		}
	}
	return "", false
}

// locationLoggedIn reports whether the fleet's copy of location's login
// files holds tool's (a regular file with something in it). The daemon
// host's own login never counts for a cluster.
func locationLoggedIn(fleetName, location string, tool fleet.LoginTool) bool {
	rel, ok := credentialFile(tool)
	if !ok {
		return false
	}
	fi, err := os.Lstat(filepath.Join(state.SyncLocationDir(fleetName, location), filepath.FromSlash(rel)))
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// fleetDevcontainerEnv is the names the fleet's devcontainer.json sets in
// containerEnv and remoteEnv, read from the newest checkout of the fleet on
// the daemon host (an instance's workspace, or a template fleet's own
// folder). A name counts only when its value is set: "${localEnv:X}" is
// resolved the way the devcontainer CLI resolves it on this host, so a key
// passed through from a variable the daemon does not have is not one.
// Best-effort: with no checkout, or one that does not parse, it is empty, and
// only the agent's command counts.
func fleetDevcontainerEnv(st *state.State, fleetName string) map[string]bool {
	names := map[string]bool{}
	if st == nil {
		return names
	}
	f := st.Fleets[fleetName]
	if f == nil {
		return names
	}
	for _, dir := range devcontainerDirs(f) {
		for _, cfg := range []string{filepath.Join(".devcontainer", "devcontainer.json"), ".devcontainer.json"} {
			data, err := os.ReadFile(filepath.Join(dir, cfg))
			if err != nil {
				continue
			}
			var doc struct {
				ContainerEnv map[string]any `json:"containerEnv"`
				RemoteEnv    map[string]any `json:"remoteEnv"`
			}
			if json.Unmarshal(devcontainer.ToStrictJSON(data), &doc) != nil {
				continue
			}
			for _, env := range []map[string]any{doc.ContainerEnv, doc.RemoteEnv} {
				for k, v := range env {
					if devcontainerEnvSet(v, os.LookupEnv) {
						names[k] = true
					}
				}
			}
			return names
		}
	}
	return names
}

// localEnvRef is a devcontainer.json ${localEnv:NAME} or
// ${localEnv:NAME:default} reference.
var localEnvRef = regexp.MustCompile(`\$\{localEnv:([^}:]*)(?::([^}]*))?\}`)

// devcontainerEnvSet reports whether a containerEnv or remoteEnv value sets
// its variable to something: a string not empty once its ${localEnv:...}
// references are resolved through lookup (the default when the variable is
// unset or empty, as the devcontainer CLI does), or any other JSON value but
// null. Other references (${containerEnv:...}) are the container's to
// resolve, so they count.
func devcontainerEnvSet(v any, lookup func(string) (string, bool)) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		resolved := localEnvRef.ReplaceAllStringFunc(v, func(ref string) string {
			m := localEnvRef.FindStringSubmatch(ref)
			if val, ok := lookup(m[1]); ok && val != "" {
				return val
			}
			return m[2]
		})
		return resolved != ""
	}
	return true
}

// devcontainerDirs are the fleet's checkouts on the daemon host, newest
// first: a template fleet's folder, then its instances' workspaces.
func devcontainerDirs(f *fleet.Fleet) []string {
	var dirs []string
	if fleet.IsTemplateRemote(f.Remote) {
		if dir, err := fleet.TemplateDir(f.Remote); err == nil {
			dirs = append(dirs, dir)
		}
	}
	insts := append([]*fleet.Instance(nil), f.Instances...)
	sort.SliceStable(insts, func(i, j int) bool { return insts[i].CreatedAt.After(insts[j].CreatedAt) })
	for _, inst := range insts {
		if inst.WorkspaceDir != "" {
			dirs = append(dirs, inst.WorkspaceDir)
		}
	}
	return dirs
}
