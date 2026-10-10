package podseed

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// JobSpecVersion is the job.json version this fleet writes and reads.
const JobSpecVersion = 1

// maxJobSpecSize caps job.json.
const maxJobSpecSize = 1 << 20

// Op is what a pod-job does to the instance PVC.
type Op string

const (
	// OpSeed copies the image into root generation Gen (after moving the
	// previous one to trash when Seed.Reset is set: a rebuild).
	OpSeed Op = "seed"
	// OpRollback restores generation Gen from trash, discarding whatever
	// newer generation a failed rebuild left.
	OpRollback Op = "rollback"
	// OpCommit deletes every trashed generation older than Gen once a
	// rebuild to Gen has succeeded.
	OpCommit Op = "commit"
)

// JobSpec is job.json, what fleetd stages for `fleet pod-job`. Unknown fields
// are ignored, so an older pod binary still runs a newer daemon's spec; a
// newer V is refused.
type JobSpec struct {
	V  int `json:"v"`
	Op Op  `json:"op"`
	// Gen is the root generation the job seeds, restores or commits (≥ 1).
	Gen int `json:"gen"`
	// BinarySHA256 is the staged binary's sha256: pod-job refuses to run as
	// any other binary. "" skips the check (tests, a hand-run job).
	BinarySHA256 string `json:"binary_sha256,omitempty"`
	// PVC is where the Job mounts the whole instance PVC.
	PVC string `json:"pvc"`
	// Tools is the tools emptyDir (jobboot.Paths.Dir); "" is
	// jobboot.ToolsDir.
	Tools string `json:"tools,omitempty"`
	// TerminationLog is the container's terminationMessagePath; "" is the
	// kubelet's default.
	TerminationLog string `json:"termination_log,omitempty"`
	// StallTimeout and ResultHold override jobboot's defaults (Go duration
	// strings).
	StallTimeout string `json:"stall_timeout,omitempty"`
	ResultHold   string `json:"result_hold,omitempty"`
	// Seed is what an OpSeed copies.
	Seed *SeedSpec `json:"seed,omitempty"`
}

// SeedSpec is an OpSeed's work.
type SeedSpec struct {
	// Reset moves generation PrevGen to trash before seeding: a rebuild.
	Reset   bool `json:"reset,omitempty"`
	PrevGen int  `json:"prev_gen,omitempty"`
	// Source is the image root to copy; "" is "/", the seed pod's own root.
	Source string `json:"source,omitempty"`
	// Exclude names more top-level dirs never to copy, beyond /proc, /sys,
	// /dev and whatever is mounted in the seed pod (the PVC, the tools dir).
	Exclude []string `json:"exclude,omitempty"`
	// MountDirs are the instance pod's directory mount targets (the
	// workspace folder, volumes, tmpfs, share mounts, /opt/fleet and the
	// rest), absolute. A target's top-level dir that the image lacks gets a
	// root-extra/ dir; image content under a target is not copied.
	MountDirs []string `json:"mount_dirs,omitempty"`
	// Hostname is the instance pod's hostname, written to meta/hostname.
	Hostname string `json:"hostname"`
	// Remap rewrites remoteUser to the share group's ids; nil keeps the
	// image's.
	Remap *Remap `json:"remap,omitempty"`
	// Volumes are copied up from the image on first use.
	Volumes []Volume `json:"volumes,omitempty"`
	// Workspace makes the job wait for the workspace tar on its socket;
	// nil seeds none (a rebuild keeps ws/).
	Workspace *Workspace `json:"workspace,omitempty"`
	// Workers is how many files are copied at once; 0 is 8.
	Workers int `json:"workers,omitempty"`
}

