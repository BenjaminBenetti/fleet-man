package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// kube_editor_fields.go is the cluster editor's table of every KubeCluster
// field a client sets: its FieldMask path, how it is shown and edited, and
// what unset means. The editor reads and writes the record by these paths
// through protoreflect, so each save is an UpdateKubeCluster whose mask names
// exactly the field that changed. A test walks the KubeCluster descriptor and
// fails when a client-settable field is missing here.

// kubeFieldKind is how a field is shown and edited.
type kubeFieldKind int

const (
	kubeKindText        kubeFieldKind = iota // a string; empty = the default
	kubeKindChoice                           // one of options (a string enum; "" = auto); ←/→ cycles
	kubeKindClass                            // a class from the cluster's list, or a typed name
	kubeKindBool                             // a bool; ←/→/enter toggles
	kubeKindTri                              // an optional bool: unset (the default) → on → off
	kubeKindInt                              // an int32; 0 = none
	kubeKindOptInt                           // an optional int32; empty = the default
	kubeKindList                             // repeated strings, comma-separated
	kubeKindPaths                            // repeated paths, separated like $PATH
	kubeKindMap                              // a string map: "key=value, key=value"
	kubeKindTolerations                      // tolerations: "key=value:Effect@seconds, key:Effect, *"
	kubeKindYAML                             // multi-line YAML, edited in $EDITOR
	kubeKindContext                          // the context: picked in the context picker (a re-point)
)

// kubeClassSource names which of the cluster's lists a class field offers.
type kubeClassSource int

const (
	kubeClassStorage kubeClassSource = iota + 1
	kubeClassSnapshot
	kubeClassRuntime
)

// kubeField is one editable KubeCluster field.
type kubeField struct {
	path    string // the proto path, as the FieldMask names it
	label   string
	kind    kubeFieldKind
	options []string        // kubeKindChoice values, "" (auto) first
	classes kubeClassSource // kubeKindClass: the list it offers
	def     string          // what unset means, shown dim
	note    string          // a hint shown beside the value
}

// kubeGroup is a titled, foldable set of fields.
type kubeGroup struct {
	key   string
	title string
	// open: expanded when the editor opens. Only Capabilities is, so the
	// editor opens on the connection's health and one line per group.
	open   bool
	fields []kubeField
}

// kubeTopFields are always shown, above the groups.
var kubeTopFields = []kubeField{
	{path: "display_name", label: "Name", kind: kubeKindText, def: "the connection id"},
	{path: "context", label: "Context", kind: kubeKindContext},
	{path: "kubeconfig_paths", label: "Kubeconfig", kind: kubeKindPaths, note: "files on fleetd's host"},
	{path: "namespace", label: "Namespace", kind: kubeKindText},
}

