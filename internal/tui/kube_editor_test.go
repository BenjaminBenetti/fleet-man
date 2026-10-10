package tui

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// kubeDaemonOwned are the KubeCluster fields only the daemon writes (the
// server refuses them in a mask).
var kubeDaemonOwned = map[string]bool{
	"id": true, "server": true, "cluster_uid": true,
	"created_at": true, "updated_at": true, "verified_at": true, "revision": true,
}

// kubeClientPaths walks the KubeCluster descriptor: every field a client may
// set, as a mask path. A sub-message is walked into (each of its fields is
// its own row); lists, maps and repeated messages are one field.
func kubeClientPaths(md protoreflect.MessageDescriptor, prefix string) []string {
	var out []string
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		name := string(fd.Name())
		if prefix == "" && kubeDaemonOwned[name] {
			continue
		}
		if fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
			out = append(out, kubeClientPaths(fd.Message(), prefix+name+".")...)
			continue
		}
		out = append(out, prefix+name)
	}
	return out
}

// Every field a client can set is in the editor, once, under a valid mask
// path: adding a KubeCluster field without an editor row fails here.
func TestEditorCoversEveryClientSettableField(t *testing.T) {
	want := kubeClientPaths((&fleetgrpc.KubeCluster{}).ProtoReflect().Descriptor(), "")
	var got []string
	for _, f := range kubeAllFields() {
		if kubeFieldDescs(f.path) == nil {
			t.Errorf("editor field %q is not a KubeCluster path", f.path)
		}
		if _, err := fieldmaskpb.New(&fleetgrpc.KubeCluster{}, f.path); err != nil {
			t.Errorf("editor field %q is not a valid mask path: %v", f.path, err)
		}
		got = append(got, f.path)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		var missing, extra []string
		for _, p := range want {
			if !slices.Contains(got, p) {
				missing = append(missing, p)
			}
		}
		seen := map[string]bool{}
		for _, p := range got {
			if !slices.Contains(want, p) || seen[p] {
				extra = append(extra, p)
			}
			seen[p] = true
		}
		t.Fatalf("editor fields do not match the client-settable KubeCluster fields\nmissing: %v\nextra or duplicated: %v", missing, extra)
	}
}

// Each kind's text form goes to the wire and back: the source carries only
// the field it names, set as typed, and an empty value clears a defaulted
// field (an unset source field is what the daemon reads as "clear").
func TestEditorFieldTextRoundTrips(t *testing.T) {
	byPath := map[string]kubeField{}
	for _, f := range kubeAllFields() {
		byPath[f.path] = f
	}
	for _, tc := range []struct {
		path, text, want string
	}{
		{"display_name", "prod-eu", "prod-eu"},
		{"disk_size", " 100Gi ", "100Gi"},
		{"disk_size", "", ""},
		{"clone_method", "csi-clone", "csi-clone"},
		{"clone_method", "auto", ""},
		{"auto_expand_disk", "off", "off"},
		{"auto_expand_disk", "", ""},
		{"allow_privileged", "on", "on"},
		{"backup_workspace_on_stop", "on", "on"},
		{"max_instances", "20", "20"},
		{"automation_max_per_agent", "0", "0"},
		{"automation_max_per_agent", "", ""},
		{"image.mode", "gateway", "gateway"},
		{"image.pull_secrets", "a, b,,c", "a, b, c"},
		{"image.upload_pace_percent", "50", "50"},
		{"cache_sizes.buildkit", "40Gi", "40Gi"},
		{"resources.cpu_request", "1", "1"},
		{"service_images.dind", "docker:28-dind", "docker:28-dind"},
		{"extra_path", "/opt/aws/bin:/usr/local/bin", "/opt/aws/bin:/usr/local/bin"},
		{"kubeconfig_paths", "/a/config", "/a/config"},
		{"plugin_env", "AWS_PROFILE, AWS_REGION=eu-west-1", "AWS_PROFILE, AWS_REGION=eu-west-1"},
		{"node_selector", "pool=fleet, zone = b", "pool=fleet, zone=b"},
		{"tolerations", "dedicated=fleet:NoSchedule, gpu:NoExecute@300, *", "dedicated=fleet:NoSchedule, gpu:NoExecute@300, *"},
		{"pod_overrides_yaml", "priorityClassName: low", "priorityClassName: low\n"},
	} {
		f := byPath[tc.path]
		src, err := f.source(tc.text)
		if err != nil {
			t.Errorf("%s %q: %v", tc.path, tc.text, err)
			continue
		}
		if got := f.text(src); got != tc.want {
			t.Errorf("%s %q reads back %q, want %q", tc.path, tc.text, got, tc.want)
		}
		// Nothing else is set in the source.
		var set []string
		var walk func(protoreflect.Message, string)
		walk = func(msg protoreflect.Message, prefix string) {
			msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				name := prefix + string(fd.Name())
				if fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
					walk(v.Message(), name+".")
					return true
				}
				set = append(set, name)
				return true
			})
		}
		walk(src.ProtoReflect(), "")
		if len(set) > 1 || (len(set) == 1 && set[0] != tc.path) {
			t.Errorf("%s: the source sets %v", tc.path, set)
		}
	}
	for _, tc := range []struct{ path, text string }{
		{"clone_method", "fast"}, {"max_instances", "lots"}, {"node_selector", "nokey"}, {"tolerations", "=v"}, {"auto_expand_disk", "maybe"},
	} {
		if _, err := byPath[tc.path].source(tc.text); err == nil {
			t.Errorf("%s %q must be refused", tc.path, tc.text)
		}
	}
}

