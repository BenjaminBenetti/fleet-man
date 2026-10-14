package kubeimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

// errRemoveUnsupported: the local cluster's tool cannot remove an image
// from its nodes, so the kubelet's own image GC reclaims the space.
var errRemoveUnsupported = errors.New("the cluster's tool cannot remove images from its nodes")

// nodeLoader puts images from the daemon host's docker into a local
// cluster's nodes.
type nodeLoader interface {
	// load makes ref, an image on the daemon host, present on every node.
	// force reloads it even where the node already has that tag.
	load(ctx context.Context, log io.Writer, ref string, force bool) error
	// remove deletes ref from the nodes, or returns errRemoveUnsupported.
	remove(ctx context.Context, log io.Writer, ref string) error
}

// lookTool finds a local cluster tool in the connection's extra PATH
// entries, then in fleetd's PATH.
func lookTool(name string, extraPath []string) (string, error) {
	for _, dir := range extraPath {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return exec.LookPath(name)
}

// loaderFor returns the loader for a local cluster, or why there is none.
func loaderFor(c localcluster.Cluster, extraPath []string, docker dockerCLI) (nodeLoader, error) {
	tool := func(name string) (string, error) {
		p, err := lookTool(name, extraPath)
		if err != nil {
			return "", fmt.Errorf("loading images into %s needs %s on the daemon host's PATH (or the connection's extra PATH)", c, name)
		}
		return p, nil
	}
	switch c.Kind {
	case localcluster.Minikube:
		p, err := tool("minikube")
		if err != nil {
			return nil, err
		}
		return minikubeLoader{path: p, profile: c.Name}, nil
	case localcluster.KinD:
		p, err := tool("kind")
		if err != nil {
			return nil, err
		}
		return kindLoader{path: p, cluster: c.Name}, nil
	case localcluster.K3d:
		p, err := tool("k3d")
		if err != nil {
			return nil, err
		}
		return k3dLoader{path: p, cluster: c.Name}, nil
	case localcluster.DockerDesktop:
		return sharedEngine{docker: docker}, nil
	}
	return nil, fmt.Errorf("node load needs a local cluster (minikube, kind, k3d or Docker Desktop); this cluster's nodes are not one")
}

// runTool runs a cluster tool, its output to log, and returns its error with
// its stderr.
func runTool(ctx context.Context, log io.Writer, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stdout = log
	cmd.Stderr = io.MultiWriter(&stderr, log)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(path), strings.Join(args, " "), err, truncate(strings.TrimSpace(stderr.String()), 600))
	}
	return nil
}

// minikubeLoader: `minikube image load --daemon` saves the image from the
// daemon host's docker into minikube's image cache, a tarball under
// $MINIKUBE_HOME/cache/images/<arch>/, and imports it on every node of the
// profile. minikube never removes that tarball, and fleet loads every image
// it builds (one per devcontainer change, each the size of the image): so
// the tarball of one of fleet's own tags (fleet.invalid/..., NodeLoadTag) goes
// once its load is done, and again when the image is removed (what a load
// that was cut short, or an older fleet, left). No other cache entry is
// touched.
type minikubeLoader struct {
	path, profile string
}

func (l minikubeLoader) load(ctx context.Context, log io.Writer, ref string, force bool) error {
	unlock := lockCachedTag(ref)
	defer unlock()
	defer forgetMinikubeCache(ref)
	return runTool(ctx, log, l.path, "-p", l.profile, "image", "load", "--daemon=true", fmt.Sprintf("--overwrite=%t", force), ref)
}

func (l minikubeLoader) remove(ctx context.Context, log io.Writer, ref string) error {
	unlock := lockCachedTag(ref)
	defer unlock()
	forgetMinikubeCache(ref)
	return runTool(ctx, log, l.path, "-p", l.profile, "image", "rm", ref)
}

// cachedTags serializes the loads and removals of one tag: the image cache is
// minikube's (every profile's) and a load reads the tarball another load of
// the same tag (into another profile) would remove.
var cachedTags = struct {
	mu   sync.Mutex
	busy map[string]chan struct{}
}{busy: map[string]chan struct{}{}}