// kubeGroups are the editor's groups, in order. Capabilities has no fields:
// it is the last preflight's findings.
var kubeGroups = []kubeGroup{
	{key: "capabilities", title: "Capabilities", open: true},
	{key: "storage", title: "Storage", fields: []kubeField{
		{path: "shared_storage_class", label: "Shared storage", kind: kubeKindClass, classes: kubeClassStorage, def: "auto",
			note: "the fleets' shares; a new class moves each fleet's share once it is stopped"},
		{path: "share_size", label: "Share size", kind: kubeKindText, def: "10Gi"},
		{path: "share_retention", label: "Share on destroy", kind: kubeKindChoice, options: []string{"", "retain"}, def: "delete"},
		{path: "storage_class", label: "Instance disks", kind: kubeKindClass, classes: kubeClassStorage, def: "auto"},
		{path: "disk_size", label: "Disk size", kind: kubeKindText, def: "50Gi", note: "each, automation too"},
		{path: "auto_expand_disk", label: "Auto-expand disks", kind: kubeKindTri, def: "on", note: "+50% at 90% full"},
		{path: "snapshot_class", label: "Snapshot class", kind: kubeKindClass, classes: kubeClassSnapshot, def: "auto"},
		{path: "clone_method", label: "Clone method", kind: kubeKindChoice, options: []string{"", "snapshot", "csi-clone", "copy"}, def: "auto"},
		{path: "clone_consistency", label: "Clone consistency", kind: kubeKindChoice, options: []string{"", "stop-source"}, def: "auto",
			note: "stop-source scales the source to 0 for the final copy"},
		{path: "backup_workspace_on_stop", label: "Back up on stop", kind: kubeKindBool, note: "export the workspace to fleetd's host"},
		{path: "node_local_ack", label: "Node-local OK", kind: kubeKindBool, note: "accept losing a disk with its node"},
	}},
	{key: "caches", title: "Caches", fields: []kubeField{
		{path: "cache_storage_class", label: "Cache storage", kind: kubeKindClass, classes: kubeClassStorage, def: "auto"},
		{path: "cache_sizes.debcache", label: "apt cache", kind: kubeKindText, def: "10Gi"},
		{path: "cache_sizes.imagecache", label: "Image cache", kind: kubeKindText, def: "20Gi"},
		{path: "cache_sizes.buildkit", label: "Buildkit cache", kind: kubeKindText, def: "20Gi"},
		{path: "allow_shared_buildkit", label: "Shared Buildkit", kind: kubeKindTri, def: "allowed",
			note: "a permission: each fleet's Buildkit toggle decides"},
		{path: "delete_cache_volumes_on_destroy", label: "Delete on destroy", kind: kubeKindBool, note: "else kept for a warm restart"},
		{path: "dind_address_pool", label: "DinD address pool", kind: kubeKindText, def: "docker's",
			note: "a CIDR (/24 networks) or base=<cidr>,size=<bits>"},
		{path: "block_metadata", label: "Block metadata", kind: kubeKindTri, def: "on", note: "pods cannot reach 169.254.0.0/16"},
		{path: "service_images.buildkit_rootless", label: "Buildkit image", kind: kubeKindText, def: "pinned default"},
		{path: "service_images.buildkit", label: "Rootful Buildkit", kind: kubeKindText, def: "pinned default"},
		{path: "service_images.debcache", label: "apt cache image", kind: kubeKindText, def: "pinned default"},
		{path: "service_images.imagecache", label: "Image cache image", kind: kubeKindText, def: "pinned default"},
		{path: "service_images.dind", label: "DinD image", kind: kubeKindText, def: "pinned default",
			note: "a docker:<version>-dind image, for docker.sock binds"},
	}},
	{key: "resources", title: "Resources & quotas", fields: []kubeField{
		{path: "resources.cpu_request", label: "CPU request", kind: kubeKindText, def: "500m"},
		{path: "resources.memory_request", label: "Memory request", kind: kubeKindText, def: "2Gi"},
		{path: "resources.autopilot_memory", label: "Autopilot memory", kind: kubeKindText, def: "4Gi"},
		{path: "memory_limit", label: "Memory limit", kind: kubeKindText, def: "8Gi", note: "set when a LimitRange injects less"},
		{path: "max_instances", label: "Max instances", kind: kubeKindInt, def: "no limit"},
		{path: "max_total_disk", label: "Max total disk", kind: kubeKindText, def: "no limit"},
		{path: "automation_fire_interval", label: "Trigger interval", kind: kubeKindText, def: "30s", note: "per trigger; 0 = off"},
		{path: "automation_max_per_agent", label: "Runs per agent", kind: kubeKindOptInt, def: "4", note: "0 = off; extra runs queue"},
	}},
	{key: "image", title: "Image", fields: []kubeField{
		{path: "image.mode", label: "Delivery", kind: kubeKindChoice, options: []string{"", "node-load", "registry", "gateway"}, def: "auto"},
		{path: "image.registry", label: "Registry", kind: kubeKindText, def: "none"},
		{path: "image.insecure_registry", label: "Insecure registry", kind: kubeKindBool},
		{path: "image.pull_secrets", label: "Pull secrets", kind: kubeKindList, def: "none"},
		{path: "image.managed_pull_secret", label: "Managed secret", kind: kubeKindTri, def: "on", note: "from the registry login on fleetd's host"},
		{path: "image.builder", label: "Builder", kind: kubeKindText, def: "default"},
		{path: "image.platform", label: "Platform", kind: kubeKindText, def: "the nodes'"},
		{path: "image.retention", label: "Retention", kind: kubeKindText, def: "72h"},
		{path: "image.upload_pace_percent", label: "Upload pace %", kind: kubeKindOptInt, def: "80", note: "0 = unpaced"},
	}},
	{key: "security", title: "Security", fields: []kubeField{
		{path: "allow_privileged", label: "Allow privileged", kind: kubeKindTri, def: "auto"},
		{path: "debug_capabilities", label: "Debug caps", kind: kubeKindChoice, options: []string{"", "require"}, def: "auto",
			note: "auto: drop SYS_PTRACE/seccomp with a warning when denied"},
		{path: "host_gateway", label: "Host gateway", kind: kubeKindChoice, options: []string{"", "off"}, def: "auto"},
		{path: "runtime_class", label: "Runtime class", kind: kubeKindClass, classes: kubeClassRuntime, def: "the cluster's"},
		{path: "service_account", label: "Service account", kind: kubeKindText, def: "namespace default"},
		{path: "protect_from_drain", label: "Protect from drain", kind: kubeKindBool, note: "a PodDisruptionBudget per instance"},
	}},
	{key: "advanced", title: "Advanced", fields: []kubeField{
		{path: "pod_overrides_yaml", label: "Pod overrides", kind: kubeKindYAML, def: "none"},
		{path: "extra_path", label: "Extra PATH", kind: kubeKindPaths, def: "none", note: "for login plugins"},
		{path: "plugin_env", label: "Plugin env", kind: kubeKindList, note: "NAME passes fleetd's value, NAME=value sets it"},
		{path: "node_selector", label: "Node selector", kind: kubeKindMap, def: "none"},
		{path: "tolerations", label: "Tolerations", kind: kubeKindTolerations, def: "none"},
		{path: "rebuild_export", label: "Rebuild export", kind: kubeKindChoice, options: []string{"", "off"}, def: "auto",
			note: "auto: a rebuild builds from the instance's workspace; off: from the daemon host's copy as it is"},
	}},
}

