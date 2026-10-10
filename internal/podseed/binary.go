package podseed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FileSHA256 is the sha256 of the file at name, hex.
func FileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// installBinary installs the fleet binary at src (whose sha256 is sha) into
// binDir as fleet-<sha> and points the `fleet` link at it: the stage the
// supervisor does on a running pod (temp file, check, rename, link swap), done
// by the seed so the first start has a binary. The previous link target is
// kept for pod-init's fallback; older binaries are deleted. It returns the
// previous target's sha ("" without one).
func installBinary(src, sha, binDir string) (prev string, err error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	name := BinaryName(sha)
	target := filepath.Join(binDir, name)
	if got, err := FileSHA256(target); err != nil || got != sha {
		if err := copyBinary(src, sha, target); err != nil {
			return "", err
		}
	}

	link := filepath.Join(binDir, "fleet")
	if old, err := os.Readlink(link); err == nil {
		prev = strings.TrimPrefix(old, "fleet-")
	}
	if prev != sha {
		tmp := filepath.Join(binDir, ".fleet.new")
		_ = os.Remove(tmp)
		if err := os.Symlink(name, tmp); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, link); err != nil {
			return "", fmt.Errorf("point %s at %s: %w", link, name, err)
		}
	} else {
		prev = ""
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return prev, err
	}
	for _, e := range entries {
		n := e.Name()
		keep := n == "fleet" || n == name || (prev != "" && n == BinaryName(prev))
		if !keep && (strings.HasPrefix(n, "fleet-") || strings.HasPrefix(n, ".stage.")) {
			_ = os.Remove(filepath.Join(binDir, n))
		}
	}
	return prev, syncDir(binDir)
}

// copyBinary copies src to target through a temp file, checking its sha.
func copyBinary(src, sha, target string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("read the fleet binary: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".stage.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy the fleet binary: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		_ = tmp.Close()
		return fmt.Errorf("the fleet binary's sha256 is %s, not %s", got, sha)
	}
	if err := tmp.Chmod(0o755); err != nil {
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
	return os.Rename(tmp.Name(), target)
}

// linkFleet makes /usr/bin/fleet in a new root generation a symlink to
// CurrentBinary, replacing whatever the image had there (docker's EnsureFresh
// overwrites it too). It reports false when the image has no /usr/bin.
func linkFleet(g genRoot) (bool, error) {
	dir, err := g.resolveDir(path.Dir(FleetLink))
	if err != nil {
		return false, nil
	}
	link := filepath.Join(dir, path.Base(FleetLink))
	if fi, err := os.Lstat(link); err == nil {
		if fi.IsDir() {
			return false, fmt.Errorf("%s is a directory", FleetLink)
		}
		if err := os.Remove(link); err != nil {
			return false, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.Symlink(CurrentBinary, link); err != nil {
		return false, err
	}
	if os.Geteuid() == 0 {
		_ = os.Lchown(link, 0, 0)
	}
	return true, nil
}
