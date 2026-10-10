//go:build linux

package podagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// VersionInfo is the "version" RPC's result.
type VersionInfo struct {
	SelfVersionInfo
	Role  podwire.Role `json:"role"`
	PID   int          `json:"pid"`
	Proto int          `json:"proto"`
}

// HeldInfo is the "held" RPC's result: the pod's held commands.
type HeldInfo struct {
	Count int `json:"count"`
	Max   int `json:"max"`
}

// methods are the RPCs the agent serves: the built-in ones, then Options'.
func (a *agent) methods() map[string]Method {
	m := map[string]Method{
		"version": func(context.Context, json.RawMessage) (any, error) {
			return VersionInfo{SelfVersionInfo: SelfVersion(), Role: a.opts.Role, PID: os.Getpid(), Proto: a.proto}, nil
		},
		"held": func(context.Context, json.RawMessage) (any, error) {
			n := 0
			if entries, err := os.ReadDir(filepath.Join(a.opts.RunDir, heldDirName)); err == nil {
				for _, e := range entries {
					if strings.HasSuffix(e.Name(), heldSuffix) {
						n++
					}
				}
			}
			return HeldInfo{Count: n, Max: a.opts.MaxHeld}, nil
		},
	}
	for name, fn := range a.opts.Methods {
		m[name] = fn
	}
	return m
}

// serveRPC answers one call: the result's JSON, then EXIT.
func (a *agent) serveRPC(s *podwire.Stream) {
	fail := func(code, msg string) { _ = s.W.WriteJSON(podwire.FrameExit, podwire.Result{Code: code, Error: msg}) }
	var h podwire.RPCHeader
	if err := podwire.DecodeHeader(s.Header, &h); err != nil {
		s.Penalize()
		fail(podwire.CodeInvalid, err.Error())
		return
	}
	method, ok := a.rpc[h.Method]
	if !ok {
		fail(podwire.CodeUnknownMethod, fmt.Sprintf("no rpc %q", h.Method))
		return
	}
	result, err := method(a.ctx, h.Args)
	if err != nil {
		fail(podwire.CodeFailed, err.Error())
		return
	}
	body, err := json.Marshal(result)
	if err != nil {
		fail(podwire.CodeFailed, "encode result: "+err.Error())
		return
	}
	if len(body) > podwire.MaxReply {
		fail(podwire.CodeFailed, fmt.Sprintf("result is %d bytes, over %d", len(body), podwire.MaxReply))
		return
	}
	if err := s.W.WriteData(podwire.FrameData, body); err != nil {
		return
	}
	_ = s.W.WriteJSON(podwire.FrameExit, podwire.Result{})
}