// kubePrivilegedWarning goes beside Allow privileged.
const kubePrivilegedWarning = "the node becomes the isolation boundary: give these pods a dedicated node pool (node selector + tolerations)"

// kubeAllFields is every editable field, top fields first.
func kubeAllFields() []kubeField {
	out := append([]kubeField(nil), kubeTopFields...)
	for _, g := range kubeGroups {
		out = append(out, g.fields...)
	}
	return out
}

// ===========================================
// Reading and writing a field by path
// ===========================================

// kubeFieldDescs resolves a mask path to the descriptors along it.
func kubeFieldDescs(path string) []protoreflect.FieldDescriptor {
	md := (&fleetgrpc.KubeCluster{}).ProtoReflect().Descriptor()
	var out []protoreflect.FieldDescriptor
	for _, part := range strings.Split(path, ".") {
		if md == nil {
			return nil
		}
		fd := md.Fields().ByName(protoreflect.Name(part))
		if fd == nil {
			return nil
		}
		out = append(out, fd)
		md = fd.Message()
	}
	return out
}

// kubeFieldValue reads path from c: its value and whether it is set. An
// unset sub-message reads as an empty one, so its fields read unset.
func kubeFieldValue(c *fleetgrpc.KubeCluster, path string) (protoreflect.Value, bool) {
	descs := kubeFieldDescs(path)
	msg := c.ProtoReflect()
	for _, fd := range descs[:len(descs)-1] {
		msg = msg.Get(fd).Message()
	}
	leaf := descs[len(descs)-1]
	return msg.Get(leaf), msg.Has(leaf)
}

// kubeFieldSource is an UpdateKubeCluster source carrying only path, set by
// set (nil leaves it unset, which the daemon reads as "clear it").
func kubeFieldSource(path string, set func(protoreflect.Message, protoreflect.FieldDescriptor)) *fleetgrpc.KubeCluster {
	src := &fleetgrpc.KubeCluster{}
	descs := kubeFieldDescs(path)
	msg := src.ProtoReflect()
	for _, fd := range descs[:len(descs)-1] {
		msg = msg.Mutable(fd).Message()
	}
	if set != nil {
		set(msg, descs[len(descs)-1])
	}
	return src
}