// openEditor opens the editor of connection id on the Settings page.
func openEditor(m *model, sp *settingsPage, id string) *kubeEditor {
	e := newKubeEditor(id)
	sp.kubeDialog = e
	return e
}

// editorRowOf puts the cursor on the field with path.
func editorRowOf(t *testing.T, e *kubeEditor, path string) {
	t.Helper()
	for i, row := range e.rows() {
		if row.field != nil && row.field.path == path {
			e.row = i
			return
		}
	}
	t.Fatalf("no editor row for %s (group folded?)", path)
}

// savedStatus answers an update by applying it to st the way the daemon's
// mask does (a set field replaces, an unset one clears) and bumping the
// revision.
func savedStatus(st *fleetgrpc.KubeClusterStatus) func(kubeUpdateCall) (*fleetgrpc.KubeClusterStatus, error) {
	return func(c kubeUpdateCall) (*fleetgrpc.KubeClusterStatus, error) {
		next := proto.Clone(st).(*fleetgrpc.KubeClusterStatus)
		for _, path := range c.paths {
			descs := kubeFieldDescs(path)
			dst, src := next.Cluster.ProtoReflect(), c.cluster.ProtoReflect()
			for _, fd := range descs[:len(descs)-1] {
				dst, src = dst.Mutable(fd).Message(), src.Get(fd).Message()
			}
			leaf := descs[len(descs)-1]
			if src.Has(leaf) {
				dst.Set(leaf, src.Get(leaf))
			} else {
				dst.Clear(leaf)
			}
		}
		next.Cluster.Revision = c.rev + 1
		return next, nil
	}
}

// Allow privileged cycles auto → on → off; each step is one masked update
// carrying the revision the editor read, and turning it on says the node
// becomes the boundary (the warning also sits beside the toggle).
func TestEditorAllowPrivilegedSavesWithMaskAndRevision(t *testing.T) {
	st := kubeSt("eu-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED)
	m, sp := kubeTestModel(t, st)
	calls := stubKubeUpdate(t, savedStatus(st))
	e := openEditor(m, sp, "eu-1")
	editorRowOf(t, e, "allow_privileged")
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "Allow privileged: [ auto → off ]") || !strings.Contains(out, "⚠ "+kubePrivilegedWarning) {
		t.Fatalf("the toggle must show its resolved auto and the node warning:\n%s", out)
	}

	pumpKube(t, m, press(m, sp, "right"))
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	c := (*calls)[0]
	if c.id != "eu-1" || !slices.Equal(c.paths, []string{"allow_privileged"}) || c.rev != 3 || c.cluster.AllowPrivileged == nil || !c.cluster.GetAllowPrivileged() {
		t.Fatalf("update = %+v, want allow_privileged=true under its own mask at revision 3", c)
	}
	if !strings.Contains(m.message, "node becomes the isolation boundary") {
		t.Fatalf("message = %q", m.message)
	}
	if got := m.kubeStatus("eu-1").GetCluster(); !got.GetAllowPrivileged() || got.GetRevision() != 4 {
		t.Fatal("the saved record must show at once")
	}
	pumpKube(t, m, press(m, sp, "l"))
	pumpKube(t, m, press(m, sp, "l"))
	if got := (*calls)[2]; got.cluster.AllowPrivileged != nil || got.rev != 5 {
		t.Fatalf("third step must clear it back to auto: %+v", got)
	}
}

