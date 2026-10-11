package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// kube.go is the TUI's model of the daemon's Kubernetes connections: the
// pushed list (Watch Event.kube_clusters) the Settings rows and the cluster
// editor draw from, the streamed tests and daemon-side logins in flight, and
// the badges every Kubernetes view shares. Badges and check marks are drawn
// in theme slots only (Success, Warning, Error, Muted), so they stay readable
// on the light themes.

// kubeClustersMsg carries the daemon's pushed connections with their health.
type kubeClustersMsg struct {
	clusters *fleetgrpc.KubeClusters
	gen      int
}

// kubeStatuses is every connection the daemon holds, in its order.
func (m *model) kubeStatuses() []*fleetgrpc.KubeClusterStatus {
	return m.kubeClusters.GetClusters()
}

// kubeStatus is the connection with this id, or nil.
func (m *model) kubeStatus(id string) *fleetgrpc.KubeClusterStatus {
	for _, st := range m.kubeStatuses() {
		if st.GetCluster().GetId() == id {
			return st
		}
	}
	return nil
}

// handleKubeClustersMsg stores a pushed list. Rows shift when connections
// come and go, so an open Settings page keeps its cursor on the same item.
func (m *model) handleKubeClustersMsg(msg kubeClustersMsg) tea.Cmd {
	if msg.gen != m.watchGen {
		return nil
	}
	page, _ := m.currentPage.(*settingsPage)
	item := -1
	if page != nil {
		item = page.settingsCursorItem(m)
	}
	var cursorID string
	if isKubeClusterItem(item) {
		if st := m.kubeStatusAt(item - settingsItemKubeBase); st != nil {
			cursorID = st.GetCluster().GetId()
		}
	}
	before := kubeContextKeys(m.kubeStatuses())
	m.kubeClusters = msg.clusters
	if page != nil {
		switch {
		case cursorID != "":
			page.cursorToKubeCluster(m, cursorID)
		case item >= 0:
			page.cursorToItem(m, item)
		}
	}
	// The login plugin rows of Tool Status come from the contexts the
	// connections use; follow a connection added, removed or re-pointed.
	if page != nil && !slices.Equal(before, kubeContextKeys(m.kubeStatuses())) {
		return loadKubePluginsCmd(m.watchGen, m.kubeStatuses())
	}
	return nil
}

// kubeStatusAt is the i-th connection, or nil.
func (m *model) kubeStatusAt(i int) *fleetgrpc.KubeClusterStatus {
	all := m.kubeStatuses()
	if i < 0 || i >= len(all) {
		return nil
	}
	return all[i]
}

// kubeUpsert folds an RPC's answer about one connection into the pushed list
// so the change shows at once. It never replaces what a push already brought
// that is as new: the next push replaces the whole list anyway.
func (m *model) kubeUpsert(st *fleetgrpc.KubeClusterStatus) {
	id := st.GetCluster().GetId()
	if id == "" {
		return
	}
	if m.kubeClusters == nil {
		m.kubeClusters = &fleetgrpc.KubeClusters{}
	}
	for i, cur := range m.kubeClusters.GetClusters() {
		if cur.GetCluster().GetId() != id {
			continue
		}
		if st.GetCluster().GetRevision() > cur.GetCluster().GetRevision() {
			m.kubeClusters.Clusters[i] = st
		}
		return
	}
	m.kubeClusters.Clusters = append(m.kubeClusters.Clusters, st)
}

// kubeForget drops a removed connection from the pushed list at once.
func (m *model) kubeForget(id string) {
	if m.kubeClusters == nil {
		return
	}
	m.kubeClusters.Clusters = slices.DeleteFunc(slices.Clone(m.kubeClusters.Clusters), func(st *fleetgrpc.KubeClusterStatus) bool {
		return st.GetCluster().GetId() == id
	})
}

// kubeContextKeys identifies which contexts the connections read, for
// noticing a change that moves the login plugin rows.
func kubeContextKeys(all []*fleetgrpc.KubeClusterStatus) []string {
	keys := make([]string, 0, len(all))
	for _, st := range all {
		c := st.GetCluster()
		keys = append(keys, c.GetId()+"\x00"+c.GetContext()+"\x00"+strings.Join(c.GetKubeconfigPaths(), "\x00"))
	}
	return keys
}

