package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/proto"
)

// kube_connect_form.go is the Connect page's form: the basic settings of the
// connection being made, set before it connects so that it starts out
// right. Name; Namespace (the daemon creates it when it does not exist);
// Delivery, how the images fleetd builds reach the cluster, with Registry
// when that is the pick; Storage, the class of the instance disks, which a
// fleet share takes too unless a class passes the multi-node test once the
// connection is saved; and Node-local OK when that class keeps a disk on one
// node of several. The choices come from the chosen context's draft tests:
// its StorageClasses, and what Auto resolves to (network storage before
// node-local). Each change tests the draft again. Everything else is the
// cluster editor's, once connected.

// The form's fields, in order.
const (
	kubeFormName      = "name"
	kubeFormNamespace = "namespace"
	kubeFormDelivery  = "delivery"
	kubeFormRegistry  = "registry"
	kubeFormStorage   = "storage"
	kubeFormNodeLocal = "node_local_ack"
	kubeFormConnect   = "connect"
)

// kubeFormLabels name the fields as their rows do.
var kubeFormLabels = map[string]string{
	kubeFormName:      "Name:",
	kubeFormNamespace: "Namespace:",
	kubeFormDelivery:  "Delivery:",
	kubeFormRegistry:  "Registry:",
	kubeFormStorage:   "Storage:",
	kubeFormNodeLocal: "Node-local OK:",
}

// kubeConnectForm is one context's settings; "" is the default.
type kubeConnectForm struct {
	name, namespace string
	mode, registry  string // image.mode ("" Auto) and image.registry
	storage         string // storage_class ("" Auto)
	nodeLocalAck    bool
}

// kubeFound is what draft tests found of one context: the latest result,
// and what Auto resolved to the last time it was left on Auto.
type kubeFound struct {
	status    *fleetgrpc.KubeClusterStatus
	autoClass string // storage_class's Auto
	autoMode  string // image.mode's Auto
}

// take keeps a finished test of draft: its result, with the checks as they
// streamed when the result carries none.
func (f *kubeFound) take(draft *fleetgrpc.KubeCluster, result *fleetgrpc.KubeClusterStatus, checks []*fleetgrpc.KubeCheck) {
	if len(result.GetChecks()) == 0 && len(checks) > 0 {
		result = proto.Clone(result).(*fleetgrpc.KubeClusterStatus)
		result.Checks = checks
	}
	f.status = result
	caps := result.GetCapabilities()
	if class := caps.GetInstanceStorageClass(); draft.GetStorageClass() == "" && class != "" {
		f.autoClass = class
	}
	if mode := caps.GetImageDelivery().GetMode(); draft.GetImage().GetMode() == "" && mode != "" {
		f.autoMode = mode
	}
}

// form is context c's settings.
func (p *kubePicker) form(c *fleetgrpc.KubeContext) *kubeConnectForm {
	f := p.forms[c.GetName()]
	if f == nil {
		f = &kubeConnectForm{}
		p.forms[c.GetName()] = f
	}
	return f
}

// foundFor is what the draft tests of context name found.
func (p *kubePicker) foundFor(name string) *kubeFound {
	f := p.found[name]
	if f == nil {
		f = &kubeFound{}
		p.found[name] = f
	}
	return f
}

// caps is what the latest finished draft test of c found, or nil.
func (p *kubePicker) caps(c *fleetgrpc.KubeContext) *fleetgrpc.KubeCapabilities {
	return p.found[c.GetName()].result().GetCapabilities()
}

// result is f's latest result (nil-safe).
func (f *kubeFound) result() *fleetgrpc.KubeClusterStatus {
	if f == nil {
		return nil
	}
	return f.status
}

// namespaceFor is the namespace a connection of context c takes: the one
// the form sets, else the context's own, except that a context on
// "default" (or on none) is offered "fleet" instead, which the daemon
// creates as it connects.
func (p *kubePicker) namespaceFor(c *fleetgrpc.KubeContext) (ns string, suggested bool) {
	if ns := p.form(c).namespace; ns != "" {
		return ns, false
	}
	if ns := c.GetNamespace(); ns != "" && ns != "default" {
		return ns, false
	}
	return "fleet", true
}

// nameFor is the name a connection of context c takes ("" the daemon names
// it after its id).
func (p *kubePicker) nameFor(m *model, c *fleetgrpc.KubeContext) string {
	if name := p.form(c).name; name != "" {
		return name
	}
	ns, _ := p.namespaceFor(c)
	return kubeDefaultName(m, c.GetName(), ns)
}