// A remote TUI, over the gateway or Armada SSH, edits every field exactly as
// a local one: the same rows, and the same requests for extra_path and
// allow_privileged.
func TestEditorRemoteEditsLikeLocal(t *testing.T) {
	type sent struct {
		rows  int
		calls []kubeUpdateCall
	}
	run := func(t *testing.T, env, value string) sent {
		st := kubeSt("eu-1", "prod-eu", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
		m, sp := kubeTestModel(t, st)
		if env != "" {
			t.Setenv(env, value)
		}
		calls := stubKubeUpdate(t, savedStatus(st))
		e := openEditor(m, sp, "eu-1")
		for _, g := range kubeGroups {
			e.open[g.key] = true
		}
		editorRowOf(t, e, "extra_path")
		press(m, sp, "enter")
		e.input.SetValue("/opt/aws/bin")
		pumpKube(t, m, press(m, sp, "enter"))
		editorRowOf(t, e, "allow_privileged")
		pumpKube(t, m, press(m, sp, "right"))
		return sent{rows: len(e.rows()), calls: *calls}
	}
	local := run(t, "", "")
	if len(local.calls) != 2 {
		t.Fatalf("local sent %d updates", len(local.calls))
	}
	for _, remote := range []struct{ name, env, value string }{
		{"gateway", fleetclient.EnvGateway, "https://gw.example/s"},
		{"armada ssh", fleetclient.EnvSSH, "ssh://ben@desk"},
	} {
		t.Run(remote.name, func(t *testing.T) {
			got := run(t, remote.env, remote.value)
			if got.rows != local.rows {
				t.Fatalf("remote editor has %d rows, local %d: no field may be hidden remotely", got.rows, local.rows)
			}
			for i := range local.calls {
				l, r := local.calls[i], got.calls[i]
				if l.id != r.id || l.rev != r.rev || !slices.Equal(l.paths, r.paths) || !proto.Equal(l.cluster, r.cluster) {
					t.Fatalf("remote update %d = %+v, local %+v", i, r, l)
				}
			}
		})
	}
}

// Text fields edit in place: enter opens the input on the current value,
// enter saves, esc discards; a value the field refuses never leaves the TUI.
func TestEditorTextFieldEditsInPlace(t *testing.T) {
	st := kubeSt("mk-1", "minikube", 2)
	m, sp := kubeTestModel(t, st)
	calls := stubKubeUpdate(t, savedStatus(st))
	e := openEditor(m, sp, "mk-1")
	editorRowOf(t, e, "disk_size")

	press(m, sp, "enter")
	if e.editing == nil || e.editing.path != "disk_size" {
		t.Fatal("enter must start editing")
	}
	e.input.SetValue("100Gi")
	press(m, sp, "esc")
	if e.editing != nil || len(*calls) != 0 {
		t.Fatal("esc must discard the edit")
	}
	press(m, sp, "enter")
	e.input.SetValue("100Gi")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*calls) != 1 || (*calls)[0].cluster.GetDiskSize() != "100Gi" || !slices.Equal((*calls)[0].paths, []string{"disk_size"}) {
		t.Fatalf("calls = %+v", *calls)
	}

	e.open["resources"] = true
	editorRowOf(t, e, "max_instances")
	press(m, sp, "enter")
	e.input.SetValue("many")
	pumpKube(t, m, press(m, sp, "enter"))
	if len(*calls) != 1 || !strings.Contains(e.fieldErr["max_instances"], "whole number") {
		t.Fatalf("a bad value must be refused locally: calls=%d err=%q", len(*calls), e.fieldErr["max_instances"])
	}
}

// A save the daemon refuses because the record moved meanwhile (ABORTED on
// the revision) says so on the row; other refusals show their reason.
func TestEditorRefusedSavesExplain(t *testing.T) {
	st := kubeSt("mk-1", "minikube", 2)
	m, sp := kubeTestModel(t, st)
	stubKubeUpdate(t, func(c kubeUpdateCall) (*fleetgrpc.KubeClusterStatus, error) {
		if c.paths[0] == "namespace" {
			return nil, status.Error(codes.FailedPrecondition, `connection "minikube" is in use (1 instance): its namespace cannot change`)
		}
		return nil, status.Error(codes.Aborted, "connection changed since it was read")
	})
	e := openEditor(m, sp, "mk-1")
	editorRowOf(t, e, "clone_method")
	pumpKube(t, m, press(m, sp, "right"))
	if !strings.Contains(e.fieldErr["clone_method"], "changed elsewhere") || e.saving["clone_method"] {
		t.Fatalf("err = %q", e.fieldErr["clone_method"])
	}
	editorRowOf(t, e, "namespace")
	press(m, sp, "enter")
	e.input.SetValue("other")
	pumpKube(t, m, press(m, sp, "enter"))
	if !strings.Contains(e.fieldErr["namespace"], "its namespace cannot change") {
		t.Fatalf("err = %q", e.fieldErr["namespace"])
	}
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "✕ connection \"minikube\" is in use") {
		t.Fatalf("the refusal must show on the row:\n%s", out)
	}
}