// ===========================================
// Text form of each kind
// ===========================================

// text is the field's value as the editor's input shows it ("" = unset).
func (f kubeField) text(c *fleetgrpc.KubeCluster) string {
	v, has := kubeFieldValue(c, f.path)
	switch f.kind {
	case kubeKindBool:
		return onOff(v.Bool())
	case kubeKindTri:
		if !has {
			return ""
		}
		return onOff(v.Bool())
	case kubeKindInt:
		return strconv.FormatInt(v.Int(), 10)
	case kubeKindOptInt:
		if !has {
			return ""
		}
		return strconv.FormatInt(v.Int(), 10)
	case kubeKindList, kubeKindPaths:
		list := v.List()
		parts := make([]string, list.Len())
		for i := range list.Len() {
			parts[i] = list.Get(i).String()
		}
		sep := ", "
		if f.kind == kubeKindPaths {
			sep = kubePathSeparator
		}
		return strings.Join(parts, sep)
	case kubeKindMap:
		var parts []string
		v.Map().Range(func(k protoreflect.MapKey, val protoreflect.Value) bool {
			parts = append(parts, k.String()+"="+val.String())
			return true
		})
		slices.Sort(parts)
		return strings.Join(parts, ", ")
	case kubeKindTolerations:
		list := v.List()
		parts := make([]string, list.Len())
		for i := range list.Len() {
			parts[i] = formatToleration(list.Get(i).Message().Interface().(*fleetgrpc.KubeToleration))
		}
		return strings.Join(parts, ", ")
	}
	return v.String()
}

// kubePathSeparator separates a path list, as in $PATH and $KUBECONFIG.
const kubePathSeparator = ":"

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// source parses text into an Update source for the field. Empty text clears
// a field whose unset value is a default.
func (f kubeField) source(text string) (*fleetgrpc.KubeCluster, error) {
	text = strings.TrimSpace(text)
	switch f.kind {
	case kubeKindText, kubeKindClass, kubeKindContext:
		if text == "" {
			return kubeFieldSource(f.path, nil), nil
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			m.Set(fd, protoreflect.ValueOfString(text))
		}), nil
	case kubeKindYAML:
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			if text != "" {
				m.Set(fd, protoreflect.ValueOfString(text+"\n"))
			}
		}), nil
	case kubeKindChoice:
		if !slices.Contains(f.options, text) && text != "auto" {
			return nil, fmt.Errorf("%s: one of %s", f.label, strings.Join(f.choiceNames(), ", "))
		}
		if text == "auto" || text == "" {
			return kubeFieldSource(f.path, nil), nil
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			m.Set(fd, protoreflect.ValueOfString(text))
		}), nil
	case kubeKindBool, kubeKindTri:
		if text == "" && f.kind == kubeKindTri {
			return kubeFieldSource(f.path, nil), nil
		}
		on, err := parseOnOff(text)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f.label, err)
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			m.Set(fd, protoreflect.ValueOfBool(on))
		}), nil
	case kubeKindInt, kubeKindOptInt:
		if text == "" {
			return kubeFieldSource(f.path, nil), nil
		}
		n, err := strconv.ParseInt(text, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%s: a whole number", f.label)
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			m.Set(fd, protoreflect.ValueOfInt32(int32(n)))
		}), nil
	case kubeKindList, kubeKindPaths:
		sep := ","
		if f.kind == kubeKindPaths {
			sep = kubePathSeparator
		}
		var items []string
		for _, part := range strings.Split(text, sep) {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			list := m.Mutable(fd).List()
			for _, item := range items {
				list.Append(protoreflect.ValueOfString(item))
			}
		}), nil
	case kubeKindMap:
		pairs := map[string]string{}
		for _, part := range strings.Split(text, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			k, v, ok := strings.Cut(part, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("%s: key=value pairs, comma-separated", f.label)
			}
			pairs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			mp := m.Mutable(fd).Map()
			for k, v := range pairs {
				mp.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfString(v))
			}
		}), nil
	case kubeKindTolerations:
		var tols []*fleetgrpc.KubeToleration
		for _, part := range strings.Split(text, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			t, err := parseToleration(part)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", f.label, err)
			}
			tols = append(tols, t)
		}
		return kubeFieldSource(f.path, func(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
			list := m.Mutable(fd).List()
			for _, t := range tols {
				list.Append(protoreflect.ValueOfMessage(t.ProtoReflect()))
			}
		}), nil
	}
	return nil, fmt.Errorf("%s cannot be edited as text", f.label)
}

