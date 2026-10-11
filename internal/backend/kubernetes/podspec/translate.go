package podspec

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// Result is a translated run: the pod, and what its two Secrets hold.
type Result struct {
	Pod     PodSpec
	Secrets Secrets
}

// Translate turns a captured docker run into the instance pod. A run the
// cluster cannot honour fails with every reason it has (Refusals), each
// error starting with its code; a translated one may carry warnings
// (PodSpec.Warnings), which the create reports.
func Translate(in Input) (*Result, error) {
	if in.Capture == nil {
		return nil, errors.New("podspec: no captured docker run")
	}
	run, err := in.Capture.Run()
	if err != nil {
		return nil, fmt.Errorf("reading the captured docker run: %w", err)
	}
	container, err := kubeshim.ContainerFor(in.Capture, in.Image)
	if err != nil {
		return nil, err
	}
	merged, err := parseMerged(in.Merged)
	if err != nil {
		return nil, err
	}
	t := &translation{
		in:        in,
		cluster:   in.Cluster,
		run:       run,
		container: container,
		merged:    merged,
		host:      in.Host,
		devID:     devcontainerID(in.Capture.Labels),
	}
	if t.cluster == nil {
		t.cluster = &fleet.KubeCluster{}
	}
	if t.host == nil {
		t.host = osFS{}
	}
	t.spec.Image = Image{Ref: run.Image, ID: in.Image.ID, Os: in.Image.Os, Arch: in.Image.Architecture, Variant: in.Image.Variant}

	t.applyOptions()
	t.finish()
	if err := joinRefusals(t.refusals); err != nil {
		return nil, err
	}
	secrets, err := t.secrets()
	if err != nil {
		return nil, err
	}
	return &Result{Pod: t.spec, Secrets: secrets}, nil
}

// translation is one Translate in progress: the pod so far, the refusals,
// and what the options asked for that is only decided once every option has
// been read (privilege, the hostname, resources).
type translation struct {
	in        Input
	cluster   *fleet.KubeCluster
	run       *kubeshim.RunArgs
	container kubeshim.Container
	merged    *mergedConfig
	host      HostFS
	// devID is the run's ${devcontainerId}.
	devID string

	spec     PodSpec
	refusals []*Refusal

	// privileged is --privileged; needsPrivileged lists what else needs a
	// privileged dev container (--device), and needsAllowed what needs
	// privileged pods allowed without making the dev container one (host
	// namespaces), each as written.
	privileged      bool
	needsPrivileged []string
	needsAllowed    []string
	// sysbox marks --privileged (or the docker engine's sidecar) served by
	// a sysbox runtime class.
	sysbox bool
	// dood lists what reaches the docker engine (a docker.sock bind,
	// --use-api-socket), as written; doodPrivileged runs its sidecar
	// privileged.
	dood           []string
	doodPrivileged bool
	// debug are the debug grants asked for.
	debug debugGrants

	hostname, domainname string
	// publishAll is -P; exposed are --expose's ports.
	publishAll bool
	exposed    []int
	// exposedOther are --expose's ports of another protocol, as written.
	exposedOther []string
	// runMounts are the run's mounts in the order given, before fleet's
	// mounts and the image's volumes join them.
	runMounts []runMount
	// res is what the resource options asked for.
	res resourceAsks
	// nodeSelector is what the options pin the pod to (--platform).
	nodeSelector map[string]string
	// exec is exec.json.
	exec execas.Spec
}

// refuse records a refusal.
func (t *translation) refuse(r *Refusal) {
	t.refusals = append(t.refusals, r)
}

// warn records a warning.
func (t *translation) warn(format string, args ...any) {
	t.spec.Warnings = append(t.spec.Warnings, fmt.Sprintf(format, args...))
}

// clusterName names the connection in messages.
func (t *translation) clusterName() string {
	if n := t.cluster.GetDisplayName(); n != "" {
		return n
	}
	return "this cluster"
}

