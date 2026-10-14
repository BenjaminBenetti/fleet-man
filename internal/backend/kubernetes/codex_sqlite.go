//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// codex_sqlite.go is Codex's SQLite state on a fleet share mounted from
// several nodes (multi-node mode). Its databases keep a write-ahead log that
// only works on one kernel: two nodes writing one corrupts it. The pods get
// CODEX_SQLITE_HOME on the instance's own disk (podspec.CodexSQLiteHome),
// but a sqlite_home in Codex's config.toml wins over that variable, and the
// fleet's config.toml is shared by every instance: one naming a folder on
// the share puts every node's databases back there. fleet does not rewrite
// a file the user owns; it warns, naming the line to remove.

// maxCodexConfig bounds how much of the fleet's config.toml is read.
const maxCodexConfig = 1 << 20

// checkCodexSQLiteHome warns when the fleet's Codex config.toml sets a
// sqlite_home on the fleet's share (or one that is not absolute, which
// Codex resolves against where it runs).
func (c *creation) checkCodexSQLiteHome(ctx context.Context) {
	if w := codexSQLiteHomeWarning(c.req.Mounts); w != "" {
		c.warn(ctx, w)
	}
}

// codexSQLiteHomeWarning is the warning for the fleet's mounts ("" when
// none is due).
func codexSQLiteHomeWarning(mounts []backend.Mount) string {
	var codex *backend.Mount
	for i := range mounts {
		m := &mounts[i]
		if m.Scope == backend.MountScopeFleetShared && (m.SubPath == ".codex" || strings.HasSuffix(m.ContainerPath, "/.codex")) {
			codex = m
		}
	}
	if codex == nil || codex.LocalPath == "" {
		return ""
	}
	home, err := codexSQLiteHomeSetting(filepath.Join(codex.LocalPath, "config.toml"))
	if err != nil {
		flog.Warn("kube share: reading the fleet's Codex config.toml", "path", codex.LocalPath, "err", err)
		return ""
	}
	if home == "" || !onShare(home, mounts) {
		return ""
	}
	return fmt.Sprintf("Codex's config.toml sets sqlite_home = %q, which wins over the CODEX_SQLITE_HOME fleet sets on this cluster: on a fleet share mounted from several nodes Codex's SQLite databases corrupt; remove sqlite_home from the fleet's %s/config.toml (in every instance and on the daemon host)", home, codex.ContainerPath)
}

// codexSQLiteHomeSetting is the top-level sqlite_home of the Codex config at
// file ("" when it sets none, or there is no file).
func codexSQLiteHomeSetting(file string) (string, error) {
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCodexConfig))
	if err != nil {
		return "", err
	}
	var cfg struct {
		SQLiteHome string `toml:"sqlite_home"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		// A config Codex itself would refuse holds no setting to warn of.
		return "", nil
	}
	return strings.TrimSpace(cfg.SQLiteHome), nil
}

// onShare reports whether a sqlite_home of home lands on the fleet's share:
// below one of its fleet mounts, or relative or ~-based (resolved where Codex
// runs or in the home, which a fleet mount may be in).
func onShare(home string, mounts []backend.Mount) bool {
	if !path.IsAbs(home) {
		return true
	}
	home = path.Clean(home)
	for _, m := range mounts {
		if m.Scope != backend.MountScopeFleetShared || m.ContainerPath == "" {
			continue
		}
		target := path.Clean(m.ContainerPath)
		if home == target || strings.HasPrefix(home, target+"/") {
			return true
		}
	}
	return false
}