// Groups fold and unfold with enter and ←/→; the Status row's [ test ] runs
// the preflight and [ login on fleetd ] the daemon-side login, whose device
// code shows in the editor.
func TestEditorGroupsStatusAndLoginOnDaemon(t *testing.T) {
	st := kubeSt("aks-1", "aks", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED)
	m, sp := kubeTestModel(t, st)
	e := openEditor(m, sp, "aks-1")
	for i, row := range e.rows() {
		if row.kind == kubeEditorRowGroup && row.group.key == "caches" {
			e.row = i
		}
	}
	before := len(e.rows())
	press(m, sp, "right")
	if !e.open["caches"] || len(e.rows()) <= before {
		t.Fatal("→ on a folded group must unfold it")
	}
	press(m, sp, "enter")
	if e.open["caches"] {
		t.Fatal("enter on an open group folds it")
	}

	e.row = len(kubeTopFields) // Status
	stubKubeTest(t, func(*fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
		return testEvents(kubeSt("aks-1", "aks", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED))
	})
	pumpKube(t, m, press(m, sp, "enter"))
	if run := m.kubeRuns[kubeTestKey("aks-1")]; run == nil || run.running() {
		t.Fatal("enter on Status must run the test")
	}

	codeSent := make(chan struct{})
	origLogin := openKubeLoginRemote
	openKubeLoginRemote = func(_ context.Context, id string) (kubeLoginStream, error) {
		return &fakeLoginStream{id: id, sent: codeSent}, nil
	}
	t.Cleanup(func() { openKubeLoginRemote = origLogin })
	press(m, sp, "l")
	press(m, sp, "l")
	if e.button != 2 {
		t.Fatalf("button = %d, want [ login on fleetd ]", e.button)
	}
	cmd := press(m, sp, "enter")
	msg := cmd()
	m.handleKubeLoginMsg(msg.(kubeLoginMsg))
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "login on fleetd: open https://microsoft.com/devicelogin and enter the code ABCD-1234") {
		t.Fatalf("the device code must show:\n%s", out)
	}
	close(codeSent)
	pumpKube(t, m, m.kubeLogins["aks-1"].next())
	if !strings.Contains(m.message, "Logged in: ☸ aks is ready") || m.kubeStatus("aks-1").GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Fatalf("message = %q state = %v", m.message, m.kubeStatus("aks-1").GetState())
	}
}

// fakeLoginStream sends a device code, then (once sent is closed) a READY
// result, then io.EOF.
type fakeLoginStream struct {
	id   string
	sent chan struct{}
	step int
}

func (s *fakeLoginStream) Recv() (*fleetgrpc.LoginKubeClusterEvent, error) {
	s.step++
	switch s.step {
	case 1:
		return &fleetgrpc.LoginKubeClusterEvent{Kind: &fleetgrpc.LoginKubeClusterEvent_DeviceCode{
			DeviceCode: &fleetgrpc.KubeDeviceCode{Url: "https://microsoft.com/devicelogin", Code: "ABCD-1234"}}}, nil
	case 2:
		<-s.sent
		return &fleetgrpc.LoginKubeClusterEvent{Kind: &fleetgrpc.LoginKubeClusterEvent_Result{
			Result: kubeSt(s.id, "aks", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)}}, nil
	}
	return nil, io.EOF
}

