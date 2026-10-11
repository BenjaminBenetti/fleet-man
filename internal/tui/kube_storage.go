package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
)

// kube_storage.go is the cluster editor's Storage group: the two class
// pickers (Shared storage, the fleets' shares; Instance disks, every
// instance's root disk), each showing what Auto resolved to and what the
// last preflight found of it: the share mode with the single-node warning
// and the fleet-size cap, the expected outcome of the classes whose
// multi-node test result is known in advance (SMB Azure Files and EFS
// access points fail it, Filestore must be enabled and is large), the NFS
// warning for instance disks. Under the group's fields: the attach limits,
// every class with its multi-node test result, each fleet's share on the
// cluster with how full it is, and the fleets whose share is moving to a
// new shared class. [ test storage ] runs the multi-node test again, and
// asks before it tests a class whose volumes are large (and billed while
// the test runs); so does picking one as the shared class. When the
// cluster forbids listing StorageClasses, a class is typed by name and the
// test still validates it. Everything comes from the pushed status and
// GetShareSync, so a remote TUI shows and does exactly what a local one
// does.

// The pickers' paths.
const (
	kubeSharedClassPath = "shared_storage_class"
	kubeDiskClassPath   = "storage_class"
)

// kubeOneNodeWarning starts the daemon's capability line of single-node
// mode on a cluster of several nodes ("... (cap 6)").
const kubeOneNodeWarning = "Each fleet's instances run together on one node"

// kubeEditorActionTestStorage is the Storage group's [ test storage ] row.
const kubeEditorActionTestStorage = "test_storage"

// kubeConsent is a test of StorageClasses with a large minimum size waiting
// for the user's yes: one the Shared storage picker would save (field set:
// saving it tests it), or the ones [ test storage ] would add.
type kubeConsent struct {
	field   *kubeField
	value   string
	rev     int64
	classes []string
	minimum string
}

// kubeShareUsageMsg is the fleets' shares on the editor's cluster, as
// GetShareSync measured them.
type kubeShareUsageMsg struct {
	editor *kubeEditor
	shares []*fleetgrpc.KubeShareUsage
	err    error
}

// loadUsage reads the fleets' shares on the editor's cluster.
func (e *kubeEditor) loadUsage() tea.Cmd {
	id := e.id
	return func() tea.Msg {
		reply, err := getShareSyncRemote()
		var out []*fleetgrpc.KubeShareUsage
		for _, f := range reply.GetFleets() {
			for _, u := range f.GetShares() {
				if u.GetCluster() == id {
					out = append(out, u)
				}
			}
		}
		return kubeShareUsageMsg{editor: e, shares: out, err: err}
	}
}

// kubeClassNamed is the class name ("" the cluster's default) as the
// preflight listed it, or nil.
func kubeClassNamed(caps *fleetgrpc.KubeCapabilities, name string) *fleetgrpc.KubeStorageClass {
	for _, sc := range caps.GetStorageClasses() {
		if (name != "" && sc.GetName() == name) || (name == "" && sc.GetIsDefault()) {
			return sc
		}
	}
	return nil
}

// kubeAutoClass is what a class picker's Auto resolved to ("" unknown).
func kubeAutoClass(st *fleetgrpc.KubeClusterStatus, path string) string {
	switch path {
	case kubeSharedClassPath:
		return st.GetCapabilities().GetSharedStorageClass()
	case kubeDiskClassPath:
		return st.GetCapabilities().GetInstanceStorageClass()
	}
	return ""
}

// kubeEffectiveClass is the class a picker stands for: the one set, else
// Auto's.
func kubeEffectiveClass(st *fleetgrpc.KubeClusterStatus, path string) string {
	f := kubeField{path: path, kind: kubeKindClass}
	if v := f.text(st.GetCluster()); v != "" {
		return v
	}
	return kubeAutoClass(st, path)
}

// kubeNeedsConsent reports whether testing class would make a volume of a
// large minimum size it was not tested at yet: the size, or "".
func kubeNeedsConsent(caps *fleetgrpc.KubeCapabilities, class string) string {
	sc := kubeClassNamed(caps, class)
	if sc == nil || sc.GetLargeMinimum() == "" || class == "" {
		return ""
	}
	if t := sc.GetMultiNodeTest(); t != nil && t.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED {
		return ""
	}
	return sc.GetLargeMinimum()
}

