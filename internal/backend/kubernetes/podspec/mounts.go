package podspec

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/hostbind"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The mount options. Each mount is read as docker reads it, then classified
// by where its data lives on a cluster (MountKind):
//
//	bind of the workspace (or a folder in it)  → MountWorkspace (ws/)
//	${devcontainerId}, anonymous, image volume → MountVolume (vol/, copy-up)
//	any other named volume                     → MountSharedVolume (share .vol/, copy-up)
//	tmpfs, --shm-size                          → MountTmpfs
//	bind of the user's ssh-agent socket        → MountSSHAgent
//	bind of docker.sock, --use-api-socket      → MountDockerSocket (DooD)
//	bind of another socket / socket directory  → MountHostSocket(Dir)
//	bind of another host directory / file      → MountHostDir / MountHostFile (share .hostbind/, synced)
//	bind of a device or FIFO                   → refused
//
// Each bind source is stat-ed on the daemon host (Input.Host). A fleet mount
// of a custom mount's chosen host folder (MountScopeHostFolder) is a
// MountHostDir too.

// reservedTargets are the container paths fleet's own mounts own; a run
// mount at or below one is refused.
var reservedTargets = []string{"/opt/fleet", control.ContainerMountDir}

// runMount is a classified run mount and the option it came from.
type runMount struct {
	Mount
	// option is the option's canonical name ("mount", "volume").
	option string
}

// rawMount is one mount as the run wrote it.
type rawMount struct {
	// option is the option and value as written, for messages.
	option string
	typ    string // bind, volume, tmpfs, ...
	source string
	target string
	ro     bool
	noCopy bool
	// subpath is volume-subpath.
	subpath string
	// tmpfsSize is in bytes; 0 is unlimited.
	tmpfsSize int64
	// fromVolumeFlag marks -v, which creates a missing bind source where
	// --mount refuses it.
	fromVolumeFlag bool
}

func (t *translation) optMount(vals []string) Outcome {
	for _, v := range vals {
		m, err := parseMountOption(v)
		if err != nil {
			t.refuse(refuseOption("mount", err.Error()))
			continue
		}
		t.addRunMount(m)
	}
	return ""
}

func (t *translation) optVolume(vals []string) Outcome {
	for _, v := range vals {
		m, err := parseVolumeOption(v)
		if err != nil {
			t.refuse(refuseOption("volume", err.Error()))
			continue
		}
		t.addRunMount(m)
	}
	return ""
}

func (t *translation) optTmpfs(vals []string) Outcome {
	for _, v := range vals {
		target, opts, _ := strings.Cut(v, ":")
		if !path.IsAbs(target) {
			t.refuse(refuseOption("tmpfs", fmt.Sprintf("invalid --tmpfs %q: the path must be absolute", v)))
			continue
		}
		m := rawMount{option: flagText("tmpfs", v), typ: "tmpfs", target: target}
		var unused []string
		for _, o := range strings.Split(opts, ",") {
			key, val, _ := strings.Cut(o, "=")
			switch key {
			case "":
			case "size":
				n, err := dockerBytes(val)
				if err != nil {
					unused = append(unused, o)
					continue
				}
				m.tmpfsSize = n
			case "ro":
				m.ro = true
			case "rw":
			case "mode":
				if val != "1777" && val != "01777" {
					unused = append(unused, o)
				}
			default:
				unused = append(unused, o)
			}
		}
		if len(unused) > 0 {
			t.warn("%s: a memory-backed emptyDir takes only a size; %s not applied", m.option, strings.Join(unused, ","))
		}
		t.addRunMount(m)
	}
	return ""
}

func (t *translation) optShmSize(vals []string) Outcome {
	n, err := dockerBytes(last(vals))
	if err != nil || n <= 0 {
		t.refuse(refuseOption("shm-size", fmt.Sprintf("invalid --shm-size %q", last(vals))))
		return ""
	}
	t.addRunMount(rawMount{option: flagText("shm-size", last(vals)), typ: "tmpfs", target: "/dev/shm", tmpfsSize: n})
	return ""
}

func (t *translation) optReadOnly(vals []string) Outcome {
	if lastBool(vals) {
		t.spec.ReadOnlyRoot = true
		t.spec.Security.ReadOnlyRootFilesystem = true
	}
	return ""
}