// resetKube drops everything learned from the daemon being left (an armada
// switch) and stops what is still streaming from it.
func (m *model) resetKube() {
	for _, run := range m.kubeRuns {
		run.cancel()
	}
	for _, run := range m.kubeLogins {
		run.cancel()
	}
	m.kubeClusters = nil
	m.kubeRuns = nil
	m.kubeLogins = nil
	m.kubePlugins = nil
	m.kubeHost = ""
}

// A connection is called (*fleetgrpc.KubeCluster).DisplayNameOrID() in every
// view, and fleetgrpc.KubeClusterState.Usable() decides whether creates can go
// to it, the same rules `fleet cluster` and the MCP cluster tools use.

// kubeStateWords names each state the way a badge reads. It differs from
// KubeClusterState.Display() on purpose ("checking…", "login plugin missing").
var kubeStateWords = map[fleetgrpc.KubeClusterState]string{
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING:        "checking…",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY:           "ready",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED:         "limited",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE:     "unreachable",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED:    "unauthorized",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN:       "forbidden",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING: "context missing",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING:  "login plugin missing",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED:   "misconfigured",
	fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT:  "owner conflict",
}

// kubeStateWord is a state's badge word.
func kubeStateWord(state fleetgrpc.KubeClusterState) string {
	if w, ok := kubeStateWords[state]; ok {
		return w
	}
	return "unknown"
}

// The badge glyphs. Each has one theme slot: ● Success, ▲ and ⚠ Warning,
// ✕ Error; the checking state and running checks are Muted.
const (
	kubeGlyphReady   = "●"
	kubeGlyphLimited = "▲"
	kubeGlyphWarn    = "⚠"
	kubeGlyphFailed  = "✕"
	kubeGlyphPass    = "✓"
	kubeGlyphRunning = "◐"
	kubeGlyphSkipped = "–"
	kubeGlyphCluster = "☸"
)

// kubeBadge is a connection's health as a row shows it: the glyph and word
// for its state, with the daemon's one-line reason where it says more.
func kubeBadge(st *fleetgrpc.KubeClusterStatus) string {
	state := st.GetState()
	msg := strings.TrimSpace(st.GetMessage())
	switch state {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY:
		return kubeOKStyle.Render(kubeGlyphReady + " ready")
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED:
		if msg == "" {
			msg = "limited"
		}
		return kubeWarnStyle.Render(kubeGlyphLimited + " " + msg)
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:
		return dimStyle.Render(kubeGlyphRunning + " checking…")
	}
	text := kubeGlyphFailed + " " + kubeStateWord(state)
	if msg != "" && msg != kubeStateWord(state) {
		text += " (" + msg + ")"
	}
	return kubeErrStyle.Render(text)
}

// kubeBadgeShort is kubeBadge cut to width columns: the glyph and the state
// word always, the reason as far as it fits.
func kubeBadgeShort(st *fleetgrpc.KubeClusterStatus, width int) string {
	state := st.GetState()
	switch state {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED:
		return kubeWarnStyle.Render(ansi.Truncate(kubeGlyphLimited+" "+strings.TrimSpace(st.GetMessage()), width, "…"))
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:
		return kubeBadge(st)
	}
	return kubeErrStyle.Render(kubeGlyphFailed + " " + kubeStateWord(state))
}

// kubeCheckMark is one check's result mark.
func kubeCheckMark(result fleetgrpc.KubeCheckResult) string {
	switch result {
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS:
		return kubeOKStyle.Render(kubeGlyphPass)
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN:
		return kubeWarnStyle.Render(kubeGlyphLimited)
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL:
		return kubeErrStyle.Render(kubeGlyphFailed)
	case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED:
		return dimStyle.Render(kubeGlyphSkipped)
	}
	return dimStyle.Render(kubeGlyphRunning)
}

// kubeCheckLabel is a check's wire name as people read it.
func kubeCheckLabel(name string) string {
	return strings.NewReplacer("admission_debug_caps", "debug caps", "admission_unprivileged", "unprivileged",
		"admission_privileged", "privileged", "_", " ").Replace(name)
}

