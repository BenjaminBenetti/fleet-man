package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// Armada is the user's registry of remote fleetd connections (gateway URL +
// bearer token) the TUI can switch between at runtime. It lives in its own
// file rather than config.json for two reasons: the TUI persists it via
// dedicated RPCs that always target the LOCAL daemon (a remote daemon must
// never see the registry, and full-replace SetConfig from an older client
// would silently wipe a Config-resident list), and it holds bearer tokens, so
// it is written 0600 where config.json is 0644.
type Armada struct {
	Remotes []ArmadaRemote `json:"remotes,omitempty"`
}

// ArmadaRemote is one registered remote fleet.
type ArmadaRemote struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// applyDefaults normalises the armada in place: entries are trimmed and
// entries without a URL are dropped (a token alone identifies nothing).
func (a *Armada) applyDefaults() {
	if a == nil {
		return
	}
	kept := a.Remotes[:0]
	for _, r := range a.Remotes {
		r.URL = strings.TrimSpace(r.URL)
		r.Token = strings.TrimSpace(r.Token)
		if r.URL != "" {
			kept = append(kept, r)
		}
	}
	a.Remotes = kept
}

// ArmadaPath returns the path to the armada registry file.
func ArmadaPath() string {
	return filepath.Join(FleetDir(), "armada.json")
}

// LoadArmada reads the armada registry from disk. Returns an empty registry if
// the file doesn't exist.
func LoadArmada() (*Armada, error) {
	a := &Armada{}

	data, err := os.ReadFile(ArmadaPath())
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading armada file: %w", err)
	}

	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("parsing armada file: %w", err)
	}

	a.applyDefaults()
	return a, nil
}

// SaveArmada writes the armada registry to disk, 0600 because it carries
// bearer tokens. The write is atomic (temp file + rename) so a concurrent
// reader (GetArmada serves clients without a read lock) never observes a
// torn/partial file, and a crash mid-write can't leave the token store
// truncated. The rename also re-applies 0600 to any pre-existing file.
func SaveArmada(a *Armada) error {
	if a == nil {
		a = &Armada{}
	}
	a.applyDefaults()

	dir := FleetDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating fleet dir: %w", err)
	}

	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling armada: %w", err)
	}

	// 0600: the armada file embeds bearer tokens. atomicWriteFile keeps the
	// temp+rename guarantee GetArmada relies on (it serves clients unlocked).
	if err := atomicfile.Write(ArmadaPath(), data, 0600); err != nil {
		return fmt.Errorf("writing armada file: %w", err)
	}

	return nil
}