func (t *translation) optVolumeDriver(vals []string) Outcome {
	if v := last(vals); v != "" && v != "local" {
		t.refuse(refuseOption("volume-driver", fmt.Sprintf("--volume-driver %s: volumes live on the cluster's storage; only the local driver has a meaning there", v)))
	}
	return ""
}

func (t *translation) optUseAPISocket(vals []string) Outcome {
	if !lastBool(vals) {
		return Ignore
	}
	t.dood = append(t.dood, "--use-api-socket")
	t.runMounts = append(t.runMounts, runMount{Mount{Kind: MountDockerSocket, Target: "/var/run/docker.sock"}, "use-api-socket"})
	return ""
}

// parseMountOption reads docker's --mount: comma-separated key=value fields
// (CSV, so a field may be quoted), type defaulting to volume.
func parseMountOption(v string) (rawMount, error) {
	fields, err := csv.NewReader(strings.NewReader(v)).Read()
	if err != nil {
		return rawMount{}, fmt.Errorf("invalid --mount %q: %v", v, err)
	}
	m := rawMount{option: flagText("mount", v), typ: "volume"}
	for _, f := range fields {
		key, val, hasValue := strings.Cut(f, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		boolVal := func() (bool, error) {
			if !hasValue {
				return true, nil
			}
			b, err := strconv.ParseBool(val)
			if err != nil {
				return false, fmt.Errorf("invalid --mount %q: invalid value for %s: %s", v, key, val)
			}
			return b, nil
		}
		switch key {
		case "type":
			m.typ = strings.ToLower(val)
		case "source", "src":
			m.source = val
		case "target", "dst", "destination":
			m.target = val
		case "readonly", "ro":
			if m.ro, err = boolVal(); err != nil {
				return rawMount{}, err
			}
		case "volume-nocopy":
			if m.noCopy, err = boolVal(); err != nil {
				return rawMount{}, err
			}
		case "volume-subpath":
			m.subpath = val
		case "tmpfs-size":
			n, err := dockerBytes(val)
			if err != nil {
				return rawMount{}, fmt.Errorf("invalid --mount %q: invalid tmpfs-size %s", v, val)
			}
			m.tmpfsSize = n
		case "volume-driver":
			if val != "local" && val != "" {
				return rawMount{}, fmt.Errorf("--mount %s: volume driver %s; volumes live on the cluster's storage, where only the local driver has a meaning", v, val)
			}
		case "volume-opt":
			return rawMount{}, fmt.Errorf("--mount %s: volume-opt mounts a device or a network share on the docker host, which a pod cannot", v)
		case "bind-propagation", "bind-nonrecursive", "bind-recursive", "consistency", "volume-label", "tmpfs-mode":
			// No meaning for storage a pod mounts from its own disk, which
			// has no submounts and no peers to propagate to.
		default:
			return rawMount{}, fmt.Errorf("invalid --mount %q: unknown option %s", v, key)
		}
	}
	if m.target == "" {
		return rawMount{}, fmt.Errorf("invalid --mount %q: no target", v)
	}
	return m, nil
}

// volumeModes are the options docker's -v takes after the target.
var volumeModes = map[string]bool{
	"rw": true, "ro": true, "z": true, "Z": true, "nocopy": true,
	"shared": true, "rshared": true, "slave": true, "rslave": true, "private": true, "rprivate": true,
	"consistent": true, "cached": true, "delegated": true,
}

// parseVolumeOption reads docker's -v [source:]target[:mode] (Linux).
func parseVolumeOption(v string) (rawMount, error) {
	parts := strings.Split(v, ":")
	m := rawMount{option: flagText("volume", v), fromVolumeFlag: true}
	var modes string
	switch len(parts) {
	case 1:
		m.target = parts[0]
	case 2:
		if allModes(parts[1]) {
			return rawMount{}, fmt.Errorf("invalid volume specification: %q: an anonymous volume takes no mode", v)
		}
		m.source, m.target = parts[0], parts[1]
	case 3:
		m.source, m.target, modes = parts[0], parts[1], parts[2]
		if !allModes(modes) {
			return rawMount{}, fmt.Errorf("invalid volume specification: %q: invalid mode %s", v, modes)
		}
	default:
		return rawMount{}, fmt.Errorf("invalid volume specification: %q", v)
	}
	switch {
	case m.source == "":
		m.typ = "volume"
	case strings.HasPrefix(m.source, "/"):
		m.typ = "bind"
	case strings.HasPrefix(m.source, "."):
		return rawMount{}, fmt.Errorf("-v %s: a relative bind source cannot be resolved; use an absolute path", v)
	default:
		m.typ = "volume"
	}
	for _, mode := range strings.Split(modes, ",") {
		switch mode {
		case "ro":
			m.ro = true
		case "nocopy":
			m.noCopy = true
		}
	}
	return m, nil
}

func allModes(s string) bool {
	for _, m := range strings.Split(s, ",") {
		if !volumeModes[m] {
			return false
		}
	}
	return true
}

// volumeNameRE is a valid docker volume name.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// addRunMount classifies a run mount.
func (t *translation) addRunMount(m rawMount) {
	if !path.IsAbs(m.target) {
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: the target %q is not an absolute path", m.option, m.target)))
		return
	}
	target := cleanTarget(m.target)
	if target == "/" {
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: a mount cannot replace the container's root", m.option)))
		return
	}
	switch m.typ {
	case "bind":
		t.addBind(m, target)
	case "volume":
		t.addVolume(m, target)
	case "tmpfs":
		mt := Mount{Kind: MountTmpfs, Target: target, ReadOnly: m.ro}
		if m.tmpfsSize > 0 {
			mt.SizeLimit = formatBytes(m.tmpfsSize)
		}
		t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
	default:
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: type=%s mounts have no pod equivalent", m.option, m.typ)))
	}
}

// optionOf names the option a mount came from.
func optionOf(m rawMount) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(m.option, "--"), " ")
	return name
}

