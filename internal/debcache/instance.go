package debcache

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// instanceExecer is the slice of backend.Backend this package needs to run
// commands inside an instance. Narrowing the dependency keeps the package
// testable with a tiny fake instead of the full Backend surface.
type instanceExecer interface {
	ExecCommand(workspaceDir string, command []string) *backend.Cmd
}

// probeMarkerPresent / probeMarkerAbsent are the single-token stdout signals the
// probe script emits so the outcome is unambiguous regardless of locale or
// extra warnings on stderr.
const (
	probeMarkerPresent = "PRESENT"
	probeMarkerAbsent  = "ABSENT"
)

// proxyConfFile is the apt drop-in fleet-man writes inside an instance. The
// 01-prefix sorts it early; the fleet-proxy name makes it identifiable and lets
// re-runs overwrite it idempotently.
const proxyConfFile = "/etc/apt/apt.conf.d/01fleet-proxy"

// ConfigureInstanceApt points an instance's apt at the fleet's shared deb cache
// by writing an http-proxy drop-in. It first probes for apt AND a writable
// apt.conf.d (directly or via passwordless sudo); if either is missing it
// returns nil WITHOUT touching the instance — the documented "do nothing, no
// error" behaviour for images that lack apt or are not configurable. When usable
// it writes the proxy config (overwriting any prior one, so it is idempotent).
//
// Callers should treat a returned error as non-fatal (warn and continue): a
// cache-wiring failure must not block an otherwise-usable instance.
func ConfigureInstanceApt(b instanceExecer, workspaceDir, proxyURL string) error {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", probeScript()}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe apt: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !aptPresent(string(out)) {
		// apt absent or apt.conf.d not writable — silently skip per the contract.
		return nil
	}

	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", configureScript(proxyURL)}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("configure apt proxy: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// probeScript reports whether apt is present AND apt.conf.d is writable (either
// directly by the exec user or via passwordless sudo). It always exits 0 and
// prints exactly one marker so an exec failure is distinguishable from a clean
// "not present" answer.
func probeScript() string {
	return fmt.Sprintf(
		`if command -v apt-get >/dev/null 2>&1 && { [ -w /etc/apt/apt.conf.d ] || { command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; }; }; then echo %s; else echo %s; fi`,
		probeMarkerPresent, probeMarkerAbsent,
	)
}

// configureScript writes the apt http-proxy drop-in pointing at proxyURL. Only
// http is proxied: the default Debian/Ubuntu archives are HTTP (so the bulk of
// `apt install` is cached), while HTTPS sources keep working by going direct —
// caching HTTPS would require rewriting the instance's apt sources, which is
// fragile. It writes directly when apt.conf.d is writable, else via sudo. The
// content is a fixed, shell-safe string (the proxy host is a sanitized container
// name), single-quoted so its embedded double-quotes need no escaping.
func configureScript(proxyURL string) string {
	conf := fmt.Sprintf(`Acquire::http::Proxy "%s";`, proxyURL)
	return fmt.Sprintf(
		`if [ -w /etc/apt/apt.conf.d ]; then printf '%%s\n' '%[1]s' > %[2]s; else printf '%%s\n' '%[1]s' | sudo tee %[2]s >/dev/null; fi`,
		conf, proxyConfFile,
	)
}

// aptPresent reports whether the probe output indicates apt is configurable. It
// scans for the PRESENT marker as a substring so leading warnings on the
// combined stream don't mask a positive result.
func aptPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}
