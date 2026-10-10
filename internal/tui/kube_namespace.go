package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kube_namespace.go is the explicit namespace Create action of the context
// picker and the cluster editor. Fleet never creates a namespace on its own
// (that is a cluster-scoped write): the panel first shows the exact manifest,
// labels included, from a dry run, and creates it only on enter. Without
// namespaces/create the manifest is there to copy and apply as someone who
// has it.

// kubeNamespacePanel previews and creates one namespace.
type kubeNamespacePanel struct {
	id    string                 // the saved connection; "" with draft
	draft *fleetgrpc.KubeCluster // the picker's unsaved connection
	name  string

	manifest string
	loading  bool
	creating bool
	err      string
}

// kubeNamespaceMsg is the answer to the panel's dry run or create.
type kubeNamespaceMsg struct {
	panel  *kubeNamespacePanel
	dryRun bool
	reply  *fleetgrpc.CreateKubeNamespaceReply
	err    error
}

// newKubeNamespacePanel opens the panel for namespace name of a saved
// connection (id) or a draft, and asks for its manifest.
func newKubeNamespacePanel(id string, draft *fleetgrpc.KubeCluster, name string) (*kubeNamespacePanel, tea.Cmd) {
	p := &kubeNamespacePanel{id: id, draft: draft, name: name, loading: true}
	return p, p.call(true)
}

func (p *kubeNamespacePanel) call(dryRun bool) tea.Cmd {
	req := &fleetgrpc.CreateKubeNamespaceRequest{Id: p.id, Namespace: p.name, DryRun: dryRun}
	if p.id == "" {
		req.Cluster = p.draft
	}
	return func() tea.Msg {
		reply, err := createKubeNamespaceRemote(req)
		return kubeNamespaceMsg{panel: p, dryRun: dryRun, reply: reply, err: err}
	}
}

// handle applies an answer; created reports a namespace that now exists.
func (p *kubeNamespacePanel) handle(msg kubeNamespaceMsg) (created bool) {
	p.loading, p.creating = false, false
	if msg.err != nil {
		p.err = kubeRPCError(msg.err)
		if status.Code(msg.err) == codes.AlreadyExists {
			p.err = ""
			return true
		}
		if status.Code(msg.err) == codes.PermissionDenied {
			p.err += " · y copies the manifest"
		}
		return false
	}
	if m := msg.reply.GetManifest(); m != "" {
		p.manifest = m
	}
	return !msg.dryRun && msg.reply.GetCreated()
}

// key handles a key; closed reports the panel was dismissed.
func (p *kubeNamespacePanel) key(m *model, key tea.KeyMsg) (cmd tea.Cmd, closed bool) {
	switch key.String() {
	case "esc", "q":
		return nil, true
	case "enter":
		if p.loading || p.creating || p.manifest == "" {
			return nil, false
		}
		p.creating, p.err = true, ""
		return p.call(false), false
	case "y":
		if p.manifest == "" {
			return nil, false
		}
		m.message = fmt.Sprintf("Namespace %s manifest copied to clipboard", p.name)
		return copyToClipboardCmd(p.manifest), false
	}
	return nil, false
}

// view renders the panel inside its dialog, width columns wide.
func (p *kubeNamespacePanel) view(m *model, width int) string {
	var b strings.Builder
	b.WriteString(dialogLabel.Render(fmt.Sprintf("Create namespace %s", p.name)) + "\n")
	switch {
	case p.loading:
		b.WriteString("  " + m.spinner.View() + " " + dimStyle.Render("rendering the manifest…") + "\n")
	case p.manifest != "":
		b.WriteString(dimStyle.Render("  fleet creates exactly this, labels included:") + "\n")
		for _, line := range strings.Split(strings.TrimRight(p.manifest, "\n"), "\n") {
			b.WriteString("    " + line + "\n")
		}
	}
	if p.creating {
		b.WriteString("  " + m.spinner.View() + " " + dimStyle.Render("creating…") + "\n")
	}
	if p.err != "" {
		b.WriteString(wrapToWidth("  "+kubeErrStyle.Render(kubeGlyphFailed+" "+p.err), width) + "\n")
	}
	b.WriteString(dialogHint.Render(kubeFooter(kubeNamespaceKeys, nil)))
	return b.String()
}