// The Namespace row offers [ Create ] when the last preflight found the
// namespace missing, through l or c; Context opens the picker on a re-point
// and esc there comes back to the editor.
func TestEditorNamespaceCreateAndContextRepoint(t *testing.T) {
	st := kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED)
	st.Checks = []*fleetgrpc.KubeCheck{{Name: "namespace", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "namespace 'fleet' does not exist"}}
	m, sp := kubeTestModel(t, st)
	var nsReqs []*fleetgrpc.CreateKubeNamespaceRequest
	origNS := createKubeNamespaceRemote
	createKubeNamespaceRemote = func(req *fleetgrpc.CreateKubeNamespaceRequest) (*fleetgrpc.CreateKubeNamespaceReply, error) {
		nsReqs = append(nsReqs, req)
		return &fleetgrpc.CreateKubeNamespaceReply{Manifest: "kind: Namespace\n"}, nil
	}
	t.Cleanup(func() { createKubeNamespaceRemote = origNS })
	e := openEditor(m, sp, "mk-1")
	editorRowOf(t, e, "namespace")
	if out := flatText(sp.viewSettings(m)); !strings.Contains(out, "✕ namespace 'fleet' does not exist [ Create ]") || !strings.Contains(out, "[c] create namespace") {
		t.Fatalf("namespace row:\n%s", out)
	}
	press(m, sp, "l")
	pumpKube(t, m, press(m, sp, "enter"))
	if e.ns == nil || len(nsReqs) != 1 || nsReqs[0].GetId() != "mk-1" || !nsReqs[0].GetDryRun() || nsReqs[0].GetNamespace() != "fleet" {
		t.Fatalf("[ Create ] must dry-run the saved connection's namespace: %v", nsReqs)
	}
	press(m, sp, "esc")
	if e.ns != nil {
		t.Fatal("esc closes the panel")
	}
	pumpKube(t, m, press(m, sp, "c"))
	if e.ns == nil {
		t.Fatal("c opens it too")
	}
	press(m, sp, "esc")

	stubKubeContexts(t, pickerContexts())
	editorRowOf(t, e, "context")
	pumpKube(t, m, press(m, sp, "enter"))
	if p, ok := sp.kubeDialog.(*kubePicker); !ok || p.repoint != "mk-1" {
		t.Fatalf("Context opened %T", sp.kubeDialog)
	}
	press(m, sp, "esc")
	if sp.kubeDialog != e {
		t.Fatalf("esc in the re-point picker must return to the editor, got %T", sp.kubeDialog)
	}
}

// [L] Run login here is offered on a connection that logs in through a
// plugin (and only over the local socket): not on one with a client cert.
func TestEditorLoginHereFollowsTheCredential(t *testing.T) {
	for _, tc := range []struct {
		detail string
		state  fleetgrpc.KubeClusterState
		want   bool
	}{
		{"exec kubelogin (expires in 54m)", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, true},
		{"client certificate (expires in 1094d)", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, false},
		{"", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING, true},
	} {
		st := kubeSt("mk-1", "minikube", tc.state)
		if tc.detail != "" {
			st.Checks = []*fleetgrpc.KubeCheck{{Name: "credentials", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: tc.detail}}
		}
		m, sp := kubeTestModel(t, st)
		e := openEditor(m, sp, "mk-1")
		if got := strings.Contains(e.footer(m, st), "[L] login here"); got != tc.want {
			t.Errorf("%q/%v: [L] offered = %v, want %v", tc.detail, tc.state, got, tc.want)
		}
	}
}

// Pod overrides edit in $EDITOR: unchanged text saves nothing, changed text
// saves under its own mask.
func TestEditorPodOverridesInExternalEditor(t *testing.T) {
	st := kubeSt("mk-1", "minikube", 2)
	m, sp := kubeTestModel(t, st)
	calls := stubKubeUpdate(t, savedStatus(st))
	e := openEditor(m, sp, "mk-1")
	f := kubeField{}
	for _, field := range kubeAllFields() {
		if field.path == "pod_overrides_yaml" {
			f = field
		}
	}
	sp.Update(m, kubeYAMLEditedMsg{editor: e, field: f, before: "", text: "\n", rev: 3})
	if len(*calls) != 0 || !strings.Contains(m.message, "unchanged") {
		t.Fatalf("an unchanged file must save nothing: %d calls, %q", len(*calls), m.message)
	}
	pumpKube(t, m, sp.Update(m, kubeYAMLEditedMsg{editor: e, field: f, before: "", text: "priorityClassName: low\n", rev: 3}))
	if len(*calls) != 1 || (*calls)[0].cluster.GetPodOverridesYaml() != "priorityClassName: low\n" || !slices.Equal((*calls)[0].paths, []string{"pod_overrides_yaml"}) {
		t.Fatalf("calls = %+v", *calls)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "code --wait")
	if cmd := kubeExternalEditor("/tmp/x.yaml"); !slices.Equal(cmd.Args, []string{"sh", "-c", `code --wait "$1"`, "sh", "/tmp/x.yaml"}) {
		t.Fatalf("editor command = %v", cmd.Args)
	}
}

func TestTolerationText(t *testing.T) {
	for _, s := range []string{"dedicated=fleet:NoSchedule", "gpu:NoExecute@300", "*", "key", "k=v", "*:NoSchedule"} {
		tol, err := parseToleration(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := formatToleration(tol); got != s {
			t.Errorf("%q formats back as %q", s, got)
		}
	}
	tol, _ := parseToleration("gpu:NoExecute@300")
	if tol.GetKey() != "gpu" || tol.GetOperator() != "Exists" || tol.GetEffect() != "NoExecute" || tol.GetTolerationSeconds() != 300 {
		t.Fatalf("parsed %v", tol)
	}
}