// askConsent holds a pick of the Shared storage class that would test a
// class with a large minimum size until the user says yes; it reports
// whether it did.
func (e *kubeEditor) askConsent(st *fleetgrpc.KubeClusterStatus, f kubeField, value string, rev int64) bool {
	if f.path != kubeSharedClassPath {
		return false
	}
	value = strings.TrimSpace(value)
	min := kubeNeedsConsent(st.GetCapabilities(), value)
	if min == "" {
		return false
	}
	e.consent = &kubeConsent{field: &f, value: value, rev: rev, classes: []string{value}, minimum: min}
	return true
}

// testStorage is [ test storage ]: the multi-node test again on the
// classes the share could use, asking first when Auto would add classes
// with a large minimum size.
func (e *kubeEditor) testStorage(m *model, st *fleetgrpc.KubeClusterStatus) tea.Cmd {
	caps := st.GetCapabilities()
	if st.GetCluster().GetSharedStorageClass() == "" {
		var large []string
		min := ""
		for _, sc := range caps.GetStorageClasses() {
			if size := kubeNeedsConsent(caps, sc.GetName()); size != "" {
				large, min = append(large, sc.GetName()), size
			}
		}
		if len(large) > 0 {
			e.consent = &kubeConsent{classes: large, minimum: min}
			return nil
		}
	}
	return e.runStorageTest(m, nil)
}

// runStorageTest starts the multi-node test again, classes added (the
// consent they need given).
func (e *kubeEditor) runStorageTest(m *model, classes []string) tea.Cmd {
	m.message = "Testing the storage classes across two nodes (about a minute each)"
	return m.startKubeTest(kubeTestKey(e.id), e.id, &fleetgrpc.TestKubeClusterRequest{Id: e.id, TestStorage: true, StorageClasses: classes})
}

// consentKey answers a pending consent: y goes ahead, n or esc declines.
func (e *kubeEditor) consentKey(m *model, key string) (tea.Cmd, bool) {
	c := e.consent
	switch key {
	case "y":
		e.consent = nil
		if c.field != nil {
			return e.save(*c.field, c.value, c.rev), true
		}
		return e.runStorageTest(m, c.classes), true
	case "n", "esc":
		e.consent = nil
		if c.field != nil {
			m.message = c.field.label + " unchanged"
			return nil, true
		}
		return e.runStorageTest(m, nil), true
	}
	return nil, false
}

// consentLine is the pending consent's question.
func (c *kubeConsent) line() string {
	what := strings.Join(c.classes, ", ")
	q := fmt.Sprintf("%s %s: each volume is at least %s, and the multi-node test makes one (billed while it runs)", kubeGlyphWarn, what, c.minimum)
	if c.field != nil {
		return kubeWarnStyle.Render(q) + "  " + dialogHint.Render("[y] save and test it  [n] keep the setting")
	}
	return kubeWarnStyle.Render(q) + "  " + dialogHint.Render("[y] test it too  [n] test the others")
}

// kubeStorageChecks are the checks the Storage group shows (and the
// Capabilities group leaves to it).
var kubeStorageChecks = map[string]bool{"root_storage": true, "shared_storage": true, "attach_limits": true, "snapshots": true, "share_pods": true}

// kubeStorageSummary is the folded Storage group's line: the share mode,
// the fleet-size cap, the classes.
func kubeStorageSummary(st *fleetgrpc.KubeClusterStatus) string {
	caps := st.GetCapabilities()
	var parts []string
	if mode := caps.GetShareMode(); mode != "" {
		parts = append(parts, mode+" mode")
	}
	if n := caps.GetSingleNodeFleetCap(); n > 0 {
		parts = append(parts, fmt.Sprintf("cap %d", n))
	}
	if c := kubeEffectiveClass(st, kubeSharedClassPath); c != "" {
		parts = append(parts, "share "+c)
	}
	if c := kubeEffectiveClass(st, kubeDiskClassPath); c != "" {
		parts = append(parts, "disks "+c)
	}
	if n := len(st.GetShareMigrations()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d moving", n))
	}
	return strings.Join(parts, " · ")
}

