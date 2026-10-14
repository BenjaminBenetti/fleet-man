package podspec

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// The options about the network: namespaces, names, DNS and published
// ports.

func (t *translation) optNetwork(vals []string) Outcome {
	for _, v := range vals {
		mode, _, _ := strings.Cut(v, ":")
		switch {
		case v == "bridge" || v == "default":
			// The pod network is the instance's network.
		case v == "host":
			t.spec.HostNetwork = true
			t.needsPrivilegeAllowed("--network host")
		case v == "none":
			t.refuse(refuseOption("network", "--network none: a pod always has its network"))
		case mode == "container":
			t.refuse(refuseOption("network", fmt.Sprintf("--network %s: fleet runs one container per instance; there is no other container's network to join", v)))
		default:
			t.refuse(refuseOption("network", fmt.Sprintf("--network %s: a pod has no docker networks; sibling instances reach each other as <sts>-0.<fleet service> through the fleet's headless Service", v)))
		}
	}
	return ""
}

func (t *translation) optIPC(vals []string) Outcome {
	v := last(vals)
	mode, _, _ := strings.Cut(v, ":")
	switch {
	case v == "" || v == "private" || v == "shareable":
	case v == "host":
		t.spec.HostIPC = true
		t.needsPrivilegeAllowed("--ipc host")
	case v == "none":
		t.warn("--ipc none: the pod has its own IPC namespace with a /dev/shm; docker would mount none")
		return Delta
	case mode == "container":
		t.refuse(refuseOption("ipc", fmt.Sprintf("--ipc %s: fleet runs one container per instance; there is no other container's IPC namespace to join", v)))
	default:
		t.refuse(refuseOption("ipc", fmt.Sprintf("invalid IPC mode %q", v)))
	}
	return ""
}

func (t *translation) optPID(vals []string) Outcome {
	v := last(vals)
	mode, _, _ := strings.Cut(v, ":")
	switch {
	case v == "" || v == "private":
	case v == "host":
		t.spec.HostPID = true
		t.needsPrivilegeAllowed("--pid host")
	case mode == "container":
		t.refuse(refuseOption("pid", fmt.Sprintf("--pid %s: fleet runs one container per instance; there is no other container's PID namespace to join", v)))
	default:
		t.refuse(refuseOption("pid", fmt.Sprintf("invalid PID mode %q", v)))
	}
	return ""
}

// hostDockerInternal is the name docker gives the daemon host; a run that
// declares host-gateway under any name gets it too.
const hostDockerInternal = "host.docker.internal"

func (t *translation) optAddHost(vals []string) Outcome {
	gateway := false
	for _, v := range vals {
		// docker takes host=ip, and host:ip from before IPv6 needed "=".
		name, addr, ok := strings.Cut(v, "=")
		if !ok {
			name, addr, ok = strings.Cut(v, ":")
		}
		if !ok || name == "" || addr == "" {
			t.refuse(refuseOption("add-host", fmt.Sprintf("invalid --add-host %q: host:ip", v)))
			continue
		}
		if addr == "host-gateway" {
			gateway = true
			t.addHostAlias(HostGatewayIP, name)
			if t.cluster.MirrorsHostGateway() {
				t.spec.HostGateway = true
			} else {
				t.warn("%s: %s resolves to %s, but host_gateway is off on %s, so no daemon-host port is mirrored there", flagText("add-host", v), name, HostGatewayIP, t.clusterName())
			}
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]"))
		if err != nil {
			t.refuse(refuseOption("add-host", fmt.Sprintf("invalid IP address in --add-host %q", v)))
			continue
		}
		t.addHostAlias(ip.String(), name)
	}
	if gateway {
		t.aliasHostDockerInternal()
	}
	return ""
}

// aliasHostDockerInternal names the host-gateway address host.docker.internal
// as well, for a run that declared host-gateway, unless the run gives that
// name an address of its own.
func (t *translation) aliasHostDockerInternal() {
	for _, a := range t.spec.HostAliases {
		if containsString(a.Hostnames, hostDockerInternal) {
			return
		}
	}
	t.addHostAlias(HostGatewayIP, hostDockerInternal)
}

