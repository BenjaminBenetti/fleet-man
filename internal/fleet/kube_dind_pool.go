package fleet

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// DockerAddressPool is one of docker's default-address-pools: the networks a
// docker engine creates (docker network create, compose) take Size-bit
// subnets of Base.
type DockerAddressPool struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

// DefaultDockerPoolSize is the subnet size of a pool written as a bare CIDR.
const DefaultDockerPoolSize = 24

// ParseDockerAddressPools reads a connection's dind_address_pool: one pool or
// several, separated by spaces or ';', each a CIDR ("10.200.0.0/16", whose
// networks are /24s) or docker's own --default-address-pool form
// ("base=10.200.0.0/16,size=24"). "" is no pool (docker's defaults).
func ParseDockerAddressPools(s string) ([]DockerAddressPool, error) {
	var pools []DockerAddressPool
	seen := map[DockerAddressPool]bool{}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' || r == '\n' }) {
		p, err := parseDockerAddressPool(field)
		if err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			pools = append(pools, p)
		}
	}
	return pools, nil
}

func parseDockerAddressPool(field string) (DockerAddressPool, error) {
	base, size := field, ""
	if strings.Contains(field, "=") {
		base = ""
		for _, kv := range strings.Split(field, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch strings.TrimSpace(k) {
			case "base":
				base = strings.TrimSpace(v)
			case "size":
				size = strings.TrimSpace(v)
			default:
				return DockerAddressPool{}, fmt.Errorf("address pool %q: unknown key %q (want base=<cidr>,size=<bits>)", field, k)
			}
		}
		if base == "" {
			return DockerAddressPool{}, fmt.Errorf("address pool %q has no base", field)
		}
	}
	prefix, err := netip.ParsePrefix(base)
	if err != nil {
		return DockerAddressPool{}, fmt.Errorf("address pool %q: %q is not a CIDR such as 10.200.0.0/16", field, base)
	}
	prefix = prefix.Masked()
	bits := prefix.Addr().BitLen()
	n := max(DefaultDockerPoolSize, prefix.Bits())
	if prefix.Addr().Is6() && size == "" {
		n = max(64, prefix.Bits())
	}
	if size != "" {
		if n, err = strconv.Atoi(size); err != nil {
			return DockerAddressPool{}, fmt.Errorf("address pool %q: size %q is not a number of bits", field, size)
		}
	}
	if n < prefix.Bits() || n > bits {
		return DockerAddressPool{}, fmt.Errorf("address pool %q: size %d must be between the base's /%d and %d", field, n, prefix.Bits(), bits)
	}
	return DockerAddressPool{Base: prefix.String(), Size: n}, nil
}
