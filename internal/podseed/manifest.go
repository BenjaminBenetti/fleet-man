package podseed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ManifestVersion is the meta/rootfs.json version this fleet writes. Readers
// take any version up to theirs; a newer one is read as far as it goes and
// flagged with ErrNewerManifest, read-only.
const ManifestVersion = 1

// maxWarnings caps the warnings a manifest keeps.
const maxWarnings = 50

// ErrNewerManifest is a manifest written by a newer fleet: its known fields are
// usable, but nothing should rewrite it.
var ErrNewerManifest = errors.New("the root manifest was written by a newer fleet")

// Manifest is meta/rootfs.json: what a seed made of one root generation.
type Manifest struct {
	V   int `json:"v"`
	Gen int `json:"gen"`
	// SeededAt is when the seed finished.
	SeededAt time.Time `json:"seeded_at"`
	// Dirs are the image's top-level dirs persisted under root/, sorted:
	// the instance pod mounts each root/<d> at /<d>.
	Dirs []string `json:"dirs"`
	// RootExtra are the top-level dirs under root-extra/, sorted. Neither
	// list holds a dir that a mount target covers whole.
	RootExtra []string `json:"root_extra,omitempty"`
	// Volumes are the volumes this seed copied up (new ones only).
	Volumes []string `json:"volumes,omitempty"`
	// Files, Bytes and Hardlinks count what the root copy wrote.
	Files     int64 `json:"files"`
	Bytes     int64 `json:"bytes"`
	Hardlinks int64 `json:"hardlinks,omitempty"`
	// Capabilities and Setuid count the files carrying file capabilities
	// and setuid/setgid bits; the self-check verified each one.
	Capabilities int64 `json:"capabilities,omitempty"`
	Setuid       int64 `json:"setuid,omitempty"`
	// SingleUID marks a seed run without root (capability tier C): owners
	// are the pod uid's, device nodes and capabilities are not kept.
	SingleUID bool `json:"single_uid,omitempty"`
	// Fallback lists the top-level dirs copied with GNU cp after the Go
	// copier failed on them.
	Fallback []string `json:"fallback,omitempty"`
	// Remap is what the share-uid remap did.
	Remap *RemapResult `json:"remap,omitempty"`
	// Binary is the sha256 of the fleet binary the seed installed.
	Binary string `json:"binary,omitempty"`
	// SeedCache marks a seed cache (OpSeedCache): this disk holds only the
	// generation's root/ and this manifest, and its id is SeedCache.
	SeedCache string `json:"seed_cache,omitempty"`
	// FromCache is the seed cache this generation's root/ was cloned from
	// (SeedSpec.Cache); "" when the seed copied the root itself.
	FromCache string `json:"from_cache,omitempty"`
	// Warnings are what the seed could not keep (at most 50).
	Warnings []string `json:"warnings,omitempty"`
}

// addWarning records a warning, keeping at most maxWarnings.
func (m *Manifest) addWarning(format string, args ...any) {
	if len(m.Warnings) < maxWarnings {
		m.Warnings = append(m.Warnings, fmt.Sprintf(format, args...))
	} else if len(m.Warnings) == maxWarnings {
		m.Warnings = append(m.Warnings, "more warnings not recorded")
	}
}

// ReadManifest reads the manifest at file. A manifest from a newer fleet is
// returned with ErrNewerManifest.
func ReadManifest(file string) (Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

// ParseManifest parses a manifest (what ReadManifest reads, or a seed's
// jobboot Result output).
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse root manifest: %w", err)
	}
	switch {
	case m.V < 1:
		return Manifest{}, fmt.Errorf("root manifest has no version")
	case m.V > ManifestVersion:
		return m, fmt.Errorf("%w (version %d, this fleet reads %d)", ErrNewerManifest, m.V, ManifestVersion)
	}
	return m, nil
}

// encode is m as written to the PVC, and its sha256. It is compact JSON, the
// form a jobboot Result's output arrives in too, so both have the sha the
// termination message carries.
func (m Manifest) encode() ([]byte, string) {
	data, _ := json.Marshal(m)
	return data, sha256Hex(data)
}

// sha256Hex is data's sha256, hex.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeDurable writes data to file through a synced temp file and a rename,
// then syncs the dir, so a crash leaves either the old file or the new one.
func writeDurable(file string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, file); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory, making the renames in it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
