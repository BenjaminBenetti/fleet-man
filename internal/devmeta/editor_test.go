package devmeta

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEditorFromMetadata(t *testing.T) {
	// The fixture-vscode label, behind a feature entry that brings an
	// extension and settings of its own.
	label := `[
	  {"id":"ghcr.io/devcontainers/features/python:1",
	   "customizations":{"vscode":{"extensions":["ms-python.python","ms-python.vscode-pylance"],
	     "settings":{"python.defaultInterpreterPath":"/usr/local/python/current/bin/python","editor.tabSize":2}}},
	   "postAttachCommand":"echo feature",
	   "portsAttributes":{"8000":{"label":"Feature"},"17390":{"label":"Old"}}},
	  {"postAttachCommand":"echo attached >> \"$HOME/.fleet-postattach.log\"",
	   "forwardPorts":[17390],
	   "portsAttributes":{"17390":{"label":"Itest Forwarded","onAutoForward":"silent"}},
	   "otherPortsAttributes":{"onAutoForward":"ignore"},
	   "customizations":{"vscode":{"extensions":["fleet-itest.fixture-extension","-MS-Python.vscode-pylance","ms-python.python"],
	     "settings":{"fleetItest.fromFixture":"customized","editor.tabSize":4}}}}
	]`
	cfg, err := EditorFromMetadata(label)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ms-python.python", "fleet-itest.fixture-extension"}; !reflect.DeepEqual(cfg.Extensions, want) {
		t.Errorf("extensions = %q, want %q", cfg.Extensions, want)
	}
	settings := map[string]any{}
	for k, v := range cfg.Settings {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			t.Fatalf("setting %s is not JSON: %v", k, err)
		}
		settings[k] = x
	}
	want := map[string]any{
		"python.defaultInterpreterPath": "/usr/local/python/current/bin/python",
		"editor.tabSize":                float64(4),
		"fleetItest.fromFixture":        "customized",
		"remote.portsAttributes": map[string]any{
			"8000":  map[string]any{"label": "Feature"},
			"17390": map[string]any{"label": "Itest Forwarded", "onAutoForward": "silent"},
		},
		"remote.otherPortsAttributes": map[string]any{"onAutoForward": "ignore"},
	}
	if !reflect.DeepEqual(settings, want) {
		t.Errorf("settings = %v\nwant %v", settings, want)
	}
	if len(cfg.PostAttach) != 2 || string(cfg.PostAttach[0]) != `"echo feature"` {
		t.Errorf("postAttach = %s", cfg.PostAttach)
	}
	if cfg.V != EditorConfigVersion {
		t.Errorf("version %d", cfg.V)
	}
}

func TestEditorFromMetadataEdges(t *testing.T) {
	for _, label := range []string{"", "  ", `[]`, `[{}]`, `[{"postAttachCommand":null,"otherPortsAttributes":null}]`} {
		cfg, err := EditorFromMetadata(label)
		if err != nil {
			t.Errorf("EditorFromMetadata(%q): %v", label, err)
		}
		if len(cfg.Extensions) != 0 || len(cfg.Settings) != 0 || len(cfg.PostAttach) != 0 {
			t.Errorf("EditorFromMetadata(%q) = %+v, want an empty config", label, cfg)
		}
	}
	if _, err := EditorFromMetadata(`{"not":"an array"}`); err == nil {
		t.Error("a label that is not an array was taken")
	}
	// The pre-customizations spelling, and an explicit portsAttributes
	// setting the attributes merge into.
	cfg, err := EditorFromMetadata(`[{"extensions":["a.b"],"settings":{"remote.portsAttributes":{"1":{"label":"one"},"2":{"label":"two"}}}},{"portsAttributes":{"2":{"label":"TWO"}}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Extensions, []string{"a.b"}) {
		t.Errorf("extensions = %q", cfg.Extensions)
	}
	var ports map[string]map[string]string
	if err := json.Unmarshal(cfg.Settings[PortsAttributesSetting], &ports); err != nil {
		t.Fatal(err)
	}
	if ports["1"]["label"] != "one" || ports["2"]["label"] != "TWO" {
		t.Errorf("remote.portsAttributes = %v", ports)
	}
	// postAttachCommand as an array and as an object pass through as written.
	cfg, _ = EditorFromMetadata(`[{"postAttachCommand":["echo","a b"]},{"postAttachCommand":{"x":"echo x","y":["echo","y"]}}]`)
	if len(cfg.PostAttach) != 2 || string(cfg.PostAttach[0]) != `["echo","a b"]` {
		t.Errorf("postAttach = %s", cfg.PostAttach)
	}
}
