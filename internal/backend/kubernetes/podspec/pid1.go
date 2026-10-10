package podspec

// PID1Format is the version of the pid1.json layout.
const PID1Format = 1

// PID1File is the name of the PID 1 spec in the spec Secret, which the pod
// mounts at /opt/fleet/spec.
const PID1File = "pid1.json"

// PID1 is pid1.json: what pod-init, the dev container's PID 1, does before it
// becomes the devcontainer's own main process, as the captured docker run
// asked for it. Fields docker applies to the container itself, which a pod
// cannot express, are carried out by pod-init instead: the user, the
// working directory, the stop signal, rlimits, the umask, the OOM score, the
// hostname.
//
// The type has no dependencies, so the in-pod reader can share it.
type PID1 struct {
	// Format is PID1Format.
	Format int `json:"format"`
	// Argv is the command to run: the effective entrypoint followed by the
	// command (the devcontainer CLI's start script).
	Argv []string `json:"argv"`
	// User is whom to run Argv as, in docker's user[:group] form (-u, else
	// the image's USER); "" is root.
	User string `json:"user,omitempty"`
	// WorkingDir is where to run it (-w, else the image's WORKDIR); "" is /.
	WorkingDir string `json:"working_dir,omitempty"`
	// Init asks for an init process (--init): pod-init stays PID 1, reaping
	// orphans and forwarding signals, instead of exec'ing Argv.
	Init bool `json:"init,omitempty"`
	// StopSignal is the signal Argv gets when the pod stops ("SIGTERM",
	// "SIGRTMIN+3"): --stop-signal, else the image's STOPSIGNAL; "" is
	// SIGTERM. The runtime sends pod-init SIGTERM (or the image's signal),
	// and pod-init sends this one on.
	StopSignal string `json:"stop_signal,omitempty"`
	// Rlimits are --ulimit's resource limits, set before Argv starts.
	Rlimits []Rlimit `json:"rlimits,omitempty"`
	// Umask is --umask, in octal ("0022").
	Umask string `json:"umask,omitempty"`
	// OOMScoreAdj is --oom-score-adj.
	OOMScoreAdj *int `json:"oom_score_adj,omitempty"`
	// Hostname and Domainname are --hostname and --domainname, set by
	// pod-init when the pod may (it is privileged or has SYS_ADMIN); "" when
	// the pod keeps the name its StatefulSet gives it.
	Hostname   string `json:"hostname,omitempty"`
	Domainname string `json:"domainname,omitempty"`
	// Privileged marks a privileged pod, whose PID 1 may lay out its own
	// cgroup (the fleet-user and fleet-agent children).
	Privileged bool `json:"privileged,omitempty"`
}

// Rlimit is one resource limit; -1 is unlimited.
type Rlimit struct {
	// Name is docker's name for the resource ("nofile", "nproc").
	Name string `json:"name"`
	Soft int64  `json:"soft"`
	Hard int64  `json:"hard"`
}
