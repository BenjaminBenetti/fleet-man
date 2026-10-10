package podagent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// SelfVersionInfo is `pod-agent self-version`: what the supervisor's probe
// learns about the binary in the pod before it decides whether to restage.
type SelfVersionInfo struct {
	Version   string `json:"version"`
	SHA       string `json:"sha"`
	ProtoMin  int    `json:"proto_min"`
	ProtoMax  int    `json:"proto_max"`
	Arch      string `json:"arch"`
	UID       int    `json:"uid"`
	CanSetuid bool   `json:"can_setuid"`
}

// SelfVersion describes this binary.
func SelfVersion() SelfVersionInfo {
	return SelfVersionInfo{
		Version:   version.Version,
		SHA:       selfSHA(),
		ProtoMin:  podwire.ProtoMin,
		ProtoMax:  podwire.ProtoMax,
		Arch:      runtime.GOARCH,
		UID:       os.Geteuid(),
		CanSetuid: execas.CanSwitchUser(),
	}
}

// stagedSHA is the sha256 in a content-addressed staged binary's name,
// fleet-<sha256>; "" for any other name.
func stagedSHA(name string) string {
	sha, ok := strings.CutPrefix(name, "fleet-")
	if !ok || len(sha) != 64 {
		return ""
	}
	for i := 0; i < len(sha); i++ {
		if c := sha[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return sha
}

var (
	selfSHAOnce  sync.Once
	selfSHAValue string
)

// selfSHA is the hex sha256 of this binary: read off a content-addressed
// staged file's name, else hashed once.
func selfSHA() string {
	selfSHAOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if sha := stagedSHA(filepath.Base(exe)); sha != "" {
			selfSHAValue = sha
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			selfSHAValue = hex.EncodeToString(h.Sum(nil))
		}
	})
	return selfSHAValue
}