// kubeCheckTrail renders the last max checks of a test as "✓ kubeconfig ✓
// reachable ◐ rbac", the progress line of a test in flight.
func kubeCheckTrail(checks []*fleetgrpc.KubeCheck, max int) string {
	if len(checks) > max {
		checks = checks[len(checks)-max:]
	}
	parts := make([]string, 0, len(checks))
	for _, c := range checks {
		parts = append(parts, kubeCheckMark(c.GetResult())+" "+kubeCheckLabel(c.GetName()))
	}
	return strings.Join(parts, " ")
}

// kubeCheckSummary is a finished test in brief: how many checks passed,
// then the marks of those that did not.
func kubeCheckSummary(checks []*fleetgrpc.KubeCheck) string {
	passed := 0
	var other []*fleetgrpc.KubeCheck
	for _, c := range checks {
		switch c.GetResult() {
		case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS:
			passed++
		case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED:
		default:
			other = append(other, c)
		}
	}
	out := kubeOKStyle.Render(kubeGlyphPass) + " " + dimStyle.Render(countNoun(passed, "check")+" passed")
	if len(other) > 0 {
		out += "  " + kubeCheckTrail(other, len(other))
	}
	return out
}

// kubeWarningLine is one exposure warning ("⚠ privileged allowed: ...").
func kubeWarningLine(w string) string {
	return kubeWarnStyle.Render(kubeGlyphWarn + " " + w)
}

