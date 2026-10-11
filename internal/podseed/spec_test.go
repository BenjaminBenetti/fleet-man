package podseed

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validSeedSpec() JobSpec {
	return JobSpec{
		V: JobSpecVersion, Op: OpSeed, Gen: 1, PVC: "/fleet-pvc",
		Seed: &SeedSpec{
			MountDirs: []string{"/workspaces/repo", "/var/lib/docker"},
			Hostname:  "fi-fleet-alpha-1a2b3c4d-0",
			Remap:     &Remap{User: "vscode", UID: 1000, GID: 1000},
			Volumes:   []Volume{{Name: "docker", Target: "/var/lib/docker", CopyUp: true}},
			Workspace: &Workspace{Owner: &IDs{UID: 1000, GID: 1000}},
		},
	}
}

func TestJobSpecValidate(t *testing.T) {
	if err := validSeedSpec().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, op := range []Op{OpRollback, OpCommit, OpTool} {
		if err := (JobSpec{V: 1, Op: op, Gen: 2, PVC: "/p"}).Validate(); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
	reset := validSeedSpec()
	reset.Gen, reset.Seed.Reset, reset.Seed.PrevGen = 3, true, 2
	if err := reset.Validate(); err != nil {
		t.Fatalf("a reset: %v", err)
	}
	cached := validSeedSpec()
	cached.Seed.Cache = "abc234def567"
	if err := cached.Validate(); err != nil {
		t.Fatalf("a seed from a seed cache: %v", err)
	}
	if err := (JobSpec{V: 1, Op: OpSeedCache, Gen: 1, PVC: "/p", Cache: "abc234def567"}).Validate(); err != nil {
		t.Fatalf("a seed cache trim: %v", err)
	}
	for name, spec := range map[string]JobSpec{
		"trim without an id":    {V: 1, Op: OpSeedCache, Gen: 1, PVC: "/p"},
		"trim with a seed":      {V: 1, Op: OpSeedCache, Gen: 1, PVC: "/p", Cache: "abc", Seed: validSeedSpec().Seed},
		"an id on another op":   {V: 1, Op: OpCommit, Gen: 1, PVC: "/p", Cache: "abc"},
		"an id that is a path":  {V: 1, Op: OpSeedCache, Gen: 1, PVC: "/p", Cache: "../abc"},
		"an id in another case": {V: 1, Op: OpSeedCache, Gen: 1, PVC: "/p", Cache: "ABC"},
	} {
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, spec)
		}
	}
	for name, mutate := range map[string]func(*JobSpec){
		"version":          func(s *JobSpec) { s.V = 2 },
		"gen":              func(s *JobSpec) { s.Gen = 0 },
		"op":               func(s *JobSpec) { s.Op = "explode" },
		"pvc relative":     func(s *JobSpec) { s.PVC = "pvc" },
		"pvc root":         func(s *JobSpec) { s.PVC = "/" },
		"pvc unclean":      func(s *JobSpec) { s.PVC = "/a/../b" },
		"tools":            func(s *JobSpec) { s.Tools = "tools" },
		"stall":            func(s *JobSpec) { s.StallTimeout = "soon" },
		"hold":             func(s *JobSpec) { s.ResultHold = "-1s" },
		"no seed":          func(s *JobSpec) { s.Seed = nil },
		"seed on commit":   func(s *JobSpec) { s.Op = OpCommit },
		"seed on tool":     func(s *JobSpec) { s.Op = OpTool },
		"reset no prev":    func(s *JobSpec) { s.Seed.Reset = true },
		"reset same gen":   func(s *JobSpec) { s.Seed.Reset, s.Seed.PrevGen = true, 1 },
		"source":           func(s *JobSpec) { s.Seed.Source = "image" },
		"exclude":          func(s *JobSpec) { s.Seed.Exclude = []string{"a/b"} },
		"mount relative":   func(s *JobSpec) { s.Seed.MountDirs = []string{"workspaces"} },
		"mount root":       func(s *JobSpec) { s.Seed.MountDirs = []string{"/"} },
		"hostname":         func(s *JobSpec) { s.Seed.Hostname = "" },
		"hostname slash":   func(s *JobSpec) { s.Seed.Hostname = "a/b" },
		"remap user":       func(s *JobSpec) { s.Seed.Remap.User = "" },
		"remap uid":        func(s *JobSpec) { s.Seed.Remap.UID = -1 },
		"volume name":      func(s *JobSpec) { s.Seed.Volumes[0].Name = "../x" },
		"volume dup":       func(s *JobSpec) { s.Seed.Volumes = append(s.Seed.Volumes, s.Seed.Volumes[0]) },
		"volume target":    func(s *JobSpec) { s.Seed.Volumes[0].Target = "x" },
		"shared, no share": func(s *JobSpec) { s.Seed.SharedVolumes = []Volume{{Name: "0a1b2c3d", Target: "/v"}} },
		"share relative":   func(s *JobSpec) { s.Seed.Share = "share" },
		"shared name": func(s *JobSpec) {
			s.Seed.Share, s.Seed.SharedVolumes = "/fleet-share", []Volume{{Name: "../x", Target: "/v"}}
		},
		"shared target": func(s *JobSpec) {
			s.Seed.Share, s.Seed.SharedVolumes = "/fleet-share", []Volume{{Name: "0a1b2c3d", Target: "v"}}
		},
		"workspace owner": func(s *JobSpec) { s.Seed.Workspace.Owner.GID = -2 },
		"workers":         func(s *JobSpec) { s.Seed.Workers = -1 },
		"cache id":        func(s *JobSpec) { s.Seed.Cache = "a/b" },
		"cache on reset":  func(s *JobSpec) { s.Gen, s.Seed.Reset, s.Seed.PrevGen, s.Seed.Cache = 2, true, 1, "abc" },
	} {
		s := validSeedSpec()
		seed := *s.Seed
		remap := *seed.Remap
		owner := *seed.Workspace.Owner
		seed.Remap, seed.Workspace = &remap, &Workspace{Owner: &owner}
		seed.Volumes = append([]Volume(nil), seed.Volumes...)
		s.Seed = &seed
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, s)
		}
	}
}