// kubeShareModeBadge is the share mode the last preflight resolved.
func kubeShareModeBadge(st *fleetgrpc.KubeClusterStatus) string {
	caps := st.GetCapabilities()
	switch {
	case caps.GetShareRefused() != "":
		return kubeErrStyle.Render(kubeGlyphFailed + " fleet shares refused")
	case caps.GetShareMode() == "multi-node":
		return kubeOKStyle.Render(kubeGlyphReady + " multi-node mode")
	case caps.GetShareMode() == "copy":
		return kubeWarnStyle.Render(kubeGlyphLimited + " copy tier")
	case caps.GetShareMode() == "single-node" && kubeOneNode(st) != "":
		return kubeWarnStyle.Render(kubeGlyphLimited + " single-node mode")
	case caps.GetShareMode() == "single-node":
		return dimStyle.Render("single-node mode")
	}
	return ""
}

// kubeOneNode is the single-node warning of a cluster of several nodes
// ("" when it has none).
func kubeOneNode(st *fleetgrpc.KubeClusterStatus) string {
	for _, w := range st.GetWarnings() {
		if strings.HasPrefix(w, kubeOneNodeWarning) {
			return w
		}
	}
	return ""
}

// kubeFleetCap is the fleet-size cap line ("" without one).
func kubeFleetCap(caps *fleetgrpc.KubeCapabilities) string {
	n := caps.GetSingleNodeFleetCap()
	if n <= 0 {
		return ""
	}
	line := fmt.Sprintf("cap: %d instances per fleet", n)
	if n == 1 {
		line = "cap: 1 instance per fleet"
	}
	if r := caps.GetSingleNodeFleetCapReason(); r != "" {
		line += " (" + r + ")"
	}
	return line
}

// kubeCheckLines are a check's lines: its mark and detail (each detail line
// its own), and its fix.
func kubeCheckLines(c *fleetgrpc.KubeCheck, label bool) []string {
	if c == nil {
		return nil
	}
	detail := strings.Split(strings.TrimRight(c.GetDetail(), "\n"), "\n")
	head := kubeCheckMark(c.GetResult()) + " "
	if label {
		head += kubeCheckLabel(c.GetName()) + "  "
	}
	out := []string{head + dimStyle.Render(detail[0])}
	for _, more := range detail[1:] {
		out = append(out, "  "+dimStyle.Render(more))
	}
	if c.GetFix() != "" {
		out = append(out, "  "+dimStyle.Render("hint: "+c.GetFix()))
	}
	return out
}

// storageFieldLines are the lines under the Storage group's class pickers.
func (e *kubeEditor) storageFieldLines(st *fleetgrpc.KubeClusterStatus, f kubeField) []string {
	caps := st.GetCapabilities()
	var out []string
	switch f.path {
	case kubeSharedClassPath:
		out = append(out, kubeCheckLines(kubeCheck(st, "shared_storage"), false)...)
		if w := kubeOneNode(st); w != "" {
			out = append(out, kubeWarningLine(w))
		}
		if line := kubeFleetCap(caps); line != "" {
			out = append(out, "  "+dimStyle.Render(line))
		}
		if sc := kubeClassNamed(caps, kubeEffectiveClass(st, f.path)); sc != nil && sc.GetShareHint() != "" &&
			sc.GetMultiNodeTest().GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
			out = append(out, "  "+dimStyle.Render("expected: "+sc.GetShareHint()))
		}
		if c := e.consent; c != nil && c.field != nil && c.field.path == f.path {
			out = append(out, c.line())
		}
	case kubeDiskClassPath:
		check := kubeCheck(st, "root_storage")
		out = append(out, kubeCheckLines(check, false)...)
		if sc := kubeClassNamed(caps, kubeEffectiveClass(st, f.path)); sc != nil && sc.GetDiskHint() != "" && !strings.Contains(check.GetDetail(), sc.GetDiskHint()) {
			out = append(out, kubeWarningLine(sc.GetDiskHint()))
		}
	}
	return out
}

// storageHeadLines are the lines under the Storage group's title.
func storageHeadLines(st *fleetgrpc.KubeClusterStatus) []string {
	if st.GetCapabilities().GetStorageClassesUnreadable() {
		return []string{kubeWarnStyle.Render(kubeGlyphLimited+" StorageClasses are not readable here") + "  " +
			dimStyle.Render("type a class name on a picker (enter); the multi-node test still validates it")}
	}
	return nil
}

