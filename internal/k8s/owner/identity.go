// Package owner is how a daemon claims what it created in its clusters.
//
// Every object fleet creates carries the daemon's owner label
// (names.Owner(daemon_id)), and names are derived from the daemon id too. A
// restored or copied ~/.fleet therefore makes a second daemon that believes it
// owns the first one's objects. The incarnation tells the two apart: a nonce
// kept in ~/.fleet/kube/incarnation.json beside a fingerprint of the machine
// (machine id and hostname), regenerated whenever the fingerprint differs. A
// per-connection Lease (lease.go) records which incarnation may adopt and
// garbage-collect the connection's objects; another live incarnation holding
// it is OWNER_CONFLICT.
//
// The identity half is plain Go; the Lease half needs client-go and is left
// out of the -tags nok8s pod build.
package owner

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Identity is who the daemon is to its clusters.
type Identity struct {
	// DaemonID is the daemon's 128-bit id in 32 lowercase hex digits
	// (~/.fleet/daemon_id). Names, keys and the owner label derive from it.
	DaemonID string
	// Nonce tells this machine's incarnation of the daemon from one running
	// on a copy of its ~/.fleet elsewhere.
	Nonce string
	// Host is this machine's hostname, shown to the other incarnation.
	Host string
}

// Owner is the owner label value of every object the daemon creates.
func (id Identity) Owner() string { return names.Owner(id.DaemonID) }

// Incarnation is the holder identity this daemon writes into its owner
// Leases: "<owner>-<nonce>".
func (id Identity) Incarnation() string { return id.Owner() + "-" + id.Nonce }

var (
	daemonIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	noncePattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// incarnationFile is ~/.fleet/kube/incarnation.json.
type incarnationFile struct {
	V int `json:"v"`
	// Nonce is the incarnation nonce (16 hex digits).
	Nonce string `json:"nonce"`
	// Fingerprint is a hash of the machine id and hostname the nonce was made
	// on; the raw machine id is never stored.
	Fingerprint string `json:"fingerprint"`
}

// Test seams.
var (
	machineIDFunc = machineID
	hostnameFunc  = os.Hostname
)

var identityMu sync.Mutex

// LoadIdentity returns the daemon's identity, creating ~/.fleet/daemon_id on
// first use and a new incarnation nonce whenever this machine's fingerprint
// differs from the one the stored nonce was made on. A daemon_id that exists
// but is malformed is an error, never replaced: a new id would orphan every
// object the daemon owns.
func LoadIdentity() (Identity, error) {
	identityMu.Lock()
	defer identityMu.Unlock()

	daemonID, err := loadOrCreateDaemonID()
	if err != nil {
		return Identity{}, err
	}
	host, _ := hostnameFunc()
	nonce, err := loadOrCreateNonce(fingerprint(machineIDFunc(), host))
	if err != nil {
		return Identity{}, err
	}
	return Identity{DaemonID: daemonID, Nonce: nonce, Host: host}, nil
}

func loadOrCreateDaemonID() (string, error) {
	path := state.DaemonIDPath()
	if id, err := readDaemonID(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("creating fleet dir: %w", err)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	if err := createExclusive(path, []byte(hex.EncodeToString(b)+"\n")); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("writing daemon id: %w", err)
	}
	// Whoever won a race to create it, the file now holds the id.
	return readDaemonID(path)
}

func readDaemonID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !daemonIDPattern.MatchString(id) {
		return "", fmt.Errorf("%s does not hold a daemon id (32 lowercase hex digits); restore it from a backup, since a new id would orphan this daemon's cluster objects", path)
	}
	return id, nil
}

// createExclusive writes data to path only if path does not exist yet, and
// atomically: a temp file hard-linked into place, so a reader (the backup
// loop) never sees a partial file and a concurrent creator never overwrites.
func createExclusive(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

func loadOrCreateNonce(fp string) (string, error) {
	path := state.KubeIncarnationPath()
	if data, err := os.ReadFile(path); err == nil {
		var f incarnationFile
		if json.Unmarshal(data, &f) == nil && f.Fingerprint == fp && noncePattern.MatchString(f.Nonce) {
			return f.Nonce, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading incarnation: %w", err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	f := incarnationFile{V: 1, Nonce: hex.EncodeToString(b), Fingerprint: fp}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("creating kube dir: %w", err)
	}
	if err := atomicfile.Write(path, data, 0600); err != nil {
		return "", fmt.Errorf("writing incarnation: %w", err)
	}
	return f.Nonce, nil
}

// fingerprint hashes the machine id and hostname (never stored raw: systemd
// asks that the machine id not be exposed).
func fingerprint(machineID, host string) string {
	sum := sha256.Sum256([]byte("fleet incarnation\x00" + machineID + "\x00" + host))
	return hex.EncodeToString(sum[:16])
}

// machineID reads the OS's stable machine identifier, or "" where there is
// none (the hostname alone then fingerprints the machine).
func machineID() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return ""
		}
		return parseIOPlatformUUID(string(out))
	case "freebsd":
		return readFirst("/etc/hostid")
	default:
		return readFirst("/etc/machine-id", "/var/lib/dbus/machine-id")
	}
}

var ioPlatformUUID = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

func parseIOPlatformUUID(ioreg string) string {
	if m := ioPlatformUUID.FindStringSubmatch(ioreg); m != nil {
		return m[1]
	}
	return ""
}

func readFirst(paths ...string) string {
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return ""
}