// draft is the unsaved connection of context c, as the form sets it.
func (p *kubePicker) draft(m *model, c *fleetgrpc.KubeContext) *fleetgrpc.KubeCluster {
	f := p.form(c)
	ns, _ := p.namespaceFor(c)
	d := &fleetgrpc.KubeCluster{
		DisplayName:     p.nameFor(m, c),
		Context:         c.GetName(),
		KubeconfigPaths: append([]string(nil), p.paths...),
		Namespace:       ns,
		StorageClass:    f.storage,
	}
	if f.mode != "" {
		d.Image = &fleetgrpc.KubeImageSettings{Mode: f.mode}
		if f.mode == kubeDeliveryRegistry {
			d.Image.Registry = f.registry
		}
	}
	if p.nodeLocalShown(c) {
		d.NodeLocalAck = f.nodeLocalAck
	}
	return d
}

// formFields are context c's form rows.
func (p *kubePicker) formFields(c *fleetgrpc.KubeContext) []string {
	out := []string{kubeFormName, kubeFormNamespace, kubeFormDelivery}
	if p.form(c).mode == kubeDeliveryRegistry {
		out = append(out, kubeFormRegistry)
	}
	out = append(out, kubeFormStorage)
	if p.nodeLocalShown(c) {
		out = append(out, kubeFormNodeLocal)
	}
	return append(out, kubeFormConnect)
}

// kubeFormCycles reports whether ←/→ change the field.
func kubeFormCycles(field string) bool {
	return field == kubeFormDelivery || field == kubeFormStorage || field == kubeFormNodeLocal
}

// storageClass is the class the Storage row stands for, as the draft test
// listed it: the one set, else Auto's (nil while unknown).
func (p *kubePicker) storageClass(c *fleetgrpc.KubeContext) *fleetgrpc.KubeStorageClass {
	name := p.form(c).storage
	if name == "" {
		name = p.found[c.GetName()].autoClassName()
		if name == "" {
			return nil
		}
	}
	return kubeClassNamed(p.caps(c), name)
}

func (f *kubeFound) autoClassName() string {
	if f == nil {
		return ""
	}
	return f.autoClass
}

// nodeLocalShown reports whether the form asks for node_local_ack: its
// class keeps a disk on one node, of several.
func (p *kubePicker) nodeLocalShown(c *fleetgrpc.KubeContext) bool {
	return p.storageClass(c).GetNodeLocal() && len(p.caps(c).GetNodes()) > 1
}

// storageOptions are the values ←/→ step the Storage row through: Auto,
// then each class the cluster lists (and the one set, listed or not).
func (p *kubePicker) storageOptions(c *fleetgrpc.KubeContext) []string {
	out := []string{""}
	for _, sc := range p.caps(c).GetStorageClasses() {
		out = append(out, sc.GetName())
	}
	if cur := p.form(c).storage; !slices.Contains(out, cur) {
		out = append(out, cur)
	}
	return out
}

// storageTyped reports whether the Storage row takes a typed name: no test
// is running and none listed a class (the cluster forbids listing them, or
// was not reached).
func (p *kubePicker) storageTyped(m *model, c *fleetgrpc.KubeContext) bool {
	return !p.run(m).running() && len(p.caps(c).GetStorageClasses()) == 0
}

// deliveryOptions are the Delivery selector's choices: Auto (labelled with
// what it resolved to), Node load, Registry and Gateway, with Gateway greyed
// out while the gateway serves no images for this daemon and Node load
// while the nodes are not a local cluster.
func (p *kubePicker) deliveryOptions(c *fleetgrpc.KubeContext) []kubeDeliveryOption {
	caps := p.caps(c)
	auto := "Auto"
	if f := p.found[c.GetName()]; f != nil && f.autoMode != "" {
		auto += "→" + kubeDeliveryModeLabel(f.autoMode)
	}
	opts := []kubeDeliveryOption{
		{Mode: kubeDeliveryAuto, Label: auto},
		{Mode: kubeDeliveryNodeLoad, Label: "Node load"},
		{Mode: kubeDeliveryRegistry, Label: "Registry"},
		{Mode: kubeDeliveryGateway, Label: "Gateway"},
	}
	mode := p.form(c).mode
	for i := range opts {
		opts[i].Selected = opts[i].Mode == mode
		switch del := caps.GetImageDelivery(); opts[i].Mode {
		case kubeDeliveryGateway:
			if del != nil && !del.GetGatewayRegistry() {
				opts[i].Disabled, opts[i].Reason = true, del.GetGatewayReason()
			}
		case kubeDeliveryNodeLoad:
			if len(caps.GetNodes()) > 0 && !caps.GetLocalCluster() {
				opts[i].Disabled, opts[i].Reason = true, "the nodes are not a local cluster fleetd loads images into"
			}
		}
	}
	return opts
}

