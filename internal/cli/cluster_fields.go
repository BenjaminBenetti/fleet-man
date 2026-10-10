package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// cluster_fields.go reads and prints the `<field>=<value>` arguments of
// `fleet cluster add|set|test`. Field names are the KubeCluster proto field
// names (dashes accepted for underscores), so every field the daemon stores
// is settable here with nothing to keep in step: a field added to kube.proto
// is a field `set` takes. A sub-field is named by its path
// (resources.cpu_request, image.mode); a list or map field takes one item per
// occurrence (extra_path=/a extra_path=/b, node_selector=pool=fleet); an
// empty value (field=) clears a field back to its default. `ls <cluster>`
// prints a record in the same form.

// clusterFieldAliases are friendlier spellings of KubeCluster fields.
var clusterFieldAliases = map[string]string{
	"name":       "display_name",
	"kubeconfig": "kubeconfig_paths",
}

// daemonOwnedClusterFields are the KubeCluster fields only fleetd writes.
var daemonOwnedClusterFields = map[string]bool{
	"id": true, "server": true, "cluster_uid": true,
	"created_at": true, "updated_at": true, "verified_at": true, "revision": true,
}

const timestampFullName = "google.protobuf.Timestamp"

// clusterFieldArg is one `<field>=<value>` argument.
type clusterFieldArg struct {
	path  string
	value string
}

// isClusterFieldArg reports an argument of the `<field>=<value>` form.
func isClusterFieldArg(arg string) bool {
	i := strings.IndexByte(arg, '=')
	return i > 0
}

// normalizeClusterField is a field path as the proto names it.
func normalizeClusterField(path string) string {
	path = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(path)), "-", "_")
	if alias, ok := clusterFieldAliases[path]; ok {
		return alias
	}
	return path
}

// parseClusterFields applies `<field>=<value>` arguments to a new KubeCluster
// and returns it with the field paths they name (the UpdateKubeCluster mask),
// in the order first given.
func parseClusterFields(args []string) (*fleetgrpc.KubeCluster, []string, error) {
	c := &fleetgrpc.KubeCluster{}
	var order []string
	values := map[string][]string{}
	for _, arg := range args {
		i := strings.IndexByte(arg, '=')
		if i <= 0 {
			return nil, nil, fmt.Errorf("%q is not <field>=<value>", arg)
		}
		path := normalizeClusterField(arg[:i])
		if _, seen := values[path]; !seen {
			order = append(order, path)
		}
		values[path] = append(values[path], unquoteValue(arg[i+1:]))
	}
	for _, path := range order {
		if err := setClusterField(c.ProtoReflect(), path, values[path]); err != nil {
			return nil, nil, err
		}
	}
	return c, order, nil
}

// unquoteValue undoes the Go quoting `ls <cluster>` puts on a value with a
// newline or another control character, so a printed line can be pasted back.
func unquoteValue(v string) string {
	if len(v) >= 2 && v[0] == '"' {
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
	}
	return v
}

// setClusterField sets the field path names on root from its values.
func setClusterField(root protoreflect.Message, path string, values []string) error {
	segs := strings.Split(path, ".")
	if daemonOwnedClusterFields[segs[0]] {
		return fmt.Errorf("%s is set by fleetd, not by clients", segs[0])
	}
	msg := root
	for i, seg := range segs {
		fd := msg.Descriptor().Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			return fmt.Errorf("unknown field %q (fleet cluster set --help lists them)", strings.Join(segs[:i+1], "."))
		}
		if i < len(segs)-1 {
			if fd.Message() == nil || fd.IsList() || fd.IsMap() || fd.Message().FullName() == timestampFullName {
				return fmt.Errorf("%s has no sub-fields", strings.Join(segs[:i+1], "."))
			}
			msg = msg.Mutable(fd).Message()
			continue
		}
		return setField(msg, fd, path, values)
	}
	return nil
}