func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, errors.New("on or off")
}

// choiceNames are a choice field's values as shown ("auto" for "").
func (f kubeField) choiceNames() []string {
	names := make([]string, len(f.options))
	for i, o := range f.options {
		if o == "" {
			o = f.def
		}
		names[i] = o
	}
	return names
}

// cycleOptions are the values ←/→ steps through, current value included.
func (f kubeField) cycleOptions(st *fleetgrpc.KubeClusterStatus) []string {
	switch f.kind {
	case kubeKindChoice:
		return f.options
	case kubeKindBool:
		return []string{"off", "on"}
	case kubeKindTri:
		return []string{"", "on", "off"}
	case kubeKindClass:
		out := []string{""}
		caps := st.GetCapabilities()
		switch f.classes {
		case kubeClassStorage:
			for _, sc := range caps.GetStorageClasses() {
				out = append(out, sc.GetName())
			}
		case kubeClassSnapshot:
			out = append(out, caps.GetSnapshotClasses()...)
		case kubeClassRuntime:
			out = append(out, caps.GetRuntimeClasses()...)
		}
		if cur := f.text(st.GetCluster()); !slices.Contains(out, cur) {
			out = append(out, cur)
		}
		return out
	}
	return nil
}

// cycles reports whether ←/→ change the field.
func (f kubeField) cycles() bool {
	switch f.kind {
	case kubeKindChoice, kubeKindBool, kubeKindTri, kubeKindClass:
		return true
	}
	return false
}

// next is the value one step from the current one (dir ±1), wrapping.
func (f kubeField) next(st *fleetgrpc.KubeClusterStatus, dir int) string {
	opts := f.cycleOptions(st)
	if len(opts) == 0 {
		return ""
	}
	cur := f.text(st.GetCluster())
	i := slices.Index(opts, cur)
	if i < 0 {
		i = 0
	}
	return opts[(i+dir+len(opts))%len(opts)]
}

// ===========================================
// Tolerations as text
// ===========================================

// formatToleration writes t as "key=value:Effect@seconds"; "key" alone
// tolerates every value of key (Exists), "*" every taint.
func formatToleration(t *fleetgrpc.KubeToleration) string {
	var b strings.Builder
	key := t.GetKey()
	if key == "" {
		key = "*"
	}
	b.WriteString(key)
	if t.GetOperator() != "Exists" {
		b.WriteString("=" + t.GetValue())
	}
	if t.GetEffect() != "" {
		b.WriteString(":" + t.GetEffect())
	}
	if t.TolerationSeconds != nil {
		b.WriteString("@" + strconv.FormatInt(t.GetTolerationSeconds(), 10))
	}
	return b.String()
}

// parseToleration reads formatToleration's form. The daemon validates the
// keys, values and effects.
func parseToleration(s string) (*fleetgrpc.KubeToleration, error) {
	t := &fleetgrpc.KubeToleration{}
	if rest, secs, ok := strings.Cut(s, "@"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(secs), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q: seconds after @ must be a number", s)
		}
		t.TolerationSeconds = &n
		s = rest
	}
	if rest, effect, ok := strings.Cut(s, ":"); ok {
		t.Effect = strings.TrimSpace(effect)
		s = rest
	}
	key, value, equal := strings.Cut(strings.TrimSpace(s), "=")
	key = strings.TrimSpace(key)
	switch {
	case key == "*" && !equal:
		t.Operator = "Exists"
	case key == "":
		return nil, fmt.Errorf("%q: a key (or * for every taint)", s)
	case equal:
		t.Key, t.Operator, t.Value = key, "Equal", strings.TrimSpace(value)
	default:
		t.Key, t.Operator = key, "Exists"
	}
	return t, nil
}
