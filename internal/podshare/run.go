package podshare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unicode/utf8"
)

// maxTerminationMessage keeps a failed replay's reason under the kubelet's
// 4KiB termination message.
const maxTerminationMessage = 2048

// RunLayout is `fleet pod-layout`, the share-layout init container: it reads
// the layout at spec, makes SyncDir root's alone (made when missing, so the
// dev container's mount of the tree lands inside it), makes the tree's
// folder tree when missing (the copy tier's on a new disk, or one a clone
// left out), and replays the layout there. A failure's reason also goes to
// termLog (the container's termination message, which the pod's status
// shows) when it is set.
func RunLayout(spec, tree, termLog string) error {
	err := runLayout(spec, SyncDir, tree)
	if err != nil && termLog != "" {
		msg := "fleet share layout: " + err.Error()
		for len(msg) > maxTerminationMessage || !utf8.ValidString(msg) {
			msg = msg[:min(len(msg)-1, maxTerminationMessage)]
		}
		_ = os.WriteFile(termLog, []byte(msg), 0o644)
	}
	return err
}

func runLayout(spec, syncDir, tree string) error {
	l, err := LoadLayout(spec)
	if err != nil {
		return err
	}
	if err := PrivateSyncDir(syncDir); err != nil {
		return err
	}
	if err := ensureTree(tree); err != nil {
		return err
	}
	if err := Apply(tree, l); err != nil {
		return fmt.Errorf("laying out the fleet's share: %w", err)
	}
	return nil
}

// ensureTree makes the tree's folder when it is missing; one that is not a
// folder is refused (only fleet writes the volume's root).
func ensureTree(tree string) error {
	info, err := os.Lstat(tree)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(tree, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("making the fleet's share folder %s: %w", tree, err)
		}
		return nil
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("the fleet's share folder %s is a %s, not a folder", tree, kind(info))
	}
	return nil
}