// storageTailLines are the lines under the Storage group's last field:
// the attach limits, the snapshot classes, each class with its multi-node
// test result, the fleets' shares and the moves under way.
func (e *kubeEditor) storageTailLines(st *fleetgrpc.KubeClusterStatus) []string {
	caps := st.GetCapabilities()
	var out []string
	out = append(out, kubeCheckLines(kubeCheck(st, "attach_limits"), true)...)
	out = append(out, kubeCheckLines(kubeCheck(st, "snapshots"), true)...)
	for i, sc := range caps.GetStorageClasses() {
		if i == 8 {
			out = append(out, dimStyle.Render(fmt.Sprintf("… %d more classes", len(caps.GetStorageClasses())-8)))
			break
		}
		out = append(out, dimStyle.Render("class "+kubeStorageClassLine(sc))+"  "+kubeMultiNodeBadge(sc))
	}
	if c := e.consent; c != nil && c.field == nil {
		out = append(out, c.line())
	}
	out = append(out, e.shareUsageLines()...)
	for _, mv := range st.GetShareMigrations() {
		out = append(out, kubeShareMoveLine(mv))
	}
	return out
}

// kubeMultiNodeBadge is a class's multi-node test result in brief.
func kubeMultiNodeBadge(sc *fleetgrpc.KubeStorageClass) string {
	t := sc.GetMultiNodeTest()
	switch t.GetResult() {
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS:
		return kubeOKStyle.Render(kubeGlyphPass + " multi-node")
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN:
		return kubeWarnStyle.Render(kubeGlyphLimited+" multi-node: ") + dimStyle.Render(kubeCut(t.GetDetail(), 70))
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL:
		return kubeErrStyle.Render(kubeGlyphFailed+" multi-node: ") + dimStyle.Render(kubeCut(t.GetDetail(), 70))
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED:
		if sc.GetLargeMinimum() != "" {
			return dimStyle.Render(kubeGlyphSkipped + " not tested: each volume is at least " + sc.GetLargeMinimum() + " ([ test storage ] asks first)")
		}
		return dimStyle.Render(kubeGlyphSkipped + " " + strings.TrimPrefix(t.GetDetail(), "not tested: "))
	}
	if sc.GetShareHint() != "" {
		return dimStyle.Render("expected: " + kubeCut(sc.GetShareHint(), 70))
	}
	return ""
}

// kubeCut shortens s to n runes.
func kubeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// shareUsageLines are the fleets' shares on the cluster, how full.
func (e *kubeEditor) shareUsageLines() []string {
	if e.usageErr != nil {
		return []string{dimStyle.Render("shares: " + kubeRPCError(e.usageErr))}
	}
	shares := slices.Clone(e.usage)
	slices.SortFunc(shares, func(a, b *fleetgrpc.KubeShareUsage) int { return strings.Compare(a.GetFleet(), b.GetFleet()) })
	var out []string
	for _, u := range shares {
		line := "share of " + u.GetFleet() + ": "
		switch {
		case u.GetMeasured() && u.GetCapacityBytes() > 0:
			line += fmt.Sprintf("%s of %s (%d%%)", formatCacheBytes(u.GetUsedBytes()), formatCacheBytes(u.GetCapacityBytes()), u.GetUsedBytes()*100/u.GetCapacityBytes())
		case u.GetCapacityBytes() > 0:
			line += formatCacheBytes(u.GetCapacityBytes()) + " (not measured: no pod of the fleet runs)"
		default:
			line += "not measured"
		}
		if u.GetExpandable() {
			line += " · grows when 90% full"
		}
		if w := u.GetWarning(); w != "" {
			out = append(out, dimStyle.Render(line)+"  "+kubeWarnStyle.Render(kubeGlyphWarn+" "+w))
			continue
		}
		out = append(out, dimStyle.Render(line))
	}
	return out
}

// kubeShareMoveLine is one fleet's share moving to the shared class.
func kubeShareMoveLine(mv *fleetgrpc.KubeShareMigration) string {
	class := func(c string) string {
		if c == "" {
			return "the default class"
		}
		return c
	}
	head := fmt.Sprintf("%s's share moves %s → %s", mv.GetFleet(), class(mv.GetFromClass()), class(mv.GetToClass()))
	detail := mv.GetState()
	if mv.GetDetail() != "" {
		detail += ": " + mv.GetDetail()
	}
	switch mv.GetState() {
	case "failed", "blocked":
		return kubeWarnStyle.Render(kubeGlyphWarn+" "+head) + "  " + dimStyle.Render(detail)
	}
	return dimStyle.Render("⟳ "+head) + "  " + dimStyle.Render(detail)
}