// addHostAlias adds name for ip to the pod's /etc/hosts, after the names ip
// already has.
func (t *translation) addHostAlias(ip, name string) {
	for i := range t.spec.HostAliases {
		if t.spec.HostAliases[i].IP == ip {
			t.spec.HostAliases[i].Hostnames = appendUnique(t.spec.HostAliases[i].Hostnames, name)
			return
		}
	}
	t.spec.HostAliases = append(t.spec.HostAliases, HostAlias{IP: ip, Hostnames: []string{name}})
}

func (t *translation) dnsConfig() *DNSConfig {
	if t.spec.DNS == nil {
		t.spec.DNS = &DNSConfig{}
	}
	return t.spec.DNS
}

func (t *translation) optDNS(vals []string) Outcome {
	for _, v := range vals {
		ip, err := netip.ParseAddr(v)
		if err != nil {
			t.refuse(refuseOption("dns", fmt.Sprintf("invalid DNS server %q", v)))
			continue
		}
		d := t.dnsConfig()
		d.Nameservers = appendUnique(d.Nameservers, ip.String())
	}
	if t.spec.DNS != nil && len(t.spec.DNS.Nameservers) > 0 {
		t.warn("--dns %s: the pod asks the cluster's DNS server first, so fleet's services and sibling instances keep resolving; docker would ask only these", strings.Join(t.spec.DNS.Nameservers, ", "))
	}
	return ""
}

func (t *translation) optDNSSearch(vals []string) Outcome {
	for _, v := range vals {
		if v == "." {
			// docker's "no search domains".
			continue
		}
		d := t.dnsConfig()
		d.Searches = appendUnique(d.Searches, v)
	}
	return ""
}

func (t *translation) optDNSOption(vals []string) Outcome {
	for _, v := range vals {
		name, value, hasValue := strings.Cut(v, ":")
		if name == "" {
			t.refuse(refuseOption("dns-option", fmt.Sprintf("invalid DNS option %q", v)))
			continue
		}
		opt := DNSOption{Name: name}
		if hasValue {
			opt.Value = &value
		}
		d := t.dnsConfig()
		d.Options = append(d.Options, opt)
	}
	return ""
}

func (t *translation) optHostname(vals []string) Outcome {
	t.hostname = last(vals)
	return ""
}

func (t *translation) optDomainname(vals []string) Outcome {
	t.domainname = last(vals)
	return ""
}

// finishHostname gives the pod the run's hostname and domain name where it
// can: pod-init sets them when the pod has SYS_ADMIN. Otherwise the pod keeps
// the name its StatefulSet gives it, and the run's name resolves locally.
//
// A host-network pod is in the node's UTS namespace (docker's --network host
// keeps a private one), so setting the name there would rename the node: it
// keeps the node's name whatever the pod may do.
func (t *translation) finishHostname() {
	canSet := (t.hasCap("SYS_ADMIN") || t.sysbox) && !t.spec.HostNetwork
	why := "setting another needs SYS_ADMIN"
	if t.spec.HostNetwork {
		why = "a host-network pod shares the node's hostname, which fleet never changes"
	}
	if t.hostname != "" {
		if canSet {
			t.spec.PID1.Hostname = t.hostname
			t.setOutcome("hostname", Map)
		} else {
			t.addHostAlias("127.0.0.1", t.hostname)
			t.warn("--hostname %s: the pod keeps the hostname its StatefulSet gives it (%s); %s resolves to 127.0.0.1", t.hostname, why, t.hostname)
		}
	}
	if t.domainname != "" {
		if canSet {
			t.spec.PID1.Domainname = t.domainname
			t.setOutcome("domainname", Map)
		} else {
			t.warn("--domainname %s: the pod keeps its own domain name (%s)", t.domainname, why)
		}
	}
}

func (t *translation) optPublish(vals []string) Outcome {
	outcome := Outcome("")
	for _, v := range vals {
		ports, proto, err := parsePublish(v)
		if err != nil {
			t.refuse(refuseOption("publish", err.Error()))
			continue
		}
		if proto != "tcp" {
			t.warn("%s: only TCP is forwarded from a pod; the %s port is not published", flagText("publish", v), strings.ToUpper(proto))
			outcome = Delta
			continue
		}
		t.spec.Ports = append(t.spec.Ports, ports...)
	}
	return outcome
}