// applyOptions runs each option the run gave through its row of the table,
// in the table's order, and records the outcome.
func (t *translation) applyOptions() {
	given := map[string]bool{}
	for _, name := range t.run.Names() {
		given[name] = true
	}
	// Delta rows without an apply share a reason now and then (the health
	// check options); their warnings are merged, one per reason.
	var deltaReasons []string
	deltaFlags := map[string][]string{}
	for _, opt := range options {
		if !given[opt.Name] {
			continue
		}
		delete(given, opt.Name)
		vals := t.run.Values(opt.Name)
		before := len(t.refusals)
		var outcome Outcome
		switch {
		case opt.apply != nil:
			outcome = opt.apply(t, vals)
		case allFalse(vals):
			// A boolean option turned off (--oom-kill-disable=false) asks
			// for nothing.
			outcome = Ignore
		case opt.Outcome == Delta:
			if _, ok := deltaFlags[opt.Reason]; !ok {
				deltaReasons = append(deltaReasons, opt.Reason)
			}
			shown := flagValues(opt.Name, vals)
			if keysOnly[opt.Name] {
				shown = flagKeys(opt.Name, vals)
			}
			deltaFlags[opt.Reason] = append(deltaFlags[opt.Reason], shown)
		case opt.Outcome == Refuse:
			t.refuse(refuseOption(opt.Name, flagValues(opt.Name, vals)+": "+opt.Reason))
		}
		if outcome == "" && allFalse(vals) {
			outcome = Ignore
		}
		if outcome == "" {
			outcome = opt.Outcome
		}
		if len(t.refusals) > before {
			outcome = Refuse
		}
		t.spec.Options = append(t.spec.Options, OptionOutcome{Option: opt.Name, Outcome: outcome})
	}
	for _, reason := range deltaReasons {
		t.warn("%s: %s", strings.Join(deltaFlags[reason], ", "), reason)
	}
	// An option kubeshim parses that the table lacks is a bug, caught by the
	// table's coverage test; refuse rather than drop it.
	var missing []string
	for name := range given {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.refuse(refuseOption(name, "fleet has no translation for docker run --"+name))
		t.spec.Options = append(t.spec.Options, OptionOutcome{Option: name, Outcome: Refuse})
	}
	sort.Slice(t.spec.Options, func(i, j int) bool { return t.spec.Options[i].Option < t.spec.Options[j].Option })
}

// setOutcome changes the recorded outcome of an option the run gave, for a
// decision only made once every option was read.
func (t *translation) setOutcome(name string, o Outcome) {
	for i := range t.spec.Options {
		if t.spec.Options[i].Option == name && t.spec.Options[i].Outcome != Refuse {
			t.spec.Options[i].Outcome = o
		}
	}
}

// allFalse reports whether every value given is "false": a boolean option
// turned off.
func allFalse(vals []string) bool {
	for _, v := range vals {
		if v != "false" {
			return false
		}
	}
	return len(vals) > 0
}

// keysOnly are the key=value options a message names by key alone: a log
// driver's options carry its credentials (splunk-token, ...).
var keysOnly = map[string]bool{"log-opt": true}

// flagKeys writes a key=value option with the keys it was given:
// "--log-opt max-size, splunk-token".
func flagKeys(name string, vals []string) string {
	var keys []string
	for _, v := range vals {
		k, _, _ := strings.Cut(v, "=")
		keys = appendUnique(keys, k)
	}
	return flagText(name, strings.Join(keys, ", "))
}

// flagValues writes an option with every value it was given:
// "--storage-opt size=120G", or "--rm" for a boolean.
func flagValues(name string, vals []string) string {
	var shown []string
	for _, v := range vals {
		if v != "true" {
			shown = append(shown, v)
		}
	}
	return flagText(name, strings.Join(shown, ", "))
}

// finish decides what depends on every option at once.
func (t *translation) finish() {
	t.finishMounts()
	t.finishPorts()
	t.finishPrivilege()
	t.finishDocker()
	t.finishDebug()
	t.finishCaps()
	t.finishProcess()
	t.finishHostname()
	t.finishResources()
	t.finishNodeSelector()
	t.finishEnv()
	t.finishExec()
}

// lastBool is a boolean option's last value.
func lastBool(vals []string) bool {
	return len(vals) > 0 && vals[len(vals)-1] == "true"
}

// last is a single-valued option's value: docker keeps the last one given.
func last(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// flagText writes an option as a user would have: "--name value".
func flagText(name, value string) string {
	if value == "" {
		return "--" + name
	}
	return "--" + name + " " + value
}

// hasCap reports whether the dev container gets capability c (or ALL, or
// runs privileged).
func (t *translation) hasCap(c string) bool {
	if t.spec.Security.Privileged {
		return true
	}
	for _, a := range t.spec.Security.CapAdd {
		if a == c || a == "ALL" {
			return true
		}
	}
	return false
}

// joinNames lists names for a message: "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