// kubeCheck is the named check of a status, or nil.
func kubeCheck(st *fleetgrpc.KubeClusterStatus, name string) *fleetgrpc.KubeCheck {
	for _, c := range st.GetChecks() {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

// kubeStatusUsesPlugin reports whether a connection logs in through an exec
// credential plugin, as far as its last preflight tells: the credentials
// check names the plugin ("exec kubelogin …") or offers a login, or the
// plugin is missing. A connection not checked yet may.
func kubeStatusUsesPlugin(st *fleetgrpc.KubeClusterStatus) bool {
	if st.GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING {
		return true
	}
	c := kubeCheck(st, "credentials")
	if c == nil {
		return len(st.GetChecks()) == 0
	}
	return strings.HasPrefix(c.GetDetail(), "exec ") || strings.Contains(c.GetFix(), "login")
}

// kubeNamespaceMissing reports whether the last preflight found the
// connection's namespace absent, which is what offers [ Create ].
func kubeNamespaceMissing(checks []*fleetgrpc.KubeCheck) bool {
	for _, c := range checks {
		if c.GetName() == "namespace" && c.GetResult() == fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL {
			return true
		}
	}
	return false
}

// kubeAgo renders a duration the way the daemon's status lines do: "54m",
// "2h", "3d".
func kubeAgo(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// kubeNow is the clock the expiry and "checked … ago" lines read.
var kubeNow = time.Now

// ===========================================
// Who uses a connection
// ===========================================

// kubeUser is an instance or an automation agent on a connection.
type kubeUser struct {
	fleet, instance, agent string
	status                 string
}

func (u kubeUser) String() string {
	if u.agent != "" {
		return "agent " + u.fleet + "/" + u.agent
	}
	s := u.fleet + "/" + u.instance
	if u.status != "" {
		s += "   " + u.status
	}
	return s
}

// kubeUsers lists what uses connection id, from the state the daemon pushed:
// instances first, then agents, each in fleet order. The daemon re-checks
// when it is asked to remove the connection.
func kubeUsers(m *model, id string) (instances, agents []kubeUser) {
	if m.st == nil || id == "" {
		return nil, nil
	}
	names := make([]string, 0, len(m.st.Fleets))
	for name := range m.st.Fleets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		f := m.st.Fleets[name]
		for _, inst := range f.Instances {
			if inst.KubeCluster == id {
				instances = append(instances, kubeUser{fleet: name, instance: inst.Name, status: string(inst.Status)})
			}
		}
		for _, a := range f.Settings.Agents {
			if a.KubeCluster == id {
				agents = append(agents, kubeUser{fleet: name, agent: a.Name})
			}
		}
	}
	return instances, agents
}

// kubeUsersFromReply converts a RemoveKubeCluster reply's users.
func kubeUsersFromReply(users []*fleetgrpc.KubeClusterUser) (instances, agents []kubeUser) {
	for _, u := range users {
		if u.GetAgent() != "" {
			agents = append(agents, kubeUser{fleet: u.GetFleet(), agent: u.GetAgent()})
			continue
		}
		instances = append(instances, kubeUser{fleet: u.GetFleet(), instance: u.GetInstance(), status: u.GetStatus().Display()})
	}
	return instances, agents
}

// countNoun is "1 instance" / "2 instances".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ===========================================
// Streamed tests (TestKubeCluster)
// ===========================================

// kubeTestRun is one streamed preflight: of a saved connection (Settings
// [ test ], the editor) or of the context picker's unsaved draft.
type kubeTestRun struct {
	key    string // m.kubeRuns key: kubeTestKey(id), or the picker's draft key
	id     string // the saved connection; "" for a draft
	gen    int
	cancel context.CancelFunc
	stream kubeTestStream
	checks []*fleetgrpc.KubeCheck
	result *fleetgrpc.KubeClusterStatus
	err    error
	done   bool
}

// kubeTestMsg is one step of a run: the stream it opened (first message
// only), an event, or the error that ended it (io.EOF: it finished).
type kubeTestMsg struct {
	run    *kubeTestRun
	stream kubeTestStream
	ev     *fleetgrpc.TestKubeClusterEvent
	err    error
}

// kubeTestKey is the run key of a saved connection's test.
func kubeTestKey(id string) string { return "cluster:" + id }

// startKubeTest starts a streamed test under key, cancelling one already
// running there.
func (m *model) startKubeTest(key, id string, req *fleetgrpc.TestKubeClusterRequest) tea.Cmd {
	if old := m.kubeRuns[key]; old != nil {
		old.cancel()
	}
	timeout := kubeTestTimeout
	if req.GetDeep() {
		timeout = kubeDeepTestTimeout
	}
	if len(req.GetStorageClasses()) > 0 || req.GetTestStorage() {
		timeout += kubeLargeStorageTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	run := &kubeTestRun{key: key, id: id, gen: m.watchGen, cancel: cancel}
	if m.kubeRuns == nil {
		m.kubeRuns = map[string]*kubeTestRun{}
	}
	m.kubeRuns[key] = run
	return func() tea.Msg {
		stream, err := openKubeTestRemote(ctx, req)
		if err != nil {
			return kubeTestMsg{run: run, err: err}
		}
		ev, err := stream.Recv()
		return kubeTestMsg{run: run, stream: stream, ev: ev, err: err}
	}
}

// next reads the run's next event.
func (r *kubeTestRun) next() tea.Cmd {
	stream := r.stream
	return func() tea.Msg {
		ev, err := stream.Recv()
		return kubeTestMsg{run: r, ev: ev, err: err}
	}
}

// running reports whether the run is still streaming.
func (r *kubeTestRun) running() bool { return r != nil && !r.done }

// upsertCheck records a check's latest result in first-seen order.
func (r *kubeTestRun) upsertCheck(c *fleetgrpc.KubeCheck) {
	for i, cur := range r.checks {
		if cur.GetName() == c.GetName() {
			r.checks[i] = c
			return
		}
	}
	r.checks = append(r.checks, c)
}

// handleKubeTestMsg applies one step of a run and reads the next.
func (m *model) handleKubeTestMsg(msg kubeTestMsg) tea.Cmd {
	run := msg.run
	if run == nil || m.kubeRuns[run.key] != run || run.gen != m.watchGen || run.done {
		return nil // superseded, cancelled, or from the daemon a switch left
	}
	if msg.stream != nil {
		run.stream = msg.stream
	}
	if msg.err != nil {
		run.done = true
		if !errors.Is(msg.err, io.EOF) {
			run.err = msg.err
		} else if run.result == nil {
			run.err = errors.New("the test ended without a result")
		}
		run.cancel()
		if page, ok := m.currentPage.(*settingsPage); ok {
			return page.onKubeTestDone(m, run)
		}
		return nil
	}
	switch k := msg.ev.GetKind().(type) {
	case *fleetgrpc.TestKubeClusterEvent_Check:
		run.upsertCheck(k.Check)
	case *fleetgrpc.TestKubeClusterEvent_Result:
		run.result = k.Result
		if run.id != "" {
			// A saved connection's result is its new health: show it now
			// rather than at the next push.
			m.kubeReplaceHealth(k.Result)
		}
	}
	return run.next()
}

// kubeReplaceHealth replaces a saved connection's health with a fresh
// result, keeping the pushed record (a test never changes the record).
func (m *model) kubeReplaceHealth(result *fleetgrpc.KubeClusterStatus) {
	for i, cur := range m.kubeStatuses() {
		if cur.GetCluster().GetId() != result.GetCluster().GetId() {
			continue
		}
		next := proto.Clone(result).(*fleetgrpc.KubeClusterStatus)
		next.Cluster = cur.GetCluster()
		m.kubeClusters.Clusters[i] = next
		return
	}
}

// kubeTestError explains a run that ended in an error.
func kubeTestError(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unimplemented:
			return "this fleetd does not support Kubernetes connections"
		case codes.DeadlineExceeded:
			return "the test did not finish in time"
		case codes.Canceled:
			return "cancelled"
		}
		return st.Message()
	}
	return err.Error()
}

// kubeRPCError explains a failed kube RPC in one line.
func kubeRPCError(err error) string {
	if status.Code(err) == codes.Unimplemented {
		return "this fleetd does not support Kubernetes connections"
	}
	return status.Convert(err).Message()
}

// ===========================================
// Login on the daemon (LoginKubeCluster)
// ===========================================

// kubeLoginRun is one login plugin run on the daemon host: it relays only
// the device code the plugin asks the user to enter.
type kubeLoginRun struct {
	id     string
	gen    int
	cancel context.CancelFunc
	stream kubeLoginStream
	code   *fleetgrpc.KubeDeviceCode
	result *fleetgrpc.KubeClusterStatus
	err    error
	done   bool
}

type kubeLoginMsg struct {
	run    *kubeLoginRun
	stream kubeLoginStream
	ev     *fleetgrpc.LoginKubeClusterEvent
	err    error
}

// startKubeLogin runs connection id's login plugin on the daemon.
func (m *model) startKubeLogin(id string) tea.Cmd {
	if old := m.kubeLogins[id]; old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeLoginTimeout)
	run := &kubeLoginRun{id: id, gen: m.watchGen, cancel: cancel}
	if m.kubeLogins == nil {
		m.kubeLogins = map[string]*kubeLoginRun{}
	}
	m.kubeLogins[id] = run
	return func() tea.Msg {
		stream, err := openKubeLoginRemote(ctx, id)
		if err != nil {
			return kubeLoginMsg{run: run, err: err}
		}
		ev, err := stream.Recv()
		return kubeLoginMsg{run: run, stream: stream, ev: ev, err: err}
	}
}

