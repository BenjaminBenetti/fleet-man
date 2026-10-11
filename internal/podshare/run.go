package podshare

import (
	"fmt"
	"os"
	"unicode/utf8"
)

// maxTerminationMessage keeps a failed replay's reason under the kubelet's
// 4KiB termination message.
const maxTerminationMessage = 2048

// RunLayout is `fleet pod-layout`, the share-layout init container: it reads
// the layout at spec, makes SyncDir root's alone and replays the layout on
// the tree mounted at MountDir. A failure's reason also goes to termLog (the
// container's termination message, which the pod's status shows) when it is
// set.
func RunLayout(spec, termLog string) error {
	err := runLayout(spec)
	if err != nil && termLog != "" {
		msg := "fleet share layout: " + err.Error()
		for len(msg) > maxTerminationMessage || !utf8.ValidString(msg) {
			msg = msg[:min(len(msg)-1, maxTerminationMessage)]
		}
		_ = os.WriteFile(termLog, []byte(msg), 0o644)
	}
	return err
}

func runLayout(spec string) error {
	l, err := LoadLayout(spec)
	if err != nil {
		return err
	}
	if err := PrivateSyncDir(SyncDir); err != nil {
		return err
	}
	if err := Apply(MountDir, l); err != nil {
		return fmt.Errorf("laying out the fleet's share: %w", err)
	}
	return nil
}
