package debcache

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
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

// ProxyDetector is the command an instance's apt runs for its http proxy when
// the cache is a Proxy.Detect one (Acquire::http::Proxy-Auto-Detect; apt runs
// it as its _apt user, once per host and run, with the URL to fetch). The
// script asks the staged fleet binary (`fleet net-probe`) whether the cache
// answers and prints the cache's URL, else DIRECT, which it also prints when
// that binary is missing or fails, so apt never fails for want of an answer.
// It sits beside apt's config, not in apt.conf.d, every file of which apt
// reads as configuration.
const ProxyDetector = "/etc/apt/fleet-proxy-detect"

// Proxy is how an instance's apt reaches the fleet's deb cache.
type Proxy struct {
	// URL is the cache's HTTP proxy URL.
	URL string
	// Detect has apt check the cache answers before it uses it, and go
	// direct when it does not (ProxyDetector), for a cache that may be down
	// while the instance is up (fleet.ServiceEndpoint.Fallback); otherwise
	// apt always goes through it (Acquire::http::Proxy).
	Detect bool
}

// ConfigureInstanceApt points an instance's apt at the fleet's shared deb cache
// by writing an http-proxy drop-in (and, for a Detect proxy, ProxyDetector). It
// first probes for apt AND a writable apt.conf.d (directly or via passwordless
// sudo); if either is missing it returns (false, nil) WITHOUT touching the
// instance — the documented "do nothing, no error" behaviour for images that
// lack apt or are not configurable. When usable it writes the proxy config
// (overwriting any prior one, so it is idempotent) and returns configured=true.
//
// The configured bool reports whether the instance's apt was actually pointed at
// the cache, letting callers log only the real wiring. Callers should treat a
// returned error as non-fatal (warn and continue): a cache-wiring failure must
// not block an otherwise-usable instance.
func ConfigureInstanceApt(b instanceExecer, workspaceDir string, proxy Proxy) (configured bool, err error) {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", probeScript()}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("probe apt: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !aptPresent(string(out)) {
		// apt absent or apt.conf.d not writable — silently skip per the contract.
		return false, nil
	}

	script := configureScript(proxy.URL)
	if proxy.Detect {
		script = detectScript(proxy.URL)
	}
	out, err = b.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput()
	if err != nil {
		return true, fmt.Errorf("configure apt proxy: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return true, nil
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

// detectScript writes ProxyDetector, asking whether proxyURL answers, then
// the drop-in naming it as apt's http proxy detector (only http is proxied,
// as in configureScript). The detector goes in first, by temp file and
// rename, so apt never runs a half-written one; the content is fixed text
// around a shell-safe URL (a sanitized Service name), in a quoted
// here-document so nothing in it expands.
func detectScript(proxyURL string) string {
	detector := fmt.Sprintf(`#!/bin/sh
# Written by fleet: apt's proxy for http sources is the fleet's deb cache,
# %[1]s, while it answers; else none.
p=$(%[2]s net-probe '%[1]s' 2>/dev/null) || p=
case "$p" in
http://*|https://*) echo "$p" ;;
*) echo DIRECT ;;
esac
`, proxyURL, fleetlaunch.RemotePath)
	conf := fmt.Sprintf(`Acquire::http::Proxy-Auto-Detect "%s";`, ProxyDetector)
	return fmt.Sprintf(`set -e
SUDO=
if [ ! -w /etc/apt ] || [ ! -w /etc/apt/apt.conf.d ]; then SUDO=sudo; fi
$SUDO tee '%[1]s.fleet.tmp' >/dev/null <<'FLEET_DETECTOR'
%[2]sFLEET_DETECTOR
$SUDO chmod 0755 '%[1]s.fleet.tmp'
$SUDO mv -f '%[1]s.fleet.tmp' '%[1]s'
printf '%%s\n' '%[3]s' | $SUDO tee '%[4]s' >/dev/null
`, ProxyDetector, detector, conf, proxyConfFile)
}

// aptPresent reports whether the probe output indicates apt is configurable. It
// scans for the PRESENT marker as a substring so leading warnings on the
// combined stream don't mask a positive result.
func aptPresent(probeOutput string) bool {
	return strings.Contains(probeOutput, probeMarkerPresent)
}
