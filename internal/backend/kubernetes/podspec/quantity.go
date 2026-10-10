package podspec

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Quantities: Kubernetes' (the connection's settings, node allocatable,
// LimitRange defaults) are read into exact rationals and compared or written
// back in base units, bytes for memory and millicores for CPU. Docker's own
// sizes (-m 4g, --shm-size 256m) are 1024-based, whatever their suffix.

// quantityRE is a Kubernetes quantity: a signed decimal number and a suffix
// (binary Ki..Ei, decimal n u m k M G T P E, or an exponent e<n> / E<n>).
var quantityRE = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$`)

// parseQuantity reads a Kubernetes quantity exactly.
func parseQuantity(s string) (*big.Rat, error) {
	m := quantityRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, fmt.Errorf("%q is not a Kubernetes quantity", s)
	}
	v, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return nil, fmt.Errorf("%q is not a Kubernetes quantity", s)
	}
	suffix := m[2]
	switch {
	case suffix == "":
	case len(suffix) == 2 && suffix[1] == 'i':
		shift := uint(10 * (strings.IndexByte("KMGTPE", suffix[0]) + 1))
		v.Mul(v, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift)))
	case len(suffix) > 1 && (suffix[0] == 'e' || suffix[0] == 'E'):
		exp, err := strconv.Atoi(suffix[1:])
		if err != nil || exp > 30 || exp < -30 {
			return nil, fmt.Errorf("%q is not a Kubernetes quantity", s)
		}
		v.Mul(v, pow10(exp))
	default:
		exp := map[string]int{"n": -9, "u": -6, "m": -3, "k": 3, "M": 6, "G": 9, "T": 12, "P": 15, "E": 18}[suffix]
		v.Mul(v, pow10(exp))
	}
	return v, nil
}

// pow10 is 10^exp as a rational.
func pow10(exp int) *big.Rat {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exp))), nil)
	if exp < 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), p)
	}
	return new(big.Rat).SetInt(p)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ceilInt64 rounds a non-negative rational up to an int64.
func ceilInt64(v *big.Rat) (int64, error) {
	if v.Sign() < 0 {
		return 0, fmt.Errorf("%s is negative", v.FloatString(3))
	}
	q, r := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%s is too large", v.FloatString(0))
	}
	return q.Int64(), nil
}

// quantityBytes reads a memory or storage quantity in bytes, rounded up.
func quantityBytes(s string) (int64, error) {
	v, err := parseQuantity(s)
	if err != nil {
		return 0, err
	}
	return ceilInt64(v)
}

// quantityMillis reads a CPU quantity in millicores, rounded up.
func quantityMillis(s string) (int64, error) {
	v, err := parseQuantity(s)
	if err != nil {
		return 0, err
	}
	return ceilInt64(v.Mul(v, big.NewRat(1000, 1)))
}

// formatBytes writes a byte count as Kubernetes would: in the largest binary
// unit that divides it exactly, else in bytes.
func formatBytes(n int64) string {
	for _, u := range []struct {
		suffix string
		shift  uint
	}{{"Ei", 60}, {"Pi", 50}, {"Ti", 40}, {"Gi", 30}, {"Mi", 20}, {"Ki", 10}} {
		if n != 0 && n%(int64(1)<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// formatMillis writes millicores as Kubernetes would: whole cores when it
// can, else millicores.
func formatMillis(m int64) string {
	if m%1000 == 0 {
		return strconv.FormatInt(m/1000, 10)
	}
	return strconv.FormatInt(m, 10) + "m"
}

// dockerSizeRE is docker's size syntax (go-units RAMInBytes): a decimal
// number, then an optional unit letter, an optional i and an optional b, in
// either case. Every unit is 1024-based.
var dockerSizeRE = regexp.MustCompile(`^(\d+(?:\.\d+)*) ?([kKmMgGtTpP])?[iI]?[bB]?$`)

// dockerBytes reads a docker size (-m 4g, --shm-size 256m) in bytes.
func dockerBytes(s string) (int64, error) {
	m := dockerSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	v, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	if m[2] != "" {
		shift := uint(10 * (strings.IndexByte("kmgtp", strings.ToLower(m[2])[0]) + 1))
		v.Mul(v, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift)))
	}
	// go-units truncates the product to an int64.
	q := new(big.Int).Quo(v.Num(), v.Denom())
	if !q.IsInt64() {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	return q.Int64(), nil
}

// dockerCPUMillis reads docker's --cpus (a decimal number of CPUs) in
// millicores, rounded up.
func dockerCPUMillis(s string) (int64, error) {
	v, ok := new(big.Rat).SetString(s)
	if !ok || v.Sign() < 0 {
		return 0, fmt.Errorf("invalid CPU count: %q", s)
	}
	return ceilInt64(v.Mul(v, big.NewRat(1000, 1)))
}

// devcontainerSizeRE is the devcontainer CLI's hostRequirements size syntax.
var devcontainerSizeRE = regexp.MustCompile(`^(\d+)([tgmk]b)?$`)

// devcontainerBytes reads a hostRequirements memory or storage size as the
// devcontainer CLI does: a byte count, optionally with tb, gb, mb or kb
// (1024-based).
func devcontainerBytes(s string) (int64, error) {
	m := devcontainerSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	if m[2] != "" {
		shift := uint(10 * (strings.IndexByte("kmgt", m[2][0]) + 1))
		if n > (1<<62)>>shift {
			return 0, fmt.Errorf("invalid size: %q", s)
		}
		n <<= shift
	}
	return n, nil
}