// parsePublish reads docker's -p [ip:][hostPort:]containerPort[/proto],
// either port maybe a range of the same length (a single container port
// and a host range is docker picking one of the range, which is refused).
// The address defaults to 127.0.0.1: a remote daemon would otherwise
// publish the pod on its network.
func parsePublish(v string) ([]Port, string, error) {
	raw, proto, _ := strings.Cut(v, "/")
	if proto == "" {
		proto = "tcp"
	}
	proto = strings.ToLower(proto)
	if proto != "tcp" && proto != "udp" && proto != "sctp" {
		return nil, "", fmt.Errorf("invalid protocol %q in --publish %s", proto, v)
	}
	// The container port is the last part, the host port the one before,
	// and the rest the address (which may be IPv6, bracketed or not).
	parts := strings.Split(raw, ":")
	containerPart := parts[len(parts)-1]
	hostPart, ipPart := "", ""
	if len(parts) >= 2 {
		hostPart = parts[len(parts)-2]
	}
	if len(parts) >= 3 {
		ipPart = strings.Join(parts[:len(parts)-2], ":")
	}
	ip := "127.0.0.1"
	if ipPart != "" {
		addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(ipPart, "["), "]"))
		if err != nil {
			return nil, "", fmt.Errorf("invalid IP address %q in --publish %s", ipPart, v)
		}
		ip = addr.String()
	}
	cLo, cHi, err := portRange(containerPart)
	if err != nil {
		return nil, "", fmt.Errorf("invalid container port in --publish %s: %v", v, err)
	}
	hLo, hHi := 0, 0
	if hostPart != "" {
		if hLo, hHi, err = portRange(hostPart); err != nil {
			return nil, "", fmt.Errorf("invalid host port in --publish %s: %v", v, err)
		}
		if hHi-hLo != cHi-cLo {
			return nil, "", fmt.Errorf("--publish %s: the host and container port ranges differ in length; docker would pick one host port of the range, which fleet does not", v)
		}
	}
	var out []Port
	for p := cLo; p <= cHi; p++ {
		host := 0
		if hostPart != "" {
			host = hLo + (p - cLo)
		}
		out = append(out, Port{HostIP: ip, HostPort: host, ContainerPort: p, Protocol: "TCP"})
	}
	return out, proto, nil
}

// portRange reads a port or a lo-hi range.
func portRange(s string) (int, int, error) {
	lo, hi, isRange := strings.Cut(s, "-")
	l, err := portNumber(lo)
	if err != nil {
		return 0, 0, err
	}
	if !isRange {
		return l, l, nil
	}
	h, err := portNumber(hi)
	if err != nil {
		return 0, 0, err
	}
	if h < l {
		return 0, 0, fmt.Errorf("invalid range %s", s)
	}
	return l, h, nil
}

func portNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}

func (t *translation) optPublishAll(vals []string) Outcome {
	t.publishAll = lastBool(vals)
	return ""
}

func (t *translation) optExpose(vals []string) Outcome {
	for _, v := range vals {
		raw, proto, _ := strings.Cut(v, "/")
		lo, hi, err := portRange(raw)
		if err != nil {
			t.refuse(refuseOption("expose", fmt.Sprintf("invalid --expose %q: %v", v, err)))
			continue
		}
		if proto != "" && !strings.EqualFold(proto, "tcp") {
			t.exposedOther = appendUnique(t.exposedOther, strings.ToLower(v))
			continue
		}
		for p := lo; p <= hi; p++ {
			t.exposed = append(t.exposed, p)
		}
	}
	return ""
}

// finishPorts publishes every exposed port for -P that the run did not
// publish itself.
func (t *translation) finishPorts() {
	if !t.publishAll {
		return
	}
	published := map[int]bool{}
	for _, p := range t.spec.Ports {
		published[p.ContainerPort] = true
	}
	ports := append([]int(nil), t.exposed...)
	other := append([]string(nil), t.exposedOther...)
	for _, key := range sortedKeys(t.in.Image.Config.ExposedPorts) {
		raw, proto, _ := strings.Cut(key, "/")
		if proto != "" && !strings.EqualFold(proto, "tcp") {
			other = appendUnique(other, strings.ToLower(key))
			continue
		}
		if p, err := portNumber(raw); err == nil {
			ports = append(ports, p)
		}
	}
	if len(other) > 0 {
		t.warn("--publish-all: only TCP is forwarded from a pod; %s not published", strings.Join(other, ", "))
	}
	for _, p := range ports {
		if published[p] {
			continue
		}
		published[p] = true
		t.spec.Ports = append(t.spec.Ports, Port{HostIP: "127.0.0.1", ContainerPort: p, Protocol: "TCP"})
	}
}
