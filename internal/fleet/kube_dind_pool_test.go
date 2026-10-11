package fleet

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDockerAddressPools(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []DockerAddressPool
		err  string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: "10.200.0.0/16", want: []DockerAddressPool{{Base: "10.200.0.0/16", Size: 24}}},
		// docker's own form, and host bits masked off as docker does.
		{in: "base=10.200.1.7/16,size=20", want: []DockerAddressPool{{Base: "10.200.0.0/16", Size: 20}}},
		{in: "10.200.0.0/16; base=10.201.0.0/16,size=26 10.200.0.0/16", want: []DockerAddressPool{
			{Base: "10.200.0.0/16", Size: 24}, {Base: "10.201.0.0/16", Size: 26}}},
		// A base narrower than /24 is one network of its own size.
		{in: "192.168.240.0/26", want: []DockerAddressPool{{Base: "192.168.240.0/26", Size: 26}}},
		{in: "fd00:dead::/48", want: []DockerAddressPool{{Base: "fd00:dead::/48", Size: 64}}},
		{in: "10.200.0.0", err: "not a CIDR"},
		{in: "base=10.200.0.0/16,size=12", err: "between the base's /16 and 32"},
		{in: "base=10.200.0.0/16,size=33", err: "between"},
		{in: "base=10.200.0.0/16,size=x", err: "not a number"},
		{in: "size=24", err: "has no base"},
		{in: "base=10.200.0.0/16,mtu=1450", err: `unknown key "mtu"`},
	} {
		got, err := ParseDockerAddressPools(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q: error %v, want one containing %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	k := &KubeCluster{DinDAddressPool: "not a pool"}
	if got := k.DinDAddressPools(); got != nil {
		t.Errorf("an unparsable setting gives %+v, want docker's defaults (nil)", got)
	}
	k.DinDAddressPool = "10.200.0.0/16"
	if got := k.DinDAddressPools(); len(got) != 1 || got[0].Size != 24 {
		t.Errorf("DinDAddressPools = %+v", got)
	}
}
