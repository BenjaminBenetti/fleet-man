package jobboot

import (
	"bufio"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// ProbeScript is exec #1 into a waiting Job pod: what fleetd needs to know to
// stage into it. It prints key=value lines (ParseProbe reads them) and uses
// only what every devcontainer image has.
const ProbeScript = `printf 'arch=%s\n' "$(uname -m 2>/dev/null)"
printf 'uid=%s\n' "$(id -u 2>/dev/null)"
for t in gzip sha256sum head; do
  if command -v "$t" >/dev/null 2>&1; then printf '%s=1\n' "$t"; else printf '%s=0\n' "$t"; fi
done
`

// Probe is what ProbeScript found in a pod.
type Probe struct {
	// Arch is the pod's architecture as GOARCH spells it (amd64, arm64);
	// uname's own name when fleet knows no GOARCH for it.
	Arch string
	// UID is the uid the container runs as; -1 when id printed nothing.
	UID int
	// Gzip, SHA256Sum and Head report the tools StageScript can use: gzip
	// lets fleetd send the binary compressed, sha256sum adds an in-shell
	// sha check, and head is required.
	Gzip      bool
	SHA256Sum bool
	Head      bool
}

// ParseProbe reads ProbeScript's output.
func ParseProbe(out string) (Probe, error) {
	p := Probe{UID: -1}
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		seen[key] = true
		switch key {
		case "arch":
			p.Arch = goarch(value)
		case "uid":
			if n, err := strconv.Atoi(value); err == nil {
				p.UID = n
			}
		case "gzip":
			p.Gzip = value == "1"
		case "sha256sum":
			p.SHA256Sum = value == "1"
		case "head":
			p.Head = value == "1"
		}
	}
	for _, key := range []string{"arch", "uid", "head"} {
		if !seen[key] {
			return Probe{}, fmt.Errorf("probe output has no %s line: %q", key, out)
		}
	}
	if p.Arch == "" {
		return Probe{}, fmt.Errorf("probe could not tell the pod's architecture: %q", out)
	}
	return p, nil
}

// goarch maps `uname -m` to GOARCH.
func goarch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64", "armv8l", "armv8b":
		return "arm64"
	}
	return machine
}

// StageFile is one file fleetd streams into a pod over exec stdin.
type StageFile struct {
	// Path is where it lands, absolute.
	Path string
	// Size and SHA256 describe the file as written (after gunzip).
	Size   int64
	SHA256 string
	// Gzip is whether the stream is gzip-compressed; WireSize is then the
	// compressed length on stdin. Without gzip the stream is Size bytes.
	Gzip     bool
	WireSize int64
	// Mode is the file's permission bits (0 is 0644).
	Mode uint32
	// CheckSHA verifies SHA256 in the shell; it needs sha256sum in the pod
	// (Probe.SHA256Sum). Without it the size is still checked, and the
	// binary checks its own sha once it runs.
	CheckSHA bool
	// Ready writes the tools dir's sentinel once the file is in place, which
	// releases the wait loop. Stage the spec last with it set.
	Ready bool
	// ReadyPath is the sentinel to write; "" is .ready beside Path.
	ReadyPath string
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the file can be staged as described.
func (f StageFile) Validate() error {
	if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "/" {
		return fmt.Errorf("stage path %q is not a clean absolute file path", f.Path)
	}
	if f.Size < 0 || (f.Gzip && f.WireSize <= 0) {
		return fmt.Errorf("stage %s: bad size %d (wire %d)", f.Path, f.Size, f.WireSize)
	}
	if !hexSHA256.MatchString(f.SHA256) {
		return fmt.Errorf("stage %s: sha256 %q is not 64 lowercase hex digits", f.Path, f.SHA256)
	}
	if f.Mode&^0o777 != 0 {
		return fmt.Errorf("stage %s: mode %o has more than permission bits", f.Path, f.Mode)
	}
	return nil
}

// StageScript is the `sh -c` script that writes f from the exec's stdin:
// `head -c` exactly the stream's length (through `gzip -dc` when compressed)
// into a temp file beside the target, checks the size and, with CheckSHA, the
// sha, then renames it into place and, with Ready, writes the sentinel. A
// failed check exits non-zero and leaves the target untouched. Only one file
// rides one exec, so stdin ends where the file does.
func StageScript(f StageFile) (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	mode := f.Mode
	if mode == 0 {
		mode = 0o644
	}
	dir := path.Dir(f.Path)
	q := shellquote.Single
	var b strings.Builder
	fmt.Fprintf(&b, "set -e\numask 077\nt=%s/.stage.$$\ntrap 'rm -f \"$t\"' EXIT\n", q(dir))
	if f.Gzip {
		fmt.Fprintf(&b, "head -c %s | gzip -dc > \"$t\"\n", itoa(f.WireSize))
	} else {
		fmt.Fprintf(&b, "head -c %s > \"$t\"\n", itoa(f.Size))
	}
	fmt.Fprintf(&b, "n=$(wc -c < \"$t\" | tr -d ' ')\n[ \"$n\" = %s ] || { echo \"stage %s: got $n bytes, want %s\" >&2; exit 1; }\n",
		itoa(f.Size), shellquote.EscapeSingle(f.Path), itoa(f.Size))
	if f.CheckSHA {
		fmt.Fprintf(&b, "s=$(sha256sum < \"$t\" | cut -d' ' -f1)\n[ \"$s\" = %s ] || { echo \"stage %s: sha256 $s, want %s\" >&2; exit 1; }\n",
			f.SHA256, shellquote.EscapeSingle(f.Path), f.SHA256)
	}
	fmt.Fprintf(&b, "chmod %04o \"$t\"\nmv -f \"$t\" %s\ntrap - EXIT\n", mode, q(f.Path))
	if f.Ready {
		ready := f.ReadyPath
		if ready == "" {
			ready = path.Join(dir, ReadyName)
		}
		fmt.Fprintf(&b, ": > %s\n", q(ready))
	}
	return b.String(), nil
}