// lockCachedTag waits for the tag's other load or removal, and returns what
// lets the next one go.
func lockCachedTag(ref string) func() {
	for {
		cachedTags.mu.Lock()
		ch, held := cachedTags.busy[ref]
		if !held {
			ch = make(chan struct{})
			cachedTags.busy[ref] = ch
			cachedTags.mu.Unlock()
			return func() {
				cachedTags.mu.Lock()
				delete(cachedTags.busy, ref)
				cachedTags.mu.Unlock()
				close(ch)
			}
		}
		cachedTags.mu.Unlock()
		<-ch
	}
}

// minikubeHome is minikube's own folder: $MINIKUBE_HOME, with .minikube added
// unless it ends in it, else ~/.minikube (as minikube's localpath.MiniPath
// finds it). fleetd runs minikube with its own environment. A var for
// tests.
var minikubeHome = func() string {
	if h := os.Getenv("MINIKUBE_HOME"); h != "" {
		if filepath.Base(h) == ".minikube" {
			return h
		}
		return filepath.Join(h, ".minikube")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".minikube")
}

// minikubeCacheFiles are where minikube's image cache keeps ref's tarball,
// one per architecture folder it has (minikube's own: cache/images/<arch>/<ref>
// with each ':' as '_'), and how many folders deep under the architecture's
// the tarball is (all fleet's); nil unless ref is a node load tag of fleet's
// (fleet.invalid/<path>:<tag>, whose parts the reference grammar keeps free
// of "..").
func minikubeCacheFiles(ref string) (files []string, depth int) {
	r, err := ParseReference(ref)
	if err != nil || r.Domain != nodeLoadDomain || r.Tag == "" || r.Digest != "" {
		return nil, 0
	}
	home := minikubeHome()
	if home == "" {
		return nil, 0
	}
	images := filepath.Join(home, "cache", "images")
	arches, err := os.ReadDir(images)
	if err != nil {
		return nil, 0
	}
	name := nodeLoadDomain + "/" + r.Path
	rel := filepath.FromSlash(name + "_" + r.Tag)
	for _, a := range arches {
		if a.IsDir() {
			files = append(files, filepath.Join(images, a.Name(), rel))
		}
	}
	return files, strings.Count(name, "/")
}

// forgetMinikubeCache removes minikube's cached tarball of ref, one of
// fleet's node load tags (anything else is left alone), and the folders of
// fleet's that it leaves empty. Best effort, logged.
func forgetMinikubeCache(ref string) {
	files, depth := minikubeCacheFiles(ref)
	for _, f := range files {
		err := os.Remove(f)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			flog.Warn("kubernetes image: removing minikube's cached copy of a loaded image", "ref", ref, "file", f, "err", err)
			continue
		}
		flog.Info("kubernetes image: removed minikube's cached copy of a loaded image", "ref", ref, "file", f)
		// fleet.invalid/<daemon8>/ and fleet.invalid/, once empty.
		for dir, n := filepath.Dir(f), depth; n > 0; dir, n = filepath.Dir(dir), n-1 {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
}

// kindLoader: `kind load docker-image` imports into every node, skipping
// nodes that already hold that image ID.
type kindLoader struct {
	path, cluster string
}

func (l kindLoader) load(ctx context.Context, log io.Writer, ref string, _ bool) error {
	return runTool(ctx, log, l.path, "load", "docker-image", "--name", l.cluster, ref)
}

func (kindLoader) remove(context.Context, io.Writer, string) error { return errRemoveUnsupported }

// k3dLoader: `k3d image import`.
type k3dLoader struct {
	path, cluster string
}

func (l k3dLoader) load(ctx context.Context, log io.Writer, ref string, _ bool) error {
	return runTool(ctx, log, l.path, "image", "import", "--cluster", l.cluster, ref)
}

func (k3dLoader) remove(context.Context, io.Writer, string) error { return errRemoveUnsupported }

// sharedEngine is Docker Desktop's cluster, whose kubelet runs on the
// desktop's own docker engine and sees its images: tagging the image on the
// daemon host is the whole delivery.
type sharedEngine struct {
	docker dockerCLI
}

func (l sharedEngine) load(ctx context.Context, _ io.Writer, ref string, _ bool) error {
	id, err := l.docker.imageID(ctx, ref)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("docker-desktop's nodes share the daemon host's docker, which has no image %s", ref)
	}
	return nil
}

// remove is a no-op: untagging on the daemon host removes it from the node.
func (sharedEngine) remove(context.Context, io.Writer, string) error { return nil }