func (r *kubeLoginRun) next() tea.Cmd {
	stream := r.stream
	return func() tea.Msg {
		ev, err := stream.Recv()
		return kubeLoginMsg{run: r, ev: ev, err: err}
	}
}

// handleKubeLoginMsg applies one step of a daemon-side login.
func (m *model) handleKubeLoginMsg(msg kubeLoginMsg) tea.Cmd {
	run := msg.run
	if run == nil || m.kubeLogins[run.id] != run || run.gen != m.watchGen || run.done {
		return nil
	}
	if msg.stream != nil {
		run.stream = msg.stream
	}
	if msg.err != nil {
		run.done = true
		run.cancel()
		if !errors.Is(msg.err, io.EOF) {
			run.err = msg.err
			m.message = "Login failed: " + kubeRPCError(msg.err)
		} else if run.result != nil {
			name := run.result.GetCluster().DisplayNameOrID()
			m.message = fmt.Sprintf("Logged in: %s %s is %s", kubeGlyphCluster, name, kubeStateWord(run.result.GetState()))
		}
		return nil
	}
	switch k := msg.ev.GetKind().(type) {
	case *fleetgrpc.LoginKubeClusterEvent_DeviceCode:
		run.code = k.DeviceCode
	case *fleetgrpc.LoginKubeClusterEvent_Result:
		run.result = k.Result
		m.kubeReplaceHealth(k.Result)
	}
	return run.next()
}

// kubeDeviceCodeLine is what a login in flight asks the user to do.
func kubeDeviceCodeLine(code *fleetgrpc.KubeDeviceCode) string {
	switch {
	case code.GetUrl() != "" && code.GetCode() != "":
		return fmt.Sprintf("open %s and enter the code %s", code.GetUrl(), code.GetCode())
	case code.GetUrl() != "":
		return "open " + code.GetUrl()
	case code.GetCode() != "":
		return "enter the code " + code.GetCode()
	}
	return "waiting for the login plugin…"
}
