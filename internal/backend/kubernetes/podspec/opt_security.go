package podspec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The options about privilege: capabilities, security profiles, devices,
// sysctls, groups and the runtime.

// normalizeCap writes a capability as Kubernetes takes it: upper case,
// without the CAP_ prefix ("cap_sys_ptrace" is SYS_PTRACE), or ALL.
func normalizeCap(c string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
}

// validCap reports whether c (normalized) is a capability name.
func validCap(c string) bool {
	if c == "" {
		return false
	}
	for _, r := range c {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func (t *translation) optCapAdd(vals []string) Outcome {
	for _, v := range vals {
		c := normalizeCap(v)
		switch {
		case !validCap(c):
			t.refuse(refuseOption("cap-add", fmt.Sprintf("invalid capability %q", v)))
		case c == "SYS_PTRACE":
			t.debug.add(debugPtrace, flagText("cap-add", v))
		default:
			t.spec.Security.CapAdd = appendUnique(t.spec.Security.CapAdd, c)
		}
	}
	return ""
}

func (t *translation) optCapDrop(vals []string) Outcome {
	for _, v := range vals {
		c := normalizeCap(v)
		if !validCap(c) {
			t.refuse(refuseOption("cap-drop", fmt.Sprintf("invalid capability %q", v)))
			continue
		}
		t.spec.Security.CapDrop = appendUnique(t.spec.Security.CapDrop, c)
	}
	return ""
}

// finishCaps keeps docker's precedence between the two lists: a capability
// both added and dropped by name is added, where the container runtime
// would drop it.
func (t *translation) finishCaps() {
	s := &t.spec.Security
	var drop []string
	for _, c := range s.CapDrop {
		if c == "ALL" || !containsString(s.CapAdd, c) {
			drop = append(drop, c)
		}
	}
	s.CapDrop = drop
}

func (t *translation) optPrivileged(vals []string) Outcome {
	t.privileged = lastBool(vals)
	return ""
}

func (t *translation) optSecurityOpt(vals []string) Outcome {
	outcome := Outcome("")
	for _, v := range vals {
		if o := t.securityOpt(v); o != "" {
			outcome = o
		}
	}
	return outcome
}

// securityOpt translates one --security-opt, read as docker's CLI reads
// it: key=value (or the legacy key:value), no-new-privileges alone.
func (t *translation) securityOpt(v string) Outcome {
	key, value, ok := strings.Cut(v, "=")
	if !ok && key != "no-new-privileges" {
		key, value, ok = strings.Cut(v, ":")
	}
	if (!ok || value == "") && key != "no-new-privileges" {
		t.refuse(refuseOption("security-opt", fmt.Sprintf("invalid --security-opt: %q", v)))
		return ""
	}
	privileged := t.run.Bool("privileged", false)
	switch key {
	case "no-new-privileges":
		if value == "" || value == "true" {
			t.spec.Security.NoNewPrivileges = true
		} else if value != "false" {
			t.refuse(refuseOption("security-opt", fmt.Sprintf("invalid --security-opt: %q", v)))
		}
	case "seccomp":
		switch value {
		case "unconfined":
			t.debug.add(debugSeccomp, flagText("security-opt", v))
		case "builtin", "default":
		default:
			t.refuse(refuseOption("security-opt", fmt.Sprintf("seccomp profile %s is a file on the daemon host, which the nodes cannot read; load the profile on the nodes and give it to the pod with the connection's pod overrides", value)))
		}
	case "apparmor":
		switch value {
		case "unconfined":
			t.debug.add(debugAppArmor, flagText("security-opt", v))
		case "docker-default":
			t.spec.Security.AppArmor = AppArmorRuntimeDefault
		default:
			// Like docker, the profile has to be loaded where the
			// container runs: here, on the nodes.
			t.spec.Security.AppArmor = AppArmorLocalhostPrefix + value
		}
	case "label":
		return t.labelOpt(v, value)
	case "systempaths", "writable-cgroups":
		want := map[string]string{"systempaths": "unconfined", "writable-cgroups": "true"}[key]
		if value != want {
			if key == "writable-cgroups" && value == "false" {
				return ""
			}
			t.refuse(refuseOption("security-opt", fmt.Sprintf("invalid --security-opt: %q", v)))
			return ""
		}
		if !privileged {
			// A privileged pod already has both; nothing short of one does.
			t.refuse(refuseOption("security-opt", fmt.Sprintf("%s needs a privileged pod; add --privileged (which the connection must allow)", v)))
		}
	default:
		t.refuse(refuseOption("security-opt", fmt.Sprintf("unknown --security-opt %q", v)))
	}
	return ""
}

// labelOpt translates --security-opt label=... (SELinux).
func (t *translation) labelOpt(v, value string) Outcome {
	field, val, _ := strings.Cut(value, ":")
	se := t.spec.Security.SELinux
	if se == nil {
		se = &SELinux{}
	}
	switch field {
	case "disable":
		// docker runs the container unconfined; spc_t is the type that does
		// on a pod, and admission decides whether the namespace takes it.
		se.Type = "spc_t"
	case "user":
		se.User = val
	case "role":
		se.Role = val
	case "type":
		se.Type = val
	case "level":
		t.warn("%s: fleet gives every pod of a fleet's share-group one SELinux level, so they can share its files; this one is not used", flagText("security-opt", v))
		return Delta
	case "filetype", "nested":
		t.warn("%s: a pod has no SELinux %s option; it is not used", flagText("security-opt", v), field)
		return Delta
	default:
		t.refuse(refuseOption("security-opt", fmt.Sprintf("invalid --security-opt: %q", v)))
		return ""
	}
	t.spec.Security.SELinux = se
	return ""
}

func (t *translation) optDevice(vals []string) Outcome {
	for _, v := range vals {
		host, container, perms, err := parseDevice(v)
		if err != nil {
			t.refuse(refuseOption("device", err.Error()))
			continue
		}
		if perms != "rwm" {
			t.warn("%s: a privileged pod can read, write and create every device; the %q permissions are not enforced", flagText("device", v), perms)
		}
		t.needsPrivileged = append(t.needsPrivileged, flagText("device", v))
		t.runMounts = append(t.runMounts, runMount{Mount{Kind: MountDevice, Source: host, Target: container}, "device"})
	}
	return ""
}

// parseDevice reads docker's --device host[:container[:permissions]].
func parseDevice(v string) (host, container, perms string, err error) {
	parts := strings.Split(v, ":")
	host = parts[0]
	container, perms = host, "rwm"
	switch len(parts) {
	case 1:
	case 2:
		if validDevicePerms(parts[1]) {
			perms = parts[1]
		} else {
			container = parts[1]
		}
	case 3:
		container, perms = parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("invalid device specification: %s", v)
	}
	if !strings.HasPrefix(host, "/") || !strings.HasPrefix(container, "/") || !validDevicePerms(perms) {
		return "", "", "", fmt.Errorf("invalid device specification: %s", v)
	}
	return host, cleanTarget(container), perms, nil
}

func validDevicePerms(p string) bool {
	if p == "" || len(p) > 3 {
		return false
	}
	for _, r := range p {
		if !strings.ContainsRune("rwm", r) {
			return false
		}
	}
	return true
}

func (t *translation) optDeviceCgroupRule(vals []string) Outcome {
	if t.run.Bool("privileged", false) {
		// A privileged pod may use every device already.
		return Ignore
	}
	t.refuse(refuseOption("device-cgroup-rule", fmt.Sprintf("%s widens device access, which a pod can only get by being privileged; add --privileged (which the connection must allow)", flagText("device-cgroup-rule", last(vals)))))
	return ""
}

// safeSysctls are Kubernetes' safe sysctls (namespaced, isolated per pod,
// allowed by every kubelet and by baseline pod security).
var safeSysctls = map[string]bool{
	"kernel.shm_rmid_forced":              true,
	"net.ipv4.ip_local_port_range":        true,
	"net.ipv4.ip_local_reserved_ports":    true,
	"net.ipv4.ip_unprivileged_port_start": true,
	"net.ipv4.ping_group_range":           true,
	"net.ipv4.tcp_fin_timeout":            true,
	"net.ipv4.tcp_keepalive_intvl":        true,
	"net.ipv4.tcp_keepalive_probes":       true,
	"net.ipv4.tcp_keepalive_time":         true,
	"net.ipv4.tcp_rmem":                   true,
	"net.ipv4.tcp_syncookies":             true,
	"net.ipv4.tcp_wmem":                   true,
}

func (t *translation) optSysctl(vals []string) Outcome {
	byName := map[string]int{}
	for _, v := range vals {
		name, value, ok := strings.Cut(v, "=")
		name = strings.ReplaceAll(strings.TrimSpace(name), "/", ".")
		if !ok || name == "" {
			t.refuse(refuseOption("sysctl", fmt.Sprintf("invalid sysctl %q: name=value", v)))
			continue
		}
		if !safeSysctls[name] {
			t.refuse(refuseOption("sysctl", fmt.Sprintf("%s is not one of Kubernetes' safe sysctls; any other needs the kubelet's --allowed-unsafe-sysctls on every node and a privileged-level namespace", name)))
			continue
		}
		if i, seen := byName[name]; seen {
			t.spec.Sysctls[i].Value = value
			continue
		}
		byName[name] = len(t.spec.Sysctls)
		t.spec.Sysctls = append(t.spec.Sysctls, Sysctl{Name: name, Value: value})
	}
	return ""
}

func (t *translation) optGroupAdd(vals []string) Outcome {
	for _, v := range vals {
		gid, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			ok := false
			if t.in.LookupGroup != nil {
				gid, ok = t.in.LookupGroup(v)
			}
			if !ok {
				t.refuse(refuseOption("group-add", fmt.Sprintf("group %q is not a number and the image's /etc/group does not name it; a pod's supplementalGroups are numeric", v)))
				continue
			}
		}
		if gid < 0 {
			t.refuse(refuseOption("group-add", fmt.Sprintf("invalid group %q", v)))
			continue
		}
		if !containsInt64(t.spec.SupplementalGroups, gid) {
			t.spec.SupplementalGroups = append(t.spec.SupplementalGroups, gid)
		}
	}
	return ""
}

func (t *translation) optRuntime(vals []string) Outcome {
	v := last(vals)
	switch {
	case v == "runc" || v == "io.containerd.runc.v2":
		// docker's default runtime is the cluster's default too.
		return ""
	case t.cluster.RuntimeClass != "" && t.cluster.RuntimeClass != v:
		t.refuse(refuseOption("runtime", fmt.Sprintf("--runtime %s asks for another runtime than %s's runtime class, %s", v, t.clusterName(), t.cluster.RuntimeClass)))
	case t.in.Facts.RuntimeClasses != nil && !containsString(t.in.Facts.RuntimeClasses, v):
		t.refuse(refuseOption("runtime", fmt.Sprintf("--runtime %s: %s has no RuntimeClass %s (it has %s)", v, t.clusterName(), v, listOrNone(t.in.Facts.RuntimeClasses))))
	default:
		t.spec.RuntimeClassName = v
	}
	return ""
}

func (t *translation) optUserNS(vals []string) Outcome {
	if v := last(vals); v != "host" && v != "" {
		t.refuse(refuseOption("userns", fmt.Sprintf("--userns %s: a pod has no user namespace mode docker's maps to", v)))
	}
	return ""
}

func (t *translation) optIsolation(vals []string) Outcome {
	if v := last(vals); v != "" && v != "default" {
		t.refuse(refuseOption("isolation", fmt.Sprintf("--isolation %s: Linux containers take only default", v)))
	}
	return ""
}

func (t *translation) optAnnotation(vals []string) Outcome {
	var keys []string
	for _, v := range vals {
		k, _, _ := strings.Cut(v, "=")
		keys = appendUnique(keys, k)
	}
	// Only the keys: an annotation's value is the run's own.
	t.warn("--annotation %s: OCI runtime annotations have no pod field; they are not passed on", strings.Join(keys, ", "))
	return ""
}

func (t *translation) optCgroupNS(vals []string) Outcome {
	switch v := last(vals); v {
	case "private", "":
	case "host":
		t.refuse(refuseOption("cgroupns", "--cgroupns host: a pod cannot share the node's cgroup namespace"))
	default:
		t.refuse(refuseOption("cgroupns", fmt.Sprintf("invalid cgroup namespace mode %q", v)))
	}
	return ""
}

// debugGrant is one kind of debug-only grant.
type debugGrant int

const (
	debugPtrace debugGrant = iota
	debugSeccomp
	debugAppArmor
)

// debugGrants are the debug grants a run asks for, and how it wrote them.
type debugGrants struct {
	ptrace, seccomp, apparmor bool
	written                   []string
}

func (d *debugGrants) add(g debugGrant, written string) {
	switch g {
	case debugPtrace:
		d.ptrace = true
	case debugSeccomp:
		d.seccomp = true
	case debugAppArmor:
		d.apparmor = true
	}
	d.written = appendUnique(d.written, written)
}

func appendUnique(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsInt64(list []int64, n int64) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

// listOrNone lists names for a message, sorted, or "none".
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	s := append([]string(nil), names...)
	sort.Strings(s)
	return strings.Join(s, ", ")
}
