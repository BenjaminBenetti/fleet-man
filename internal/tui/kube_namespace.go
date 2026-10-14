package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kube_namespace.go is the cluster editor's namespace panel. The namespace a
// connection is connected with, or set to, the daemon creates when it does
// not exist (AddKubeCluster, UpdateKubeCluster), so the panel is for the
// rest: a namespace that went missing since ([ Create ], c: the exact
// manifest from a dry run first, labels included, created on enter), and one
// the daemon could not create for want of namespaces create (opened with
// the manifest it would have made, to copy and apply as someone who may, or
// to try again).

// kubeNamespacePanel previews and creates one namespace of a saved
// connection.
type kubeNamespacePanel struct {
	id   string
	name string

	manifest string
	// refused: the daemon could not create it (err says why); the manifest
	// is for someone who may.
	refused  bool
	loading  bool
	creating bool
	err      string
}

// newKubeNamespaceRefusal opens the panel on a namespace the daemon could not
// create for connection id, showing the manifest it would have made.
func newKubeNamespaceRefusal(id string, res *fleetgrpc.KubeNamespaceResult) *kubeNamespacePanel {
	return &kubeNamespacePanel{id: id, name: res.GetNamespace(), manifest: res.GetManifest(), refused: true,
		err: res.GetError() + " · y copies the manifest"}
}

// kubeNamespaceMsg is the answer to the panel's dry run or create.
type kubeNamespaceMsg struct {
	panel  *kubeNamespacePanel
	dryRun bool
	reply  *fleetgrpc.CreateKubeNamespaceReply
	err    error
}

// newKubeNamespacePanel opens the panel for namespace name of connection
// id, and asks for its manifest.
func newKubeNamespacePanel(id, name string) (*kubeNamespacePanel, tea.Cmd) {
	p := &kubeNamespacePanel{id: id, name: name, loading: true}
	return p, p.call(true)
}

func (p *kubeNamespacePanel) call(dryRun bool) tea.Cmd {
	req := &fleetgrpc.CreateKubeNamespaceRequest{Id: p.id, Namespace: p.name, DryRun: dryRun}
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
			p.refused = true
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
	case p.manifest != "" && p.refused:
		b.WriteString(dimStyle.Render("  apply this as someone who may create namespaces, or enter to try again:") + "\n")
	case p.manifest != "":
		b.WriteString(dimStyle.Render("  fleet creates exactly this, labels included:") + "\n")
	}
	if p.manifest != "" {
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
