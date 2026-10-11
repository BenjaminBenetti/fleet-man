package server

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// automation_login.go is the automation login check. Logins are per
// location (the daemon host, or one cluster: credential_scope=location), so
// an agent fired at a cluster where its tool was never logged in for the
// fleet would sit at a login prompt until it is reaped. Each location's
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
// files holds tool's (a regular file with something in it), or, for a
// connection that shares the daemon host's logins (credential_scope fleet,
// experimental), the fleet's own folders on the daemon host do.
func locationLoggedIn(fleetName, location string, tool fleet.LoginTool) bool {
	rel, ok := credentialFile(tool)
	if !ok {
		return false
	}
	dirs := []string{state.SyncLocationDir(fleetName, location)}
	if sharesHostLogins(location) {
		dirs = append(dirs, filepath.Join(state.WorkspacesDir(), fleetName))
	}
	for _, dir := range dirs {
		fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return true
		}
	}
	return false
}

// sharesHostLogins reports whether connection id shares the daemon host's
// logins (credential_scope fleet).
func sharesHostLogins(id string) bool {
	reg, err := state.LoadKubeClusters()
	if err != nil {
		return false
	}
	rec := reg.Find(id)
	return rec != nil && rec.CredentialScope == fleet.KubeCredentialsPerFleet
}

// fleetDevcontainerEnv is the names the fleet's devcontainer.json sets in
// containerEnv and remoteEnv, read from the newest checkout of the fleet on
// the daemon host (an instance's workspace, or a template fleet's own
// folder). Best-effort: with no checkout, or one that does not parse, it is
// empty, and only the agent's command counts.
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
			for k := range doc.ContainerEnv {
				names[k] = true
			}
			for k := range doc.RemoteEnv {
				names[k] = true
			}
			return names
		}
	}
	return names
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
