package micsink

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputOnlyScriptHasNoMicrophone(t *testing.T) {
	if script := audioServerScript(false); strings.Contains(script, "module-pipe-source") || strings.Contains(script, "fleetmic") {
		t.Fatalf("output-only server creates microphone: %s", script)
	}
	if script := audioServerScript(false); !strings.Contains(script, "set-default-source fleetnull.monitor\n") {
		t.Fatalf("output-only server has no silent default source: %s", script)
	}
}

func TestOutputRequiresClientRoutingBeforeBecomingReady(t *testing.T) {
	old := outputClientConfig
	outputClientConfig = filepath.Join(t.TempDir(), "missing.conf")
	defer func() { outputClientConfig = old }()
	var out bytes.Buffer
	if err := RunOutput(context.Background(), strings.NewReader(""), &out); err == nil {
		t.Fatal("source came up before applications could discover the server")
	}
	if out.Len() != 0 {
		t.Fatal("unconfigured source announced ready")
	}
}