func TestLoadJobSpec(t *testing.T) {
	dir := t.TempDir()
	spec := validSeedSpec()
	spec.StallTimeout, spec.ResultHold = "90s", "1m"
	data, _ := json.Marshal(spec)
	// A newer daemon's extra fields are ignored.
	data = []byte(strings.Replace(string(data), `{"v":1`, `{"v":1,"from_the_future":true`, 1))
	file := filepath.Join(dir, "job.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJobSpec(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, spec)
	}
	if stall, hold := got.durations(); stall != 90*time.Second || hold != time.Minute {
		t.Fatalf("durations %s %s", stall, hold)
	}
	if _, err := LoadJobSpec(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing spec loaded")
	}
	if err := os.WriteFile(file, []byte(`{"v":1,"op":"seed"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJobSpec(file); err == nil {
		t.Fatal("a truncated spec loaded")
	}
}

func TestManifestRoundTripAndVersions(t *testing.T) {
	m := Manifest{V: ManifestVersion, Gen: 2, SeededAt: time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC),
		Dirs: []string{"etc", "usr"}, RootExtra: []string{"workspaces"}, Files: 3, Bytes: 4,
		Remap: &RemapResult{User: "vscode", FromUID: 1000, FromGID: 1000, UID: 1234, GID: 1234, Changed: true}}
	for range maxWarnings + 5 {
		m.addWarning("w %d", 1)
	}
	if len(m.Warnings) != maxWarnings+1 || !strings.Contains(m.Warnings[maxWarnings], "more") {
		t.Fatalf("warnings not capped: %d", len(m.Warnings))
	}
	data, sha := m.encode()
	if len(sha) != 64 || sha != sha256Hex(data) {
		t.Fatalf("sha %q", sha)
	}
	file := filepath.Join(t.TempDir(), "rootfs.json")
	if err := writeDurable(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(file)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, m)
	}
	newer, err := ParseManifest([]byte(`{"v":99,"gen":7,"dirs":["etc"],"new_thing":1}`))
	if !errors.Is(err, ErrNewerManifest) || newer.Gen != 7 || newer.Dirs[0] != "etc" {
		t.Fatalf("a newer manifest: %+v, %v (want its known fields and ErrNewerManifest)", newer, err)
	}
	if _, err := ParseManifest([]byte(`{"gen":1}`)); err == nil {
		t.Fatal("a manifest without a version parsed")
	}
}

func TestLayoutNames(t *testing.T) {
	for got, want := range map[string]string{
		RootSubPath("etc"):             "root/etc",
		RootExtraSubPath("workspaces"): "root-extra/workspaces",
		VolumeSubPath("docker"):        "vol/docker",
		BinaryName("ab12"):             "fleet-ab12",
		trashDir(DirRoot, 3):           "root.g3.trash",
		partialDir(DirRootExtra, 4):    "root-extra.g4.partial",
		trashManifest(5):               "meta/rootfs.g5.json",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if !trashName.MatchString(trashDir(DirRootExtra, 12)) || !partialName.MatchString(partialDir(DirRoot, 1)) {
		t.Fatal("the cleanup patterns miss the names the layout makes")
	}
}
