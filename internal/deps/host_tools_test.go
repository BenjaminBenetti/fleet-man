package deps

import (
	"errors"
	"testing"
)

func TestHostBuildToolsReportsWhatLookPathFinds(t *testing.T) {
	lookPath := func(file string) (string, error) {
		if file == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	tools := HostBuildTools(lookPath)
	if len(tools) != 2 {
		t.Fatalf("HostBuildTools() returned %d tools, want docker and devcontainer: %+v", len(tools), tools)
	}
	want := map[string]bool{"docker": true, "devcontainer": false}
	for _, tool := range tools {
		found, ok := want[tool.Binary]
		if !ok {
			t.Fatalf("unexpected tool %q", tool.Binary)
		}
		if tool.Found != found {
			t.Errorf("%s Found = %v, want %v", tool.Binary, tool.Found, found)
		}
		if tool.Name == "" || tool.Description == "" || tool.InstallURL == "" {
			t.Errorf("%s is missing its name, description or install URL: %+v", tool.Binary, tool)
		}
	}
}