// Remap is the devcontainer CLI's updateRemoteUserUID, done in the seed:
// remoteUser takes UID and GID (the share group's), and its home is chowned.
type Remap struct {
	User string `json:"user"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

// Volume is one of the instance's volumes.
type Volume struct {
	// Name is its dir under vol/.
	Name string `json:"name"`
	// Target is where the instance mounts it.
	Target string `json:"target"`
	// CopyUp fills a new volume from the image's content at Target, as
	// docker does for a named volume (false: nocopy, or a bind).
	CopyUp bool `json:"copy_up,omitempty"`
}

// Workspace is how the job takes the workspace.
type Workspace struct {
	// Owner, when set, owns every extracted entry (the tar's headers
	// rewritten to the share uid); nil keeps the tar's own owners.
	Owner *IDs `json:"owner,omitempty"`
}

// IDs are a uid and gid.
type IDs struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// volumeName is a name that is one path component.
var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// LoadJobSpec reads and checks job.json at file.
func LoadJobSpec(file string) (JobSpec, error) {
	f, err := os.Open(file)
	if err != nil {
		return JobSpec{}, fmt.Errorf("read job spec: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxJobSpecSize+1))
	if err != nil {
		return JobSpec{}, fmt.Errorf("read job spec %s: %w", file, err)
	}
	if len(data) > maxJobSpecSize {
		return JobSpec{}, fmt.Errorf("job spec %s is larger than %d bytes", file, maxJobSpecSize)
	}
	var spec JobSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return JobSpec{}, fmt.Errorf("parse job spec %s: %w", file, err)
	}
	if err := spec.Validate(); err != nil {
		return JobSpec{}, fmt.Errorf("job spec %s: %w", file, err)
	}
	return spec, nil
}

// Validate checks the spec can be run as written.
func (s JobSpec) Validate() error {
	if s.V != JobSpecVersion {
		return fmt.Errorf("job spec version %d, this fleet runs %d", s.V, JobSpecVersion)
	}
	if s.Gen < 1 {
		return fmt.Errorf("generation %d is not positive", s.Gen)
	}
	if err := checkAbs("pvc", s.PVC); err != nil {
		return err
	}
	if s.PVC == "/" {
		return errors.New("the pvc mount cannot be /")
	}
	if s.Tools != "" {
		if err := checkAbs("tools", s.Tools); err != nil {
			return err
		}
	}
	for name, d := range map[string]string{"stall_timeout": s.StallTimeout, "result_hold": s.ResultHold} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v <= 0 {
			return fmt.Errorf("%s %q is not a positive duration", name, d)
		}
	}
	switch s.Op {
	case OpSeed:
		if s.Seed == nil {
			return errors.New("a seed job needs its seed spec")
		}
		return s.Seed.validate(s.Gen)
	case OpRollback, OpCommit:
		if s.Seed != nil {
			return fmt.Errorf("a %s job takes no seed spec", s.Op)
		}
		return nil
	}
	return fmt.Errorf("unknown op %q", s.Op)
}

func (s SeedSpec) validate(gen int) error {
	if s.Reset && (s.PrevGen < 1 || s.PrevGen >= gen) {
		return fmt.Errorf("a reset of generation %d needs a previous generation below it, not %d", gen, s.PrevGen)
	}
	if s.Source != "" {
		if err := checkAbs("source", s.Source); err != nil {
			return err
		}
	}
	for _, name := range s.Exclude {
		if !isComponent(name) {
			return fmt.Errorf("exclude %q is not a top-level dir name", name)
		}
	}
	for _, dir := range s.MountDirs {
		if err := checkAbs("mount dir", dir); err != nil {
			return err
		}
		if dir == "/" {
			return errors.New("a mount dir cannot be /")
		}
	}
	if s.Hostname == "" || strings.ContainsAny(s.Hostname, "/\n\x00 ") {
		return fmt.Errorf("hostname %q is not usable", s.Hostname)
	}
	if r := s.Remap; r != nil {
		if r.User == "" || strings.ContainsAny(r.User, ":\n\x00") || r.UID < 0 || r.GID < 0 {
			return fmt.Errorf("remap %+v is not a user and two ids", *r)
		}
	}
	seen := map[string]bool{}
	for _, v := range s.Volumes {
		if !volumeName.MatchString(v.Name) || seen[v.Name] {
			return fmt.Errorf("volume name %q is not one unique path component", v.Name)
		}
		seen[v.Name] = true
		if err := checkAbs("volume target", v.Target); err != nil {
			return err
		}
	}
	if w := s.Workspace; w != nil && w.Owner != nil && (w.Owner.UID < 0 || w.Owner.GID < 0) {
		return fmt.Errorf("workspace owner %+v is negative", *w.Owner)
	}
	if s.Workers < 0 {
		return fmt.Errorf("workers %d is negative", s.Workers)
	}
	return nil
}

// checkAbs reports whether p is a clean absolute path.
func checkAbs(name, p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("%s %q is not a clean absolute path", name, p)
	}
	return nil
}

// isComponent reports whether name is a single, ordinary path component.
func isComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// durations is the spec's stall timeout and result hold, 0 for jobboot's
// defaults.
func (s JobSpec) durations() (stall, hold time.Duration) {
	stall, _ = time.ParseDuration(s.StallTimeout)
	hold, _ = time.ParseDuration(s.ResultHold)
	return stall, hold
}
