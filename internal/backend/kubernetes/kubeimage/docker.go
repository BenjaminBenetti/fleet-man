package kubeimage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// dockerCLI runs the daemon host's docker for the image operations delivery
// needs. Output goes to the job log; errors carry docker's stderr.
type dockerCLI struct {
	path string
}

// run runs docker with args, copying its output to log, and returns its
// stdout.
func (d dockerCLI) run(ctx context.Context, log io.Writer, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, log)
	cmd.Stderr = io.MultiWriter(&stderr, log)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return stdout.Bytes(), &dockerError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

type dockerError struct {
	args   []string
	err    error
	stderr string
}

func (e *dockerError) Error() string {
	verb := strings.Join(e.args[:min(2, len(e.args))], " ")
	return fmt.Sprintf("docker %s: %v: %s", verb, e.err, truncate(e.stderr, 600))
}

// tag tags src (an image ID or reference) as dst.
func (d dockerCLI) tag(ctx context.Context, src, dst string) error {
	_, err := d.run(ctx, io.Discard, "tag", src, dst)
	return err
}

// pushedDigest is the line docker push ends with: "<tag>: digest: sha256:<hex> size: <n>".
var pushedDigest = regexp.MustCompile(`digest: (sha256:[a-f0-9]{64}) size: [0-9]+`)

// push pushes ref and returns the digest of what it pushed: from docker's
// last line, else from the image's RepoDigests for the repository.
func (d dockerCLI) push(ctx context.Context, log io.Writer, ref string) (string, error) {
	out, err := d.run(ctx, log, "push", ref)
	if err != nil {
		return "", err
	}
	if m := pushedDigest.FindAllSubmatch(out, -1); len(m) > 0 {
		return string(m[len(m)-1][1]), nil
	}
	r, err := ParseReference(ref)
	if err != nil {
		return "", err
	}
	digests, err := d.repoDigests(ctx, ref)
	if err != nil {
		return "", err
	}
	for _, rd := range digests {
		if p, err := ParseReference(rd); err == nil && p.Repository() == r.Repository() {
			return p.Digest, nil
		}
	}
	return "", fmt.Errorf("docker push %s did not say which digest it pushed", ref)
}

func (d dockerCLI) repoDigests(ctx context.Context, ref string) ([]string, error) {
	out, err := d.run(ctx, io.Discard, "image", "inspect", "--format", "{{json .RepoDigests}}", "--", ref)
	if err != nil {
		return nil, err
	}
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &digests); err != nil {
		return nil, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	return digests, nil
}

// imageID returns ref's image ID on the daemon host, or "" when it has no
// such image.
func (d dockerCLI) imageID(ctx context.Context, ref string) (string, error) {
	out, err := d.run(ctx, io.Discard, "image", "inspect", "--format", "{{.Id}}", "--", ref)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if isNoSuchImage(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// untag removes the tag ref from the daemon host; the image stays while
// another tag or a container holds it. A tag already gone is not an error.
func (d dockerCLI) untag(ctx context.Context, ref string) error {
	_, err := d.run(ctx, io.Discard, "image", "rm", "--", ref)
	if isNoSuchImage(err) {
		return nil
	}
	return err
}

// isNoSuchImage reports whether docker failed because the image is not on
// the daemon host.
func isNoSuchImage(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && strings.Contains(strings.ToLower(de.stderr), "no such image")
}

// pushHint names the likely fix for a failed push.
func pushHint(err error, domain string) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "unauthorized") || strings.Contains(s, "denied") || strings.Contains(s, "authentication required"):
		return fmt.Sprintf("log in on the daemon host (docker login %s) with an account that may push there", domain)
	case strings.Contains(s, "server gave http response to https client"):
		return fmt.Sprintf("%s serves plain HTTP: list it in the daemon host's docker insecure-registries (daemon.json), and configure the cluster's nodes for it", domain)
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate"):
		return fmt.Sprintf("the daemon host's docker does not trust %s's certificate: add its CA under /etc/docker/certs.d/%s/", domain, domain)
	case strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") || strings.Contains(s, "i/o timeout"):
		return fmt.Sprintf("the daemon host cannot reach %s", domain)
	}
	return ""
}