func (t *translation) addVolume(m rawMount, target string) {
	if m.source != "" && !volumeNameRE.MatchString(m.source) {
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: invalid volume name %q", m.option, m.source)))
		return
	}
	sub := ""
	if m.subpath != "" {
		clean := path.Clean(m.subpath)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: volume-subpath %q must stay inside the volume", m.option, m.subpath)))
			return
		}
		sub = "/" + clean
	}
	mt := Mount{Kind: MountVolume, Target: target, ReadOnly: m.ro, Source: m.source, CopyUp: !m.noCopy && sub == ""}
	switch {
	case m.source == "":
		mt.Anonymous = true
		mt.SubPath = podseed.VolumeSubPath("anon-" + sha8(target))
	case t.devID != "" && strings.Contains(m.source, t.devID):
		// ${devcontainerId}: this container's own volume.
		mt.SubPath = podseed.VolumeSubPath(m.source) + sub
	default:
		mt.Kind = MountSharedVolume
		mt.SubPath = SharedVolumeSubPath(m.source) + sub
	}
	t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
}

func (t *translation) addBind(m rawMount, target string) {
	if !filepath.IsAbs(m.source) {
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: the bind source %q is not an absolute path", m.option, m.source)))
		return
	}
	source := filepath.Clean(m.source)
	mt := Mount{Target: target, ReadOnly: m.ro, Source: source}
	if ws := t.in.WorkspaceDir; ws != "" {
		if rel, err := filepath.Rel(filepath.Clean(ws), source); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			mt.Kind = MountWorkspace
			if rel != "." {
				mt.SubPath = filepath.ToSlash(rel)
			}
			t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
			return
		}
	}
	for _, sock := range t.in.HostAgentSockets {
		if sock != "" && filepath.Clean(sock) == source {
			mt.Kind = MountSSHAgent
			t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
			return
		}
	}
	if dockerSocketPath(source) {
		// The pod's own engine serves it (the fleet-docker sidecar),
		// never the daemon host's, so whatever is at the source there
		// (a socket, a link, nothing on a host whose docker lives
		// elsewhere) does not matter.
		mt.Kind = MountDockerSocket
		t.dood = append(t.dood, m.option)
		t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
		return
	}
	info, err := t.host.Stat(source)
	switch {
	case errors.Is(err, fs.ErrNotExist) && m.fromVolumeFlag:
		// docker creates a missing -v source as an empty directory.
		mt.Kind, mt.Mode = MountHostDir, 0o755
	case errors.Is(err, fs.ErrNotExist):
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: bind source path does not exist: %s", m.option, source)))
		return
	case err != nil:
		t.refuse(refuseOption(optionOf(m), fmt.Sprintf("%s: %v", m.option, err)))
		return
	default:
		mt.Kind, err = t.classifyHostPath(source, info)
		if err != nil {
			t.refuse(&Refusal{Code: CodeHostSpecialFile, Reason: fmt.Sprintf("%s: %v", m.option, err)})
			return
		}
		if mt.Kind == MountHostDir {
			mt.Mode = uint32(info.Mode().Perm())
		}
	}
	if mt.Kind == MountHostDir || mt.Kind == MountHostFile {
		if !t.hostBindable(m.option, source) {
			return
		}
		if mt.Kind == MountHostFile {
			// A bound file that is a link (a dotfiles manager's
			// ~/.gitconfig) binds the file it links to, as docker does: the
			// copy is of that file, kept in step with it, never a link that
			// dangles in the pod (nor one a pod's write turns into a file
			// over the user's link on the daemon host).
			if real, err := t.host.EvalSymlinks(source); err == nil && real != source {
				source, mt.Source = real, real
			}
		}
		mt.SubPath = hostbind.Name(source)
	}
	t.runMounts = append(t.runMounts, runMount{mt, optionOf(m)})
}

