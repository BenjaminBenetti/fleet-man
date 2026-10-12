//go:build !nok8s

package server

import (
	"path"
	"slices"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// podagent_sockdirs.go is the daemon host's side of the socket directories
// the pods bind (InstanceRecord.HostSocketDirs: an X11 display's
// /tmp/.X11-unix, a gpg agent's socket directory). Docker binds such a
// directory, so a socket made there after the create shows in the container
// at once; a pod gets one socket of its agent's per socket fleetd finds
// there, so fleetd lists each directory every podAgentsInterval while a pod
// that binds it has a session, and has the pods told when a listing changes
// (a socket came or went). A connection to one is relayed to the socket of
// that name in the recorded directory (hostrelay.InDir), dialled as any
// recorded socket is.

// podSocketDirs lists the pods' socket directories.
type podSocketDirs struct {
	// list lists one directory, and logWarn is fleet.log's warning (tests
	// stand in for both).
	list    func(dir string) ([]hostrelay.DirSocket, error)
	logWarn func(msg string, args ...any)

	mu sync.Mutex
	// listings are each directory's last listing, by its daemon-host path.
	listings map[string]dirListing
	// pods are the pods that bind a socket directory, as last asked
	// about, by connection and StatefulSet.
	pods map[string]gatewayPod
	// warned is the failure last logged, by directory (and "cap" by pod).
	warned map[string]string
}

// dirListing is one listing of a socket directory: its sockets (nil when it
// could not be listed) and when.
type dirListing struct {
	socks []hostrelay.DirSocket
	at    time.Time
}

func newPodSocketDirs() *podSocketDirs {
	return &podSocketDirs{
		list:     hostrelay.ListSockets,
		logWarn:  flog.Warn,
		listings: map[string]dirListing{},
		pods:     map[string]gatewayPod{},
		warned:   map[string]string{},
	}
}

// socketsFor is the sockets t's pod binds for its socket directories now,
// after taken (the targets its own socket binds hold): each socket in each
// directory but the names another bind shadows, at most
// podwire.MaxHostSockets with taken (the first, by directory and name).
func (d *podSocketDirs) socketsFor(t kubernetes.PodTarget, taken []podwire.HostSocket) []podwire.HostSocket {
	if len(t.HostSocketDirs) == 0 {
		return nil
	}
	d.mu.Lock()
	d.pods[t.Cluster+"/"+t.StatefulSet] = gatewayPod{target: t, asked: time.Now()}
	d.mu.Unlock()
	seen := make(map[string]bool, len(taken))
	for _, h := range taken {
		seen[h.Target] = true
	}
	var out []podwire.HostSocket
	dropped := 0
	for _, dir := range t.HostSocketDirs {
		for _, s := range d.current(dir.Source) {
			target := path.Join(dir.Target, s.Name)
			if !dir.Lists(s.Name) || seen[target] || !podwire.ValidHostSocketTarget(target) || path.Dir(target) != dir.Target {
				continue
			}
			if len(taken)+len(out) >= podwire.MaxHostSockets {
				dropped++
				continue
			}
			seen[target] = true
			out = append(out, podwire.HostSocket{Target: target, Mode: s.Mode})
		}
	}
	if dropped > 0 {
		d.warnOnce("cap "+t.Cluster+"/"+t.StatefulSet, "dropped", "pod relay: a socket directory the devcontainer binds holds more sockets than fleet relays into a pod; the rest are left out",
			"fleet", t.Fleet, "instance", t.Instance, "max", podwire.MaxHostSockets, "left_out", dropped)
	} else {
		d.forgetWarning("cap " + t.Cluster + "/" + t.StatefulSet)
	}
	return out
}

// current is dir's listing from the last two intervals, else made now.
func (d *podSocketDirs) current(dir string) []hostrelay.DirSocket {
	d.mu.Lock()
	l, ok := d.listings[dir]
	d.mu.Unlock()
	if ok && time.Since(l.at) < 2*podAgentsInterval {
		return l.socks
	}
	socks, _ := d.relist(dir)
	return socks
}

// relist lists dir now, reporting whether its sockets changed since the last
// listing. A directory that cannot be listed has none, and says so in
// fleet.log once until that changes.
func (d *podSocketDirs) relist(dir string) ([]hostrelay.DirSocket, bool) {
	socks, err := d.list(dir)
	if err != nil {
		d.warnOnce(dir, err.Error(), "pod relay: cannot list a socket directory the devcontainer binds; its sockets are not relayed", "dir", dir, "err", err)
		socks = nil
	} else {
		d.forgetWarning(dir)
	}
	d.mu.Lock()
	prev, had := d.listings[dir]
	d.listings[dir] = dirListing{socks: socks, at: time.Now()}
	d.mu.Unlock()
	return socks, had && !slices.Equal(prev.socks, socks)
}

// tick lists again the directories of the pods that bind one, forgetting
// those that have had no session (live) since podGatewayForget after they
// were last asked about, and reports whether a listing changed.
func (d *podSocketDirs) tick(live func(kubernetes.PodTarget) bool) bool {
	d.mu.Lock()
	pods := make([]gatewayPod, 0, len(d.pods))
	for _, p := range d.pods {
		pods = append(pods, p)
	}
	d.mu.Unlock()
	dirs := map[string]bool{}
	for _, p := range pods {
		t := p.target
		if !live(t) && time.Since(p.asked) >= podGatewayForget {
			d.mu.Lock()
			if cur, ok := d.pods[t.Cluster+"/"+t.StatefulSet]; ok && !cur.asked.After(p.asked) {
				delete(d.pods, t.Cluster+"/"+t.StatefulSet)
			}
			d.mu.Unlock()
			continue
		}
		for _, dir := range t.HostSocketDirs {
			dirs[dir.Source] = true
		}
	}
	d.mu.Lock()
	for dir := range d.listings {
		if !dirs[dir] {
			delete(d.listings, dir)
		}
	}
	d.mu.Unlock()
	changed := false
	for dir := range dirs {
		if _, c := d.relist(dir); c {
			changed = true
		}
	}
	return changed
}

// warnOnce logs msg unless key's last logged failure was why.
func (d *podSocketDirs) warnOnce(key, why, msg string, args ...any) {
	d.mu.Lock()
	logged := d.warned[key] == why
	d.warned[key] = why
	d.mu.Unlock()
	if !logged {
		d.logWarn(msg, args...)
	}
}

func (d *podSocketDirs) forgetWarning(key string) {
	d.mu.Lock()
	delete(d.warned, key)
	d.mu.Unlock()
}
