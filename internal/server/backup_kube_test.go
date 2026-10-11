package server

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/ulikunitz/xz"
)

// archiveEntry is one entry of a backup archive, in order.
type archiveEntry struct {
	name string
	dir  bool
	mode int64
	data string
}

func readArchiveEntries(t *testing.T, path string) []archiveEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(xr)
	var out []archiveEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out = append(out, archiveEntry{name: hdr.Name, dir: hdr.Typeflag == tar.TypeDir, mode: hdr.Mode & 0o777, data: string(data)})
	}
}

// extractLikeTar unpacks the archive into dir the way `tar -xf -C dir` does:
// directories from their entries, files with their modes.
func extractLikeTar(t *testing.T, entries []archiveEntry, dir string) {
	t.Helper()
	for _, e := range entries {
		target := filepath.Join(dir, filepath.FromSlash(e.name))
		if e.dir {
			if err := os.Mkdir(target, os.FileMode(e.mode)); err != nil {
				t.Fatalf("mkdir %s (a directory entry must precede its files): %v", e.name, err)
			}
			continue
		}
		if err := os.WriteFile(target, []byte(e.data), os.FileMode(e.mode)); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}
}

// snapshotTree maps every regular file under root (relative path) to its
// content and mode.
func snapshotTree(t *testing.T, root string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			if rel != "." && skip(rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if skip(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = info.Mode().Perm().String() + " " + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A backup of a daemon with two Kubernetes connections unpacks into an empty
// ~/.fleet as the very files it was taken from, each at its own path: the
// two connections' image indexes do not collide, the kube directories come
// back owner-only, and the incarnation nonce, the gateway image cache (and
// temp files) stay out.
func TestBackupTwoClustersRoundTrip(t *testing.T) {
	home := writeFleetState(t, map[string]string{
		"config.json": `{"cfg":1}`,
		"state.json":  `{"fleets":{}}`,
		"mcp.token":   "secret-token",
	})
	fleetDir := filepath.Join(home, ".fleet")
	err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{
			{ID: "minikube-abc234", Context: "minikube", KubeconfigPaths: []string{"/home/me/.kube/config"}, Namespace: "fleet", Image: fleet.KubeImageSettings{GatewayCredential: "pull-secret-a"}},
			{ID: "prod-eu-xyz234", Context: "arn:aws:eks:eu-west-1:1:cluster/prod", KubeconfigPaths: []string{"/home/me/.kube/eks"}, Namespace: "team", PluginEnv: []string{"AWS_PROFILE=prod"}},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kubeFiles := map[string]string{
		"daemon_id":                                    "0123456789abcdef0123456789abcdef\n",
		"kube/footprint.json":                          `{"v":1,"tombstones":["aaaa2345"]}`,
		"kube/jobs/aaaa2345.json":                      `{"v":1,"op":"create","cluster":"minikube-abc234"}`,
		"kube/jobs/bbbb2345.json":                      `{"v":1,"op":"rebuild","cluster":"prod-eu-xyz234"}`,
		"kube/minikube-abc234/images.json":             `{"images":["a"]}`,
		"kube/prod-eu-xyz234/images.json":              `{"images":["b"]}`,
		"kube/minikube-abc234/instances/aaaa2345.json": `{"v":1,"gen":1}`,
		"kube/incarnation.json":                        `{"v":1,"nonce":"aaaaaaaaaaaaaaaa"}`, // never backed up
		"kube/jobs/.aaaa2345.json-1.tmp":               "torn",                               // an atomicfile temp
		"kube/minikube-abc234/notes.txt":               "not a backup source",
		// The gateway image cache is made again from docker, never backed up.
		"kube/images/store.json":                               `{"version":1}`,
		"kube/images/blobs/sha256/" + strings.Repeat("ab", 32): "layer bytes",
	}
	for rel, content := range kubeFiles {
		path := filepath.Join(fleetDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archive, n, err := writeBackup(time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("writeBackup: %v", err)
	}
	if n != 11 {
		t.Errorf("captured %d files, want 11", n)
	}
	entries := readArchiveEntries(t, archive)
	var dirs []string
	for _, e := range entries {
		if e.dir {
			dirs = append(dirs, e.name)
			if e.mode != 0o700 {
				t.Errorf("directory entry %s mode %o, want 700", e.name, e.mode)
			}
		}
		if e.name == "config.json" || e.name == "state.json" || e.name == "mcp.token" {
			continue // top-level files keep their plain names
		}
		if !strings.HasPrefix(e.name, "kube") && e.name != "daemon_id" {
			t.Errorf("unexpected entry %s", e.name)
		}
	}
	if strings.Join(dirs, " ") != "kube/ kube/jobs/ kube/minikube-abc234/ kube/prod-eu-xyz234/ kube/minikube-abc234/instances/" {
		t.Errorf("directory entries = %v", dirs)
	}

	excluded := func(rel string) bool {
		return rel == "backup/" || rel == "kube/incarnation.json" || strings.HasSuffix(rel, ".tmp") || strings.HasSuffix(rel, "notes.txt") || strings.HasPrefix(rel, "kube/images/")
	}
	want := snapshotTree(t, fleetDir, excluded)

	restored := filepath.Join(t.TempDir(), ".fleet")
	if err := os.Mkdir(restored, 0o700); err != nil {
		t.Fatal(err)
	}
	extractLikeTar(t, entries, restored)
	got := snapshotTree(t, restored, func(string) bool { return false })
	if len(got) != len(want) {
		t.Errorf("restored %d files, want %d:\n got %v\nwant %v", len(got), len(want), keys(got), keys(want))
	}
	for rel, w := range want {
		if got[rel] != w {
			t.Errorf("restored %s = %q, want %q", rel, got[rel], w)
		}
	}
	if info, err := os.Stat(filepath.Join(restored, "kube")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("restored kube dir: %v %v", info, err)
	}

	// The restored registry reads back as both connections.
	t.Setenv("HOME", filepath.Dir(restored))
	k, err := state.LoadKubeClusters()
	if err != nil || len(k.Clusters) != 2 || k.Find("prod-eu-xyz234") == nil || k.Find("minikube-abc234").Image.GatewayCredential != "pull-secret-a" {
		t.Errorf("restored registry = %+v, %v", k, err)
	}

	// The real tar agrees, where one that can read xz is installed.
	if _, err := exec.LookPath("tar"); err == nil {
		real := t.TempDir()
		var stderr bytes.Buffer
		cmd := exec.Command("tar", "-xJf", archive, "-C", real)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if _, xzErr := exec.LookPath("xz"); xzErr != nil {
				t.Logf("tar cannot unpack xz here (%v: %s); skipped", err, stderr.String())
				return
			}
			t.Fatalf("tar -xJf: %v: %s", err, stderr.String())
		}
		if gotReal := snapshotTree(t, real, func(string) bool { return false }); len(gotReal) != len(want) {
			t.Errorf("tar restored %v, want %v", keys(gotReal), keys(want))
		} else {
			for rel, w := range want {
				if gotReal[rel] != w {
					t.Errorf("tar restored %s = %q, want %q", rel, gotReal[rel], w)
				}
			}
		}
	}
}

// fleet_restore_backup documents the Kubernetes files and what a restore
// means for cluster instances.
func TestMcpRestoreBackupDocumentsKubernetes(t *testing.T) {
	writeFleetState(t, nil)
	_, out, err := (&service{}).mcpRestoreBackup(t.Context(), nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, c := range out.Contents {
		documented[c.Name] = true
	}
	for _, name := range []string{"kube_clusters.json", "daemon_id", "kube/footprint.json", "kube/jobs/<key>.json", "kube/<cluster>/images.json"} {
		if !documented[name] {
			t.Errorf("Contents does not document %s", name)
		}
	}
	for _, phrase := range []string{"disk", "fleet cluster gc", "never deleted", "discarded", "OWNER_CONFLICT"} {
		if !strings.Contains(out.Kubernetes, phrase) {
			t.Errorf("the Kubernetes note lacks %q: %s", phrase, out.Kubernetes)
		}
	}
	if !strings.Contains(out.Compression, "kube/") {
		t.Errorf("Compression does not describe the kube/ paths: %s", out.Compression)
	}
}

// Every documented file is a backup source and every source is documented.
func TestMcpRestoreBackupContentsMatchSources(t *testing.T) {
	home := writeFleetState(t, nil)
	for _, rel := range []string{"kube/jobs/abcd2345.json", "kube/c-abc234/images.json", "kube/c-abc234/seedcaches.json", "kube/c-abc234/instances/abcd2345.json", "kube/c-abc234/buildkit/tv5vpl4q.json"} {
		path := filepath.Join(home, ".fleet", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, out, _ := (&service{}).mcpRestoreBackup(t.Context(), nil, struct{}{})
	documented := map[string]bool{}
	for _, c := range out.Contents {
		documented[c.Name] = true
	}
	generic := strings.NewReplacer("abcd2345", "<key>", "c-abc234", "<cluster>", "tv5vpl4q", "<fleet>")
	sources := map[string]bool{}
	for _, src := range backupSources() {
		name := generic.Replace(backupEntryName(src))
		sources[name] = true
		if !documented[name] {
			t.Errorf("backup source %s is not documented", name)
		}
	}
	for name := range documented {
		if !sources[name] {
			t.Errorf("documented %s is not a backup source", name)
		}
	}
}
