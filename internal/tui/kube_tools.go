package tui

import (
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	tea "github.com/charmbracelet/bubbletea"
)

// kube_tools.go adds the connections' login plugins to the fleetd rows of
// Settings -> Tool Status: one row per connection whose context logs in
// through an exec credential plugin, found or missing on the daemon host, and
// enter opens the plugin's install page here, where the user reads it.

// kubePlugin is a connection's exec credential plugin as the daemon host's
// kubeconfig names it.
type kubePlugin struct {
	command string // as the kubeconfig writes it ("aws")
	missing string // why fleetd does not find it on its PATH; "" when found
}

// kubePluginsMsg carries the plugins of the connections' contexts, by
// connection id, fetched for watch generation gen.
type kubePluginsMsg struct {
	gen     int
	plugins map[string]kubePlugin
	host    string
}

// loadKubePluginsCmd reads the contexts each connection uses (one
// ListKubeContexts per set of kubeconfig files) off the Update loop.
func loadKubePluginsCmd(gen int, clusters []*fleetgrpc.KubeClusterStatus) tea.Cmd {
	type want struct{ id, context string }
	byPaths := map[string][]want{}
	paths := map[string][]string{}
	for _, st := range clusters {
		c := st.GetCluster()
		key := strings.Join(c.GetKubeconfigPaths(), "\x00")
		byPaths[key] = append(byPaths[key], want{c.GetId(), c.GetContext()})
		paths[key] = c.GetKubeconfigPaths()
	}
	if len(byPaths) == 0 {
		return func() tea.Msg { return kubePluginsMsg{gen: gen, plugins: map[string]kubePlugin{}} }
	}
	return func() tea.Msg {
		out := kubePluginsMsg{gen: gen, plugins: map[string]kubePlugin{}}
		for key, wants := range byPaths {
			reply, err := listKubeContextsRemote(paths[key])
			if err != nil {
				continue // the connection's own health already says why
			}
			if out.host == "" {
				out.host = reply.GetHost()
			}
			for _, w := range wants {
				for _, kc := range reply.GetContexts() {
					if kc.GetName() == w.context && kc.GetPlugin() != "" {
						out.plugins[w.id] = kubePlugin{command: kc.GetPlugin(), missing: kc.GetPluginMissing()}
					}
				}
			}
		}
		return out
	}
}

// handleKubePluginsMsg stores the plugins, keeping an open Settings page's
// cursor on its item (the Tool Status rows may grow under it).
func (m *model) handleKubePluginsMsg(msg kubePluginsMsg) {
	if msg.gen != m.watchGen {
		return
	}
	page, _ := m.currentPage.(*settingsPage)
	item := -1
	if page != nil {
		item = page.settingsCursorItem(m)
	}
	m.kubePlugins = msg.plugins
	if msg.host != "" {
		m.kubeHost = msg.host
	}
	if page != nil && item >= 0 {
		page.cursorToItem(m, item)
	}
}

// kubePluginTools are the login plugin rows: one per connection with a
// plugin, in the connections' order. A connection in PLUGIN_MISSING shows its
// plugin missing; any later state proves fleetd found it (with the record's
// extra PATH, which ListKubeContexts does not apply).
func kubePluginTools(m *model) []deps.ToolStatus {
	var out []deps.ToolStatus
	for _, st := range m.kubeStatuses() {
		c := st.GetCluster()
		p, ok := m.kubePlugins[c.GetId()]
		if !ok {
			continue
		}
		found := p.missing == ""
		switch st.GetState() {
		case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING:
			found = false
		case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING,
			fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING:
			// Not judged yet: go by the kubeconfig lookup.
		default:
			found = true
		}
		out = append(out, deps.ToolStatus{
			Name:        filepath.Base(p.command),
			Binary:      p.command,
			Description: "Logs in to " + kubeGlyphCluster + " " + kubeDisplayName(c) + ".",
			InstallURL:  kubePluginInstallURL(p.command),
			Found:       found,
		})
	}
	return out
}

// kubePluginInstallURL is the install page of a known login plugin, else the
// Kubernetes page on credential plugins.
func kubePluginInstallURL(command string) string {
	switch strings.ToLower(filepath.Base(command)) {
	case "aws":
		return "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
	case "aws-iam-authenticator":
		return "https://github.com/kubernetes-sigs/aws-iam-authenticator#installation"
	case "gke-gcloud-auth-plugin", "gcloud":
		return "https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin"
	case "kubelogin":
		return "https://azure.github.io/kubelogin/install.html"
	case "kubectl-oidc_login":
		return "https://github.com/int128/kubelogin#setup"
	case "doctl":
		return "https://docs.digitalocean.com/reference/doctl/how-to/install/"
	case "oci":
		return "https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm"
	}
	return "https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins"
}

// daemonToolRows are the fleetd rows of Tool Status: the daemon host's build
// tools, then the connections' login plugins.
func daemonToolRows(m *model) []deps.ToolStatus {
	var rows []deps.ToolStatus
	if m.daemonTools != nil {
		rows = append(rows, m.daemonTools.tools...)
	}
	return append(rows, kubePluginTools(m)...)
}

// daemonWhere names the daemon host on its tool rows.
func daemonWhere(m *model) string {
	host := m.kubeHost
	if m.daemonTools != nil && m.daemonTools.host != "" {
		host = m.daemonTools.host
	}
	if host == "" {
		return "on fleetd"
	}
	return "on fleetd @ " + host
}