// enterField is enter on a form row: edit a text field, step a choice, or
// connect.
func (p *kubePicker) enterField(m *model, field string) tea.Cmd {
	c := p.chosenContext()
	if c == nil || p.busy != "" {
		return nil
	}
	f := p.form(c)
	switch field {
	case kubeFormName:
		hint := "empty = the context's name"
		return p.beginInput(kubePickerInputField, field, p.nameFor(m, c), hint)
	case kubeFormNamespace:
		ns, _ := p.namespaceFor(c)
		return p.beginInput(kubePickerInputField, field, ns, "lower-case letters, digits, '-' (created when it does not exist)")
	case kubeFormRegistry:
		return p.beginInput(kubePickerInputField, field, f.registry, "host[:port][/path] the nodes pull from")
	case kubeFormStorage:
		if p.storageTyped(m, c) {
			return p.beginInput(kubePickerInputField, field, f.storage, "a StorageClass name; empty = auto")
		}
		return p.cycleField(m, field, 1)
	case kubeFormDelivery, kubeFormNodeLocal:
		return p.cycleField(m, field, 1)
	case kubeFormConnect:
		return p.connectChosen(m)
	}
	return nil
}

// cycleField steps a choice (dir ±1, wrapping), then tests the draft again.
func (p *kubePicker) cycleField(m *model, field string, dir int) tea.Cmd {
	c := p.chosenContext()
	if c == nil || p.busy != "" {
		return nil
	}
	f := p.form(c)
	switch field {
	case kubeFormDelivery:
		// The next choice that is not greyed out.
		opts := p.deliveryOptions(c)
		cur, n := max(0, slices.IndexFunc(opts, func(o kubeDeliveryOption) bool { return o.Selected })), len(opts)
		for step := 1; step < n; step++ {
			if o := opts[((cur+dir*step)%n+n)%n]; !o.Disabled {
				f.mode = o.Mode
				break
			}
		}
	case kubeFormStorage:
		opts := p.storageOptions(c)
		if len(opts) < 2 {
			return nil
		}
		i := max(0, slices.Index(opts, f.storage))
		f.storage = opts[(i+dir+len(opts))%len(opts)]
	case kubeFormNodeLocal:
		f.nodeLocalAck = !f.nodeLocalAck
	default:
		return nil
	}
	return p.formChanged(m, c)
}

// setField takes a typed value; the default's own value is the default.
func (p *kubePicker) setField(m *model, field, value string) tea.Cmd {
	c := p.chosenContext()
	if c == nil {
		return nil
	}
	f := p.form(c)
	switch field {
	case kubeFormName:
		f.name = ""
		if value != p.nameFor(m, c) {
			f.name = value
		}
	case kubeFormNamespace:
		f.namespace = ""
		if ns, _ := p.namespaceFor(c); value != ns {
			f.namespace = value
		}
	case kubeFormRegistry:
		f.registry = value
	case kubeFormStorage:
		f.storage = value
	}
	return p.formChanged(m, c)
}

// formChanged tests the draft again when what changed is what a test
// judges (the name is not).
func (p *kubePicker) formChanged(m *model, c *fleetgrpc.KubeContext) tea.Cmd {
	if p.sameDraft(m, c) {
		return nil
	}
	return p.startTest(m, c, false)
}

// connectLabel is the form's button: what enter on it does now.
func (p *kubePicker) connectLabel(m *model) string {
	switch {
	case !p.sameDraft(m, p.chosenContext()) || p.run(m).running():
		return "[ Test & connect ]"
	case p.failed:
		return "[ Connect anyway ]"
	}
	return "[ Connect ]"
}

// kubeLabelCell pads a row label to the value column (kubeEditorValueCol).
func kubeLabelCell(label string, active bool) string {
	cell := fmt.Sprintf("%-19s", label)
	if active {
		return selectedStyle.Render(cell) + " "
	}
	return dialogLabel.Render(cell) + " "
}

// formLines render the form of the chosen context (rows are the page's),
// then its draft test.
func (p *kubePicker) formLines(m *model, rows []kubePickerRow, inner int) []string {
	c := p.chosenContext()
	if c == nil {
		return []string{dimStyle.Render("enter on a context sets up its connection here: name, namespace, image delivery, storage")}
	}
	out := []string{dialogLabel.Render("New connection " + kubeGlyphCluster + " " + kubeShortContext(c.GetName(), max(10, inner-20)))}
	for i, row := range rows {
		if row.field == "" {
			continue
		}
		active := i == p.cursor
		marker := "  "
		if active {
			marker = cursorStyle.Render("> ")
		}
		if row.field == kubeFormConnect {
			out = append(out, marker+kubeButton(p.connectLabel(m), active))
			continue
		}
		line := marker + kubeLabelCell(kubeFormLabels[row.field], active) + p.fieldValue(m, c, row.field)
		out = append(out, kubeHangingIndent(line, inner, kubeEditorValueCol))
		if active {
			for _, d := range p.fieldDetails(c, row.field) {
				out = append(out, kubeHangingIndent(strings.Repeat(" ", kubeEditorValueCol)+d, inner, kubeEditorValueCol))
			}
		}
	}
	out = append(out, p.statusLines(m, c, inner, p.current().field == kubeFormConnect)...)
	return append(out, dimStyle.Render("Caches, resources, security and the rest: the cluster editor, once connected"))
}

