package fleet

import "testing"

// Privileged pods are auto only on a local cluster; a set toggle wins on any.
func TestKubeClusterAllowsPrivileged(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name   string
		toggle *bool
		local  bool
		want   bool
	}{
		{"unset on a local cluster", nil, true, true},
		{"unset elsewhere", nil, false, false},
		{"allowed elsewhere", &yes, false, true},
		{"allowed on a local cluster", &yes, true, true},
		{"refused on a local cluster", &no, true, false},
		{"refused elsewhere", &no, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := KubeCluster{AllowPrivileged: tt.toggle}
			if got := k.AllowsPrivileged(tt.local); got != tt.want {
				t.Errorf("AllowsPrivileged(%v) = %v, want %v", tt.local, got, tt.want)
			}
		})
	}
}

func TestKubeClusterDebugAndHostGatewayPolicies(t *testing.T) {
	for _, d := range []KubeDebugCapabilities{"", KubeDebugCapabilitiesAuto} {
		if (&KubeCluster{DebugCapabilities: d}).RequiresDebugCapabilities() {
			t.Errorf("debug_capabilities %q requires them; auto drops them", d)
		}
	}
	if !(&KubeCluster{DebugCapabilities: KubeDebugCapabilitiesRequire}).RequiresDebugCapabilities() {
		t.Error("debug_capabilities require does not require them")
	}
	for _, g := range []KubeHostGateway{"", KubeHostGatewayAuto} {
		if !(&KubeCluster{HostGateway: g}).MirrorsHostGateway() {
			t.Errorf("host_gateway %q does not mirror", g)
		}
	}
	if (&KubeCluster{HostGateway: KubeHostGatewayOff}).MirrorsHostGateway() {
		t.Error("host_gateway off mirrors")
	}
}