func setField(msg protoreflect.Message, fd protoreflect.FieldDescriptor, path string, values []string) error {
	if len(values) == 1 && values[0] == "" {
		// field= : leave it unset, which the daemon reads as its default
		// (and an update clears it).
		return nil
	}
	switch {
	case fd.IsMap():
		m := msg.Mutable(fd).Map()
		for _, v := range values {
			key, val, ok := strings.Cut(v, "=")
			if !ok || key == "" {
				return fmt.Errorf("%s: %q is not <key>=<value>", path, v)
			}
			k, err := parseScalar(fd.MapKey(), path, key)
			if err != nil {
				return err
			}
			if m.Has(k.MapKey()) {
				return fmt.Errorf("%s: key %q given twice", path, key)
			}
			mv, err := parseScalar(fd.MapValue(), path, val)
			if err != nil {
				return err
			}
			m.Set(k.MapKey(), mv)
		}
		return nil
	case fd.IsList():
		list := msg.Mutable(fd).List()
		for _, v := range values {
			if v == "" {
				return fmt.Errorf("%s: an empty item (%s= alone clears the list)", path, path)
			}
			if fd.Message() != nil {
				item := list.NewElement()
				if err := parseMessageItem(item.Message(), path, v); err != nil {
					return err
				}
				list.Append(item)
				continue
			}
			sv, err := parseScalar(fd, path, v)
			if err != nil {
				return err
			}
			list.Append(sv)
		}
		return nil
	}
	if len(values) > 1 {
		return fmt.Errorf("%s given more than once", path)
	}
	v := values[0]
	if md := fd.Message(); md != nil {
		if !strings.HasPrefix(strings.TrimSpace(v), "{") {
			return fmt.Errorf("%s: set its sub-fields (%s), or give a JSON object", path, subFieldNames(path, md))
		}
		sub := msg.Mutable(fd).Message()
		if err := protojson.Unmarshal([]byte(v), sub.Interface()); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		return nil
	}
	sv, err := parseScalar(fd, path, v)
	if err != nil {
		return err
	}
	msg.Set(fd, sv)
	return nil
}

func subFieldNames(path string, md protoreflect.MessageDescriptor) string {
	names := make([]string, md.Fields().Len())
	for i := range names {
		names[i] = path + "." + string(md.Fields().Get(i).Name())
	}
	return strings.Join(names, ", ")
}

func parseScalar(fd protoreflect.FieldDescriptor, path, v string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%s: %q is not true or false", path, v)
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%s: %q is not a whole number", path, v)
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%s: %q is not a whole number", path, v)
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%s: %q is not a whole number", path, v)
		}
		return protoreflect.ValueOfUint32(uint32(n)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%s: %q is not a whole number", path, v)
		}
		return protoreflect.ValueOfUint64(n), nil
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByName(protoreflect.Name(v)); ev != nil {
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		return protoreflect.Value{}, fmt.Errorf("%s: unknown value %q", path, v)
	}
	return protoreflect.Value{}, fmt.Errorf("%s cannot be set from the command line", path)
}

// parseMessageItem reads one item of a list of messages: a JSON object, or
// for tolerations the taint-like <key>[=<value>][:<effect>[:<seconds>]].
func parseMessageItem(m protoreflect.Message, path, v string) error {
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		if err := protojson.Unmarshal([]byte(v), m.Interface()); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		return nil
	}
	t, ok := m.Interface().(*fleetgrpc.KubeToleration)
	if !ok {
		return fmt.Errorf("%s: give each item as a JSON object", path)
	}
	return parseToleration(t, path, v)
}

// parseToleration reads <key>[=<value>][:<effect>[:<seconds>]]: with a value
// the toleration is Equal, without one Exists (an empty key with Exists
// tolerates every taint of the effect).
func parseToleration(t *fleetgrpc.KubeToleration, path, v string) error {
	parts := strings.Split(v, ":")
	if len(parts) > 3 {
		return fmt.Errorf("%s: %q is not <key>[=<value>][:<effect>[:<seconds>]]", path, v)
	}
	if key, value, ok := strings.Cut(parts[0], "="); ok {
		t.Key, t.Operator, t.Value = key, "Equal", value
	} else {
		t.Key, t.Operator = parts[0], "Exists"
	}
	if len(parts) > 1 {
		t.Effect = parts[1]
	}
	if len(parts) > 2 {
		secs, err := tolerationSeconds(parts[2])
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		t.TolerationSeconds = &secs
	}
	return nil
}

func tolerationSeconds(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number of seconds", s)
	}
	return int64(d / time.Second), nil
}