// fieldValue renders a form field's value: the input while editing it,
// else its value with what the draft test found of it.
func (p *kubePicker) fieldValue(m *model, c *fleetgrpc.KubeContext, field string) string {
	if p.inputMode == kubePickerInputField && p.editing == field {
		return p.input.View()
	}
	f := p.form(c)
	found := p.found[c.GetName()].result()
	testing := p.run(m).running()
	switch field {
	case kubeFormName:
		if name := p.nameFor(m, c); name != "" {
			return name
		}
		return dimStyle.Render("(the connection id)")
	case kubeFormNamespace:
		ns, suggested := p.namespaceFor(c)
		value := ns
		if suggested {
			value += "  " + dimStyle.Render("the context's is default")
		}
		if check := kubeCheck(found, "namespace"); check != nil && found.GetCluster().GetNamespace() == ns {
			switch check.GetResult() {
			case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS:
				value += "  " + kubeCheckMark(check.GetResult()) + " " + dimStyle.Render("exists")
			case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL:
				value += "  " + dimStyle.Render("does not exist: fleet creates it")
			default:
				value += "  " + kubeCheckMark(check.GetResult()) + " " + dimStyle.Render(check.GetDetail())
			}
		}
		return value
	case kubeFormDelivery:
		return kubeDeliverySelector(p.deliveryOptions(c))
	case kubeFormRegistry:
		if f.registry == "" {
			return dimStyle.Render("(none: enter sets the registry the nodes pull from)")
		}
		return f.registry
	case kubeFormStorage:
		shown := f.storage
		if shown == "" {
			shown = "auto"
			if auto := p.found[c.GetName()].autoClassName(); auto != "" {
				shown += " → " + auto
			}
		}
		value := "[ " + shown + " ]"
		switch sc := p.storageClass(c); {
		case sc != nil:
			value += "  " + dimStyle.Render(kubeClassTraits(sc))
		case testing:
			value += "  " + m.spinner.View() + " " + dimStyle.Render("listing the cluster's classes…")
		case p.storageTyped(m, c):
			value += "  " + dimStyle.Render("none listed: enter types a class name")
		}
		return value
	case kubeFormNodeLocal:
		return "[ " + onOff(f.nodeLocalAck) + " ]  " + dimStyle.Render("accept losing an instance's disk with its node (else limited)")
	}
	return ""
}

// fieldDetails are the lines under the active row: what Auto picked and
// why, what a greyed-out choice lacks, what fleet shares do.
func (p *kubePicker) fieldDetails(c *fleetgrpc.KubeContext, field string) []string {
	var out []string
	switch field {
	case kubeFormDelivery:
		found := p.found[c.GetName()].result()
		if del := found.GetCapabilities().GetImageDelivery(); p.form(c).mode == "" && found.GetCluster().GetImage().GetMode() == "" && del.GetReason() != "" {
			out = append(out, dimStyle.Render("Auto picked "+kubeDeliveryModeLabel(del.GetMode())+": "+del.GetReason()))
		}
		for _, o := range p.deliveryOptions(c) {
			if o.Disabled && o.Reason != "" {
				out = append(out, dimStyle.Render(o.Label+" unavailable: "+o.Reason))
			}
		}
	case kubeFormStorage:
		if hint := p.storageClass(c).GetDiskHint(); hint != "" {
			out = append(out, kubeWarnStyle.Render(kubeGlyphWarn+" "+hint))
		}
		out = append(out, dimStyle.Render("instance disks"))
	case kubeFormNamespace:
		out = append(out, dimStyle.Render("created when it does not exist, labelled app.kubernetes.io/managed-by=fleet"))
	}
	return out
}

// kubeClassTraits is a StorageClass in brief, for the Storage row: where
// its volumes live, then its other traits.
func kubeClassTraits(sc *fleetgrpc.KubeStorageClass) string {
	var traits []string
	switch {
	case !sc.GetBlock():
		traits = append(traits, "file share")
	case sc.GetNodeLocal():
		traits = append(traits, "node-local")
	default:
		traits = append(traits, "network")
	}
	if sc.GetIsDefault() {
		traits = append(traits, "the cluster's default")
	}
	if sc.GetAllowExpansion() {
		traits = append(traits, "expandable")
	}
	return strings.Join(traits, " · ")
}
