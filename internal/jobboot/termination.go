package jobboot

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// DefaultTerminationLog is the kubelet's default terminationMessagePath.
const DefaultTerminationLog = "/dev/termination-log"

// MaxTermination is the kubelet's cap on a container's termination message.
const MaxTermination = 4096

// maxTerminationError caps the error a Termination carries, so the encoded
// message always fits MaxTermination with room to spare.
const maxTerminationError = 1024

// Termination is the termination message a Job's container leaves: whether the
// job succeeded, the sha256 of its result file on the PVC (the seed's
// manifest), the bytes it wrote and, on failure, a short error. The full
// result lives on the PVC and in the Result fleetd collected; this is what a
// daemon that missed it can still read from the pod's status.
type Termination struct {
	OK     bool   `json:"ok"`
	SHA256 string `json:"sha,omitempty"`
	Bytes  int64  `json:"bytes"`
	Error  string `json:"error,omitempty"`
}

// Encode is t as the termination message: JSON, with Error cut to fit.
func (t Termination) Encode() []byte {
	t.Error = truncateUTF8(t.Error, maxTerminationError)
	data, _ := json.Marshal(t)
	return data
}

// ParseTermination reads a termination message Encode wrote.
func ParseTermination(msg string) (Termination, error) {
	var t Termination
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg)), &t); err != nil {
		return Termination{}, fmt.Errorf("parse job termination message: %w", err)
	}
	return t, nil
}

// WriteTermination writes t to the termination log at path. "" is the
// kubelet's DefaultTerminationLog, which is written only when it exists (the
// kubelet mounts it into every container), so a job run outside a pod leaves
// no stray file; a named path is created.
func WriteTermination(path string, t Termination) error {
	flags := os.O_WRONLY | os.O_TRUNC | os.O_CREATE
	if path == "" {
		path, flags = DefaultTerminationLog, os.O_WRONLY|os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(t.Encode())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
