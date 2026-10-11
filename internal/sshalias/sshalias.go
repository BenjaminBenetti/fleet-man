// Package sshalias is the grammar of the ssh host names VS Code Remote-SSH
// opens fleet instances through: `<instance>.<fleet>[.r<h6>].fleet`, which
// ~/.ssh/fleet_config routes through `fleet ssh-proxy %n` to the instance's
// stdio-only `fleet ssh-server`. The daemon names an instance's local alias
// (the Kubernetes EditorURI), the clients add the remote part for a remote
// daemon and write fleet_config, and ssh-proxy parses the alias strictly
// before it dials anything. Pure: no I/O.
package sshalias

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// Suffix ends every alias; HostPattern is the ssh config Host line that
// matches them all.
const (
	Suffix      = ".fleet"
	HostPattern = "*" + Suffix
)

// ConfigFileName is fleet's ssh config file in ~/.ssh, and IncludeLine the
// one line of ~/.ssh/config that pulls it in (ssh resolves a relative Include
// against ~/.ssh).
const (
	ConfigFileName = "fleet_config"
	IncludeLine    = "Include " + ConfigFileName
)

// ProxySubcommand is the hidden fleet command OpenSSH runs for an alias, and
// ServerSubcommand the one ssh-proxy runs inside the instance.
const (
	ProxySubcommand  = "ssh-proxy"
	ServerSubcommand = "ssh-server"
)

// ServerArgv is the command line ssh-proxy runs in the instance over the
// daemon's Exec stream: the staged fleet binary (fleetlaunch.RemotePath)
// serving one SSH connection on its stdio. The daemon knows an editor
// session by it.
func ServerArgv() []string { return []string{"/usr/bin/fleet", ServerSubcommand, "--stdio"} }

// aliasRE is the strict grammar ssh-proxy accepts: two name parts, each
// plain or x-hex, an optional remote part, the suffix.
var aliasRE = regexp.MustCompile(`^([A-Za-z0-9_-]+|x[0-9a-f]+)\.([A-Za-z0-9_-]+|x[0-9a-f]+)(\.r[0-9a-f]{6})?\.fleet$`)

// remoteRE is a remote part on its own.
var remoteRE = regexp.MustCompile(`^r[0-9a-f]{6}$`)

// plainRE is a name Alias writes as is: lower case only, so an editor that
// lowercases a URI's host (VS Code does) or ssh's case-insensitive matching
// never changes which instance an alias names.
var plainRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// hexRE is a part that decodes as hex: an x and whole bytes. A name that
// would read as one is hex-encoded itself, so every hex part decodes.
var hexRE = regexp.MustCompile(`^x(?:[0-9a-f]{2})+$`)

// Target is what an alias names.
type Target struct {
	Fleet    string
	Instance string
	// Remote is the remote daemon's part ("r" + six hex digits; RemoteHash),
	// "" for the local daemon.
	Remote string
}

// RemoteHash is the remote part of the aliases of the instances of the
// daemon a client reaches through key: "r" and the first six hex digits of
// its SHA-256. key is the gateway or ssh:// URL, or "server:" and the
// FLEET_SERVER address (fleetclient.Remote.Key).
func RemoteHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "r" + hex.EncodeToString(sum[:])[:6]
}

// RemoteKey is what RemoteHash hashes for a remote daemon reached at
// address: a gateway or ssh:// URL as is, a FLEET_SERVER host:port (server)
// behind "server:", so an address never reads as a URL.
func RemoteKey(server bool, address string) string {
	if server {
		return "server:" + address
	}
	return address
}

// ValidRemote reports whether s is a remote part.
func ValidRemote(s string) bool { return remoteRE.MatchString(s) }

// Alias is the alias of instance in fleetName, on the daemon whose remote
// part is remote ("" for the local daemon).
func Alias(fleetName, instance, remote string) string {
	a := encodePart(instance) + "." + encodePart(fleetName)
	if remote != "" {
		a += "." + remote
	}
	return a + Suffix
}

// Parse reads an alias, refusing anything outside the grammar, or naming an
// invalid fleet or instance, before anything is dialled.
func Parse(alias string) (Target, error) {
	m := aliasRE.FindStringSubmatch(alias)
	if m == nil {
		return Target{}, fmt.Errorf("%q is not a fleet ssh alias (<instance>.<fleet>[.r<hash>]%s)", alias, Suffix)
	}
	t := Target{Instance: decodePart(m[1]), Fleet: decodePart(m[2]), Remote: strings.TrimPrefix(m[3], ".")}
	for _, name := range []string{t.Instance, t.Fleet} {
		if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
			return Target{}, fmt.Errorf("alias %q names %q, which is not a fleet or instance name", alias, name)
		}
	}
	if err := fleet.ValidateInstanceName(t.Instance); err != nil {
		return Target{}, fmt.Errorf("alias %q: %w", alias, err)
	}
	if err := fleet.ValidateFleetName(t.Fleet); err != nil {
		return Target{}, fmt.Errorf("alias %q: %w", alias, err)
	}
	return t, nil
}

// URI is the VS Code URI that opens folder inside the instance alias names.
func URI(alias, folder string) string {
	if !strings.HasPrefix(folder, "/") {
		folder = "/" + folder
	}
	return "vscode-remote://ssh-remote+" + alias + (&url.URL{Path: folder}).EscapedPath()
}

// Config is fleet_config: every alias goes through `<exe> ssh-proxy %n`.
// Host keys are not checked: each `fleet ssh-server` makes a fresh one, and
// the proxy has already authenticated the daemon it reached. No token is ever
// written here (ssh-proxy finds the remote's through the local daemon).
func Config(exe string) string {
	// ssh hands ProxyCommand to the shell after expanding its % tokens.
	cmd := strings.ReplaceAll(shellquote.CommandWord(exe), "%", "%%")
	return "# Written by fleet (fleet code): VS Code Remote-SSH reaches fleet instances\n" +
		"# through these hosts. Fleet rewrites this file; edit ~/.ssh/config instead.\n" +
		"Host " + HostPattern + "\n" +
		"  ProxyCommand " + cmd + " " + ProxySubcommand + " %n\n" +
		"  StrictHostKeyChecking no\n" +
		"  UserKnownHostsFile /dev/null\n"
}

// HasInclude reports whether an ssh config (its text) already includes
// fleet_config.
func HasInclude(config string) bool {
	for _, line := range strings.Split(config, "\n") {
		fields := strings.Fields(strings.ReplaceAll(line, "=", " "))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "include") {
			continue
		}
		for _, f := range fields[1:] {
			f = strings.Trim(f, `"`)
			if f == ConfigFileName || strings.HasSuffix(f, "/"+ConfigFileName) {
				return true
			}
		}
	}
	return false
}

// WithInclude is an ssh config (its text) with IncludeLine added at the top,
// where it applies to every host (an Include inside a Host or Match block
// applies to that block's hosts only).
func WithInclude(config string) string {
	if HasInclude(config) {
		return config
	}
	if strings.TrimSpace(config) == "" {
		return IncludeLine + "\n"
	}
	return IncludeLine + "\n\n" + config
}

func encodePart(name string) string {
	if plainRE.MatchString(name) && !hexRE.MatchString(name) {
		return name
	}
	return "x" + hex.EncodeToString([]byte(name))
}

func decodePart(part string) string {
	if !hexRE.MatchString(part) {
		return part
	}
	b, err := hex.DecodeString(part[1:])
	if err != nil {
		return part
	}
	return string(b)
}
