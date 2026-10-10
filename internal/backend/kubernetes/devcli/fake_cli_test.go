package devcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLIScript stands in for the devcontainer CLI in the unit tests: it
// makes the docker calls the real CLI makes, through the --docker-path it is
// given (so through the shim, which is this test binary), and prints what
// the real one prints. It records its argv, its environment and the docker
// calls' results under $FAKE_DIR.
const fakeCLIScript = `#!/bin/sh
echo "[2026-10-10T00:00:00.000Z] @devcontainers/cli ${FAKE_CLI_VERSION:-0.89.0}. Node.js v24.21.0. linux x64." >&2
printf '%s\n' "$@" > "$FAKE_DIR/cli.args"
env > "$FAKE_DIR/cli.env"
cmd=$1; shift
docker=""; compose=""; ws=""; cid=""; config=""; merged=""
while [ $# -gt 0 ]; do
  case "$1" in
    --docker-path) docker=$2; shift ;;
    --docker-compose-path) compose=$2; shift ;;
    --workspace-folder) ws=$2; shift ;;
    --container-id) cid=$2; shift ;;
    --config) config=$2; shift ;;
    --include-merged-configuration) merged=1 ;;
  esac
  shift
done
case "$cmd" in
read-configuration)
  "$docker" ps -q -a --filter "label=devcontainer.local_folder=$ws" >/dev/null
  if [ -n "$merged" ]; then
    if [ -n "${FAKE_MERGED_COMPOSE:-}" ]; then
      "$compose" version --short || { echo '{"outcome":"error","message":"compose failed"}'; exit 1; }
    fi
    "$docker" inspect --type image base:1 >/dev/null || exit 1
    cat "$FAKE_DIR/merged.json"
  else
    cat "$FAKE_DIR/plain.json"
  fi
  ;;
up)
  [ -n "${FAKE_UP_FAIL:-}" ] && { echo "[time] build failed: $FAKE_UP_FAIL" >&2; "$docker" info >/dev/null 2>&1; echo '{"outcome":"error","message":"An error occurred building the image."}'; exit 1; }
  "$docker" buildx version >/dev/null
  "$docker" ps -q -a --filter "label=devcontainer.local_folder=$ws" >/dev/null
  # Like node, the CLI gives the watcher pipes of its own, not its stdio.
  "$docker" events --format '{{json .}}' --filter event=start </dev/null >/dev/null 2>&1 &
  # The real CLI has the watcher running before it runs the container.
  i=0; until grep -q '"events"' "$(dirname "$docker")/calls.jsonl" 2>/dev/null; do
    i=$((i+1)); [ $i -gt 500 ] && break; sleep 0.01
  done
  echo "[time] Start: Run: docker run -e SECRET_TOKEN=hunter2 ..." >&2
  "$docker" run --sig-proxy=false -a STDOUT -a STDERR \
    --mount "type=bind,source=$ws,target=/workspaces/app" \
    -l "devcontainer.local_folder=$ws" -e SECRET_TOKEN=hunter2 -u vscode \
    --entrypoint /bin/sh -l 'devcontainer.metadata=[{"remoteUser":"vscode"}]' \
    "${FAKE_IMAGE:-vsc-app-1234}" -c 'echo Container started' - >/dev/null 2>"$FAKE_DIR/run.err"
  echo "$?" > "$FAKE_DIR/run.rc"
  echo '{"outcome":"error","message":"Command failed: docker run -e SECRET_TOKEN=hunter2","description":"An error occurred setting up the container."}'
  exit 1
  ;;
set-up)
  cp "$config" "$FAKE_DIR/setup.config"
  "$docker" inspect --type container "$cid" > "$FAKE_DIR/inspect.json" || exit 1
  id=$(sed -n 's/.*"Id": "\([0-9a-f]*\)".*/\1/p' "$FAKE_DIR/inspect.json")
  "$docker" -v >/dev/null 2>&1
  out=$(echo "from stdin" | "$docker" exec -i -u vscode -e HOOK=postCreate -w /workspaces/app "$id" /bin/sh -c 'run hook') || {
    echo '{"outcome":"error","message":"Command failed: /bin/sh -c run hook","description":"postCreateCommand from devcontainer.json failed."}'
    exit 1
  }
  printf '%s' "$out" > "$FAKE_DIR/exec.out"
  echo '{"outcome":"success","mergedConfiguration":{"remoteUser":"vscode"}}'
  ;;
esac
`

// fakeDockerScript stands in for the real docker that capture mode hands
// builds and image inspects to.
const fakeDockerScript = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/docker.log"
case "$1 $2" in
  "context show") echo "${FAKE_CONTEXT:-desktop-linux}" ;;
  "image inspect")
    [ -f "$FAKE_DIR/image.json" ] || { echo "Error response from daemon: No such image: $4" >&2; exit 1; }
    cat "$FAKE_DIR/image.json" ;;
  "pull --platform"|"pull --")
    [ -f "$FAKE_DIR/image.remote.json" ] || { echo "pull access denied" >&2; exit 1; }
    echo "Status: Downloaded newer image"
    cp "$FAKE_DIR/image.remote.json" "$FAKE_DIR/image.json" ;;
  "inspect --type") echo '[{"Id":"sha256:base"}]' ;;
  "buildx version") echo "github.com/docker/buildx v0.38.0" ;;
  *) echo "fake docker: $*" ;;
esac
`

// fakeTools is a stand-in CLI and docker in a fresh directory, and the
// directory they record in.
type fakeTools struct {
	Tools
	dir string
}

func newFakeTools(t *testing.T) *fakeTools {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("devcontainer", fakeCLIScript, 0o755)
	write("docker", fakeDockerScript, 0o755)
	write("image.json", `[{"Id":"sha256:built","RepoTags":["vsc-app-1234:latest"],"Architecture":"amd64","Os":"linux",
		"Config":{"User":"root","Env":["PATH=/usr/bin"],"Labels":{"devcontainer.metadata":"[{\"remoteUser\":\"image-user\"}]","image.label":"1"}}}]`, 0o644)
	t.Setenv("FAKE_DIR", dir)
	return &fakeTools{Tools: Tools{CLI: filepath.Join(dir, "devcontainer"), Docker: filepath.Join(dir, "docker")}, dir: dir}
}

// read returns a file the fakes wrote.
func (f *fakeTools) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("the fake CLI wrote no %s: %v", name, err)
	}
	return string(data)
}

// args is the fake CLI's last argv.
func (f *fakeTools) args(t *testing.T) []string {
	return strings.Split(strings.TrimSpace(f.read(t, "cli.args")), "\n")
}

// writeConfigs gives read-configuration its plain and merged answers for a
// devcontainer.json at configFile with the given extra members.
func (f *fakeTools) writeConfigs(t *testing.T, configFile, members string) {
	t.Helper()
	cfg := `{"name":"app","configFilePath":{"$mid":1,"fsPath":"` + configFile + `","path":"` + configFile + `","scheme":"vscode-fileHost"}` + members + `}`
	plain := `{"configuration":` + cfg + `,"workspace":{"workspaceFolder":"/workspaces/app"}}`
	merged := `{"configuration":` + cfg + `,"workspace":{"workspaceFolder":"/workspaces/app"},"mergedConfiguration":{"remoteUser":"vscode"}}`
	for name, content := range map[string]string{"plain.json": plain, "merged.json": merged} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