// hostBindable reports whether a daemon-host folder or file can be bound
// into a pod, whose copy of it fleet's sync keeps: never fleet's own state
// (the daemon's files would be copied into the pod, and the pod's back over
// them), as written or with links followed, refused naming the option.
func (t *translation) hostBindable(option, source string) bool {
	own := t.in.FleetDir
	if own == "" {
		return true
	}
	for _, o := range t.realPaths(own) {
		for _, p := range t.realPaths(source) {
			if hostbind.Overlaps(p, o) {
				t.refuse(&Refusal{Code: CodeHostMount, Reason: fmt.Sprintf("%s: %s overlaps fleet's own state on the daemon host (%s), which never syncs into a pod", option, source, own)})
				return false
			}
		}
	}
	return true
}

// realPaths is daemon-host path p cleaned, and with its links followed when
// that differs.
func (t *translation) realPaths(p string) []string {
	out := []string{filepath.Clean(p)}
	if real, err := t.host.EvalSymlinks(p); err == nil && real != out[0] {
		out = append(out, real)
	}
	return out
}

// SharedVolumeSubPath is the folder on the fleet's share of the named volume
// called name: podshare.VolumeSubPath of a hash of the name, which may not
// be a valid path component everywhere.
func SharedVolumeSubPath(name string) string {
	return path.Join(podseed.SharedVolumesDir, sha8(name))
}

// sharedVolumeID is the folder name a MountSharedVolume's SubPath names in
// the share's volumes folder.
func sharedVolumeID(subPath string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(subPath, podseed.SharedVolumesDir+"/"), "/")
	return id
}

// hostFileLink is the link a host file bind's target becomes: to the file in
// its copy's folder, as the pod mounts it.
func hostFileLink(m Mount) podinit.Link {
	return podinit.Link{Path: m.Target, Target: path.Join(hostbind.FileDir(m.SubPath), filepath.Base(m.Source))}
}

// dockerSocketPath reports a bind source that is the docker engine's socket
// (/var/run/docker.sock, a rootless engine's $XDG_RUNTIME_DIR/docker.sock,
// Docker Desktop's ~/.docker/run/docker.sock).
func dockerSocketPath(source string) bool {
	return filepath.Base(source) == "docker.sock"
}

// classifyHostPath says what a pod gets for a daemon-host bind source.
func (t *translation) classifyHostPath(source string, info fs.FileInfo) (MountKind, error) {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSocket != 0:
		return MountHostSocket, nil
	case mode.IsDir():
		if t.onlySockets(source) {
			return MountHostSocketDir, nil
		}
		return MountHostDir, nil
	case mode.IsRegular():
		return MountHostFile, nil
	case mode&fs.ModeDevice != 0:
		return "", fmt.Errorf("%s is a device on the daemon host, which a pod cannot reach; pass it with --device to a privileged pod instead", source)
	case mode&fs.ModeNamedPipe != 0:
		return "", fmt.Errorf("%s is a FIFO on the daemon host, which a pod cannot reach", source)
	}
	return "", fmt.Errorf("%s is a special file on the daemon host, which a pod cannot reach", source)
}

