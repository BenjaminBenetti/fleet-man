//go:build !nok8s

package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podagent"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// probeScript is exec #1: jobboot's probe (arch, uid, the tools staging can
// use) and what the pod's current fleet binary says it is, one
// `self-version=<json>` line ("" when it has none, or it does not run).
func probeScript(l PodLayout) string {
	return jobboot.ProbeScript + fmt.Sprintf(`f=%s
v=
if [ -x "$f" ]; then v=$("$f" pod-agent self-version 2>/dev/null) || v=; fi
printf 'self-version=%%s\n' "$v"
`, shellquote.Single(l.Binary()))
}

// podBuild is what a pod's fleet binary is: its self-version, or the Hello of
// the last session it served.
type podBuild struct {
	Version            string
	SHA                string
	ProtoMin, ProtoMax int
}

// parseProbe reads probeScript's output.
func parseProbe(out string) (jobboot.Probe, *podBuild, error) {
	probe, err := jobboot.ParseProbe(out)
	if err != nil {
		return probe, nil, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "self-version=")
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		var info podagent.SelfVersionInfo
		if json.Unmarshal([]byte(v), &info) != nil || info.ProtoMin < 1 || info.ProtoMax < info.ProtoMin {
			return probe, nil, nil // something that is not fleet, or a broken one
		}
		return probe, &podBuild{Version: info.Version, SHA: info.SHA, ProtoMin: info.ProtoMin, ProtoMax: info.ProtoMax}, nil
	}
	return probe, nil, nil
}

// stageScript is exec #2: write the binary (from stdin, gzip-compressed when
// wire is) content-addressed into the pod's bin dir, point the fleet link at
// it, keep the previous target for pod-init's fallback and delete the rest,
// then print the previous target (`prev=fleet-<sha>`), exactly the layout
// the seed writes (podseed's installBinary).
func stageScript(l PodLayout, sha string, size int64, gzipped bool, wire int64, checkSHA bool) (string, error) {
	name := podseed.BinaryName(sha)
	write, err := jobboot.StageScript(jobboot.StageFile{
		Path:     l.BinDir + "/" + name,
		Size:     size,
		SHA256:   sha,
		Gzip:     gzipped,
		WireSize: wire,
		Mode:     0o755,
		CheckSHA: checkSHA,
	})
	if err != nil {
		return "", err
	}
	q := shellquote.Single
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p %s\n", q(l.BinDir))
	b.WriteString(write)
	fmt.Fprintf(&b, "cd %s\n", q(l.BinDir))
	b.WriteString("prev=$(readlink fleet 2>/dev/null) || prev=\n")
	fmt.Fprintf(&b, "if [ \"$prev\" = %s ]; then printf 'prev=\\n'; exit 0; fi\n", q(name))
	fmt.Fprintf(&b, "rm -f .fleet.new\nln -s %s .fleet.new\nmv -f .fleet.new fleet\n", q(name))
	fmt.Fprintf(&b, "for f in fleet-* .stage.*; do\n  [ -e \"$f\" ] || continue\n  [ \"$f\" = %s ] && continue\n  [ -n \"$prev\" ] && [ \"$f\" = \"$prev\" ] && continue\n  rm -f -- \"$f\"\ndone\n", q(name))
	b.WriteString("printf 'prev=%s\\n' \"$prev\"\n")
	return b.String(), nil
}

// stageBinary stages art into the pod over its own exec and returns the sha
// of the binary it replaced ("" for none, or when it was already current).
func (p *Pool) stageBinary(ctx context.Context, c *Cluster, pod string, probe jobboot.Probe, art *binstore.Artifact) (string, error) {
	if !probe.Head {
		return "", errors.New("the image has no head(1), which fleet stages its binary with")
	}
	path, wire, err := p.bins().Staged(ctx, art, probe.Gzip)
	if err != nil {
		return "", err
	}
	script, err := stageScript(p.opts.Layout, art.SHA256, art.Size, probe.Gzip, wire, probe.SHA256Sum)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var out, errOut bytes.Buffer
	req := kubeexec.ExecRequest{Pod: pod, Container: DevContainer, Command: []string{"sh", "-c", script}, Stdin: true}
	if err := p.exec(ctx, c, req, kubeexec.Stdio{Stdin: f, Stdout: &out, Stderr: &limitedWriter{w: &errOut, n: 4 << 10}}); err != nil {
		return "", fmt.Errorf("staging fleet %s into pod %s: %w%s", art.Info.Identity, pod, err, detail(errOut.String()))
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "prev="); ok {
			return strings.TrimPrefix(strings.TrimSpace(v), "fleet-"), nil
		}
	}
	return "", nil
}

// patchPID1Binary writes the staged binary's sha into the pid1.json of the
// pod's spec Secret, with the one it replaced as the fallback, so pod-init
// keeps running the new binary at the next start instead of re-executing the
// one the spec was rendered with.
func patchPID1Binary(ctx context.Context, cs kclient.Interface, ns, secret, sha, prev string) error {
	for attempt := 0; ; attempt++ {
		sec, err := cs.CoreV1().Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
		if err != nil {
			return err
		}
		raw, ok := sec.Data[podspec.PID1File]
		if !ok {
			return fmt.Errorf("secret %s has no %s", secret, podspec.PID1File)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &spec); err != nil {
			return fmt.Errorf("secret %s: %s: %w", secret, podspec.PID1File, err)
		}
		changed := setJSONString(spec, "binary_sha256", sha)
		if prev != "" && prev != sha && setJSONString(spec, "fallback_sha256", prev) {
			changed = true
		}
		if !changed {
			return nil
		}
		data, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		sec.Data[podspec.PID1File] = data
		_, err = cs.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{FieldManager: DefaultFieldManager})
		if apierrors.IsConflict(err) && attempt < 4 {
			continue
		}
		return err
	}
}

// setJSONString sets key to the JSON string v, reporting a change.
func setJSONString(m map[string]json.RawMessage, key, v string) bool {
	enc, _ := json.Marshal(v)
	if bytes.Equal(m[key], enc) {
		return false
	}
	m[key] = enc
	return true
}

// TmuxBundle is the bundled tmux's name under the pod's install dir:
// /opt/fleet/tmux, whose bin/ holds tmux and whose share/terminfo is where
// that tmux looks for terminfo.
const TmuxBundle = "tmux"

// exitBundleMissing is `pod-agent install-bundle --check`'s answer for a
// bundle that is not installed.
const exitBundleMissing = 3

// installBundleArgv is `pod-agent install-bundle` for art.
func installBundleArgv(l PodLayout, art *binstore.Artifact, check bool) []string {
	argv := []string{l.Binary(), "pod-agent", "install-bundle", "--dir", l.InstallDir, "--name", TmuxBundle, "--sha256", art.SHA256}
	if check {
		argv = append(argv, "--check")
	}
	return argv
}

func detail(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return ": " + kubeexec.SanitizeOutput([]byte(s))
	}
	return ""
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
