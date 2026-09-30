package devcontainer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStartRunsLifecycleCommandsForStartedContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixtures require Unix")
	}
	for _, tc := range []struct {
		name                  string
		startFails, hookFails bool
	}{
		{name: "success"}, {name: "start fails", startFails: true}, {name: "hook fails", hookFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			t.Setenv("POSTSTART_TEST_LOG", log)
			t.Setenv(sshAgentSockOverrideEnv, "off")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			docker := `#!/bin/sh
printf 'docker %s\n' "$*" >> "$POSTSTART_TEST_LOG"
case "$1" in
start) `
			if tc.startFails {
				docker += "exit 1"
			} else {
				docker += "exit 0"
			}
			docker += ` ;;
inspect) printf '%s\n' '[{"Config":{"Labels":{"devcontainer.local_folder":"/workspace with spaces"}}}]' ;;
esac
`
			cli := "#!/bin/sh\nprintf 'devcontainer\\n' >> \"$POSTSTART_TEST_LOG\"\nprintf '<%s>\\n' \"$@\" >> \"$POSTSTART_TEST_LOG\"\n"
			if tc.hookFails {
				cli += "exit 1\n"
			}
			for name, script := range map[string]string{"docker": docker, "devcontainer": cli} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := New().Start("exact-container")
			if (err != nil) != tc.startFails {
				t.Fatalf("Start error = %v, startFails = %v", err, tc.startFails)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			calls := string(data)
			if tc.startFails {
				if calls != "docker start exact-container\n" {
					t.Fatalf("ran hooks after failed start: %s", calls)
				}
				return
			}
			want := "docker start exact-container\ndocker inspect exact-container\ndevcontainer\n<run-user-commands>\n<--workspace-folder>\n</workspace with spaces>\n<--container-id>\n<exact-container>\n"
			if !strings.HasPrefix(calls, want) {
				t.Fatalf("wrong lifecycle execution: %s", calls)
			}
		})
	}
}
