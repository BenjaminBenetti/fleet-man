package backend

import "strings"

// ToolProbeScript detects which agent tool (if any) is running inside
// a container. It runs a single `ps` scan, excludes the probe's own
// process group to avoid self-matching, and checks every command-line
// field against all tool names at once. Priority order: claude,
// copilot, codex, gemini.
//
// The matching pattern (^|/)t($|[-./]) recognises:
//   - standalone binaries: `claude`, `/usr/local/bin/claude`
//   - interpreter-wrapped CLIs: `node /opt/copilot-cli/cli.js`
//     (`copilot` appears as a path segment)
//   - versioned paths: `gemini-cli`, `copilot.js`
//
// It rejects substrings like `my-claude-notes.txt` because the tool
// name must start a path component, which avoids false positives from
// files being edited or viewed.
const ToolProbeScript = `MY_PGID=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' \t\n\r')
ps -eo pid,pgid,args --no-headers 2>/dev/null | awk -v pgid="$MY_PGID" '
BEGIN {
  n = 4
  tools[1] = "claude"
  tools[2] = "copilot"
  tools[3] = "codex"
  tools[4] = "gemini"
}
$2 == pgid { next }
{
  for (i = 3; i <= NF; i++) {
    for (j = 1; j <= n; j++) {
      if ($i ~ "(^|/)"tools[j]"($|[-./])") {
        found[j] = 1
      }
    }
  }
}
END {
  for (j = 1; j <= n; j++) {
    if (found[j]) { print tools[j]; exit 0 }
  }
  print "-"
}
'
`

// ParseToolProbeOutput parses the stdout of ToolProbeScript. The
// second return is false only when the probe exec failed (empty
// output). Empty tool with ok=true means the probe ran but found no
// agent.
func ParseToolProbeOutput(output string) (string, bool) {
	tool := strings.TrimSpace(output)
	if tool == "" {
		return "", false
	}
	if tool == "-" {
		return "", true
	}
	return tool, true
}