// formatToleration is parseToleration's form of t, or false when t has none
// (an Equal toleration without a key, or Exists for everything).
func formatToleration(t *fleetgrpc.KubeToleration) (string, bool) {
	var s string
	switch t.GetOperator() {
	case "", "Equal":
		if t.GetKey() == "" {
			return "", false
		}
		s = t.GetKey() + "=" + t.GetValue()
	case "Exists":
		if t.GetValue() != "" || (t.GetKey() == "" && t.GetEffect() == "") {
			return "", false
		}
		s = t.GetKey()
	default:
		return "", false
	}
	if t.GetEffect() != "" || t.TolerationSeconds != nil {
		s += ":" + t.GetEffect()
	}
	if t.TolerationSeconds != nil {
		s += ":" + strconv.FormatInt(t.GetTolerationSeconds(), 10)
	}
	return s, true
}

// clusterFieldLines prints every field c has, one `<field>=<value>` per line
// (a list or map item per line), in proto field order: the form `set` reads.
func clusterFieldLines(c *fleetgrpc.KubeCluster) []string {
	var lines []string
	appendFieldLines(&lines, "", c.ProtoReflect())
	return lines
}

func appendFieldLines(lines *[]string, prefix string, m protoreflect.Message) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		path := prefix + string(fd.Name())
		v := m.Get(fd)
		switch {
		case fd.IsMap():
			var keys []string
			byKey := map[string]string{}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				ks := k.String()
				keys = append(keys, ks)
				byKey[ks] = formatScalar(fd.MapValue(), mv)
				return true
			})
			sort.Strings(keys)
			for _, k := range keys {
				*lines = append(*lines, path+"="+quoteValue(k+"="+byKey[k]))
			}
		case fd.IsList():
			list := v.List()
			for j := 0; j < list.Len(); j++ {
				*lines = append(*lines, path+"="+quoteValue(formatListItem(fd, list.Get(j))))
			}
		case fd.Message() != nil && fd.Message().FullName() == timestampFullName:
			ts, _ := v.Message().Interface().(*timestamppb.Timestamp)
			*lines = append(*lines, path+"="+ts.AsTime().UTC().Format(time.RFC3339))
		case fd.Message() != nil:
			appendFieldLines(lines, path+".", v.Message())
		default:
			*lines = append(*lines, path+"="+quoteValue(formatScalar(fd, v)))
		}
	}
}

func formatListItem(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	if fd.Message() == nil {
		return formatScalar(fd, v)
	}
	if t, ok := v.Message().Interface().(*fleetgrpc.KubeToleration); ok {
		if s, ok := formatToleration(t); ok {
			return s
		}
	}
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(v.Message().Interface())
	if err != nil {
		return ""
	}
	// protojson's spacing is deliberately unstable: compact it to one line.
	var b bytes.Buffer
	if err := json.Compact(&b, data); err != nil {
		return string(data)
	}
	return b.String()
}

func formatScalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	if fd.Kind() == protoreflect.EnumKind {
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return string(ev.Name())
		}
	}
	return v.String()
}

// quoteValue Go-quotes a value that would not survive as one line.
func quoteValue(s string) string {
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return strconv.Quote(s)
		}
	}
	if strings.HasPrefix(s, `"`) {
		return strconv.Quote(s)
	}
	return s
}

// clusterFieldHelp lists the settable fields for `set --help`, wrapped.
func clusterFieldHelp() string {
	var names []string
	var walk func(prefix string, md protoreflect.MessageDescriptor)
	walk = func(prefix string, md protoreflect.MessageDescriptor) {
		for i := 0; i < md.Fields().Len(); i++ {
			fd := md.Fields().Get(i)
			name := string(fd.Name())
			if prefix == "" && daemonOwnedClusterFields[name] {
				continue
			}
			if sub := fd.Message(); sub != nil && !fd.IsList() && !fd.IsMap() {
				walk(prefix+name+".", sub)
				continue
			}
			names = append(names, prefix+name)
		}
	}
	walk("", (&fleetgrpc.KubeCluster{}).ProtoReflect().Descriptor())
	var b strings.Builder
	line := " "
	for _, n := range names {
		if len(line)+1+len(n) > 78 {
			b.WriteString(line + "\n")
			line = " "
		}
		line += " " + n
	}
	b.WriteString(line)
	return b.String()
}