// onlySockets reports whether dir holds unix sockets and nothing else (an
// X11 or pulse socket directory), which is relayed rather than synced.
func (t *translation) onlySockets(dir string) bool {
	entries, err := t.host.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSocket == 0 {
			return false
		}
	}
	return true
}

// cleanTarget is a container path in clean form.
func cleanTarget(p string) string {
	return path.Clean(p)
}

// sha8 names a volume's folder without its name, which may not be a valid
// path component everywhere: the first 8 hex digits of its SHA-256.
func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// underTarget reports whether p is target or below it.
func underTarget(p, target string) bool {
	return p == target || strings.HasPrefix(p, strings.TrimSuffix(target, "/")+"/")
}

// finishMounts puts the mounts together: the run's (refusing duplicates and
// fleet's reserved paths), fleet's own (which win over a run mount at the
// same target, as on docker), and an anonymous volume for each image VOLUME
// nothing else mounts.
func (t *translation) finishMounts() {
	fleetTargets := map[string]bool{}
	var fleetMounts []Mount
	for _, fm := range t.in.FleetMounts {
		target := cleanTarget(fm.ContainerPath)
		switch fm.Scope {
		case backend.MountScopeFleetShared:
			fleetMounts = append(fleetMounts, Mount{Kind: MountFleet, Target: target, ReadOnly: fm.ReadOnly, SubPath: fm.SubPath})
		case backend.MountScopeHostFolder:
			// A custom mount's chosen host folder: its copy on the share,
			// kept in step with it as a host bind's is.
			source := filepath.Clean(fm.LocalPath)
			if !t.hostBindable("custom mount "+target, source) {
				continue
			}
			mode := uint32(0o755)
			if info, err := t.host.Stat(source); err == nil && info.IsDir() {
				mode = uint32(info.Mode().Perm())
			}
			fleetMounts = append(fleetMounts, Mount{Kind: MountHostDir, Target: target, ReadOnly: fm.ReadOnly, Source: source, SubPath: hostbind.Name(source), Mode: mode})
		default:
			t.refuse(&Refusal{Code: CodeHostMount, Reason: fmt.Sprintf("fleet mount %s is a daemon-host path (%s), which a pod cannot bind", target, fm.LocalPath)})
			continue
		}
		fleetTargets[target] = true
	}

	seen := map[string]bool{}
	var mounts []Mount
	for _, rm := range t.runMounts {
		m := rm.Mount
		if fleetTargets[m.Target] {
			t.warn("the mount at %s is replaced by fleet's own mount there, as on docker", m.Target)
			continue
		}
		reserved := false
		for _, r := range reservedTargets {
			if underTarget(m.Target, r) {
				t.refuse(refuseOption(rm.option, fmt.Sprintf("fleet reserves %s in every instance; the mount at %s cannot go there", r, m.Target)))
				reserved = true
			}
		}
		if reserved {
			continue
		}
		if seen[m.Target] {
			t.refuse(refuseOption(rm.option, fmt.Sprintf("duplicate mount point: %s", m.Target)))
			continue
		}
		seen[m.Target] = true
		mounts = append(mounts, m)
	}
	for _, m := range fleetMounts {
		seen[m.Target] = true
	}
	mounts = append(mounts, fleetMounts...)
	for _, p := range sortedKeys(t.in.Image.Config.Volumes) {
		target := cleanTarget(p)
		if !path.IsAbs(target) || seen[target] {
			continue
		}
		seen[target] = true
		mounts = append(mounts, Mount{Kind: MountVolume, Target: target, SubPath: podseed.VolumeSubPath("anon-" + sha8(target)), CopyUp: true, Anonymous: true})
	}
	t.spec.Mounts = mounts
	for _, m := range mounts {
		if m.Kind == MountHostFile {
			t.spec.PID1.Links = append(t.spec.PID1.Links, hostFileLink(m))
		}
	}
}

// sortedKeys is m's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
