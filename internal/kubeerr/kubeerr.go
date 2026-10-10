// Package kubeerr is the one vocabulary of the Kubernetes backend's errors.
//
// Every failure the backend reports to a user starts with a code,
// "kubernetes:<class>[:<detail>...]", then ": " and the message: an
// instance's error line, a create job's failure and a refused request all
// carry one. The class says what went wrong in terms a person can act on
// (the cluster is unreachable, admission refused the pod, the disk did not
// bind); the detail narrows it (kubernetes:unsupported:compose,
// kubernetes:admission_denied:debug_capabilities). The TUI routes an
// instance's error to its recovery dialog by class, so the classes are a
// contract: Classes lists them, and a new one is added here, never made up
// at a call site.
//
// The package is pure (no client-go), so clients can read the codes off the
// errors they are sent. Errors made in the backend's own packages carry their
// code through a Code() method (podspec.Refusal, kubeimage.Error, the
// deadline and pod errors); Of finds it on any error, wrapped or not, and
// falls back to the message's own prefix.
package kubeerr

import (
	"errors"
	"fmt"
	"strings"
)

// Prefix starts every code.
const Prefix = "kubernetes:"

// Code is a kubernetes: error code: a class, optionally narrowed
// ("kubernetes:unsupported:flag:--link").
type Code string

// The classes.
const (
	// Unreachable: the API server did not answer.
	Unreachable Code = Prefix + "unreachable"
	// Unauthorized: the cluster refused the connection's credentials (an
	// expired login).
	Unauthorized Code = Prefix + "unauthorized"
	// Forbidden: the connection's identity may not do what fleet needs in
	// its namespace.
	Forbidden Code = Prefix + "forbidden"
	// AdmissionDenied: admission (pod security, a webhook, a policy)
	// refused the instance's objects.
	AdmissionDenied Code = Prefix + "admission_denied"
	// Quota: a ResourceQuota, or the connection's own instance and disk
	// caps, has no room.
	Quota Code = Prefix + "quota"
	// Unsupported: the devcontainer asks for something fleet cannot do on a
	// cluster (compose, a docker run option, a mount kind, image delivery).
	Unsupported Code = Prefix + "unsupported"
	// Misconfigured: a connection setting cannot be used as it is.
	Misconfigured Code = Prefix + "misconfigured"
	// ImageBuild: the image did not build on the daemon host.
	ImageBuild Code = Prefix + "image_build"
	// ImagePush: the image did not reach the cluster's registry or nodes.
	ImagePush Code = Prefix + "image_push"
	// ImagePull: the nodes could not pull the image.
	ImagePull Code = Prefix + "image_pull"
	// PVCPending: the instance disk did not bind.
	PVCPending Code = Prefix + "pvc_pending"
	// Unschedulable: no node takes the instance pod.
	Unschedulable Code = Prefix + "unschedulable"
	// CrashLoop: the instance keeps exiting.
	CrashLoop Code = Prefix + "crash_loop"
	// OOMKilled: the instance keeps running out of memory.
	OOMKilled Code = Prefix + "oomkilled"
	// Timeout: a wait on the cluster ran out (the seed, the pod's start, the
	// agent's handshake).
	Timeout Code = Prefix + "timeout"
	// ClusterMissing: the instance's connection was removed.
	ClusterMissing Code = Prefix + "cluster_missing"
	// Interrupted: the daemon stopped in the middle of the instance's job.
	Interrupted Code = Prefix + "interrupted"
	// RebuildFailed: a rebuild failed and was rolled back.
	RebuildFailed Code = Prefix + "rebuild_failed"
	// Tampered: the instance pod is not the one fleet made.
	Tampered Code = Prefix + "tampered"
	// OwnerConflict: another daemon holds the connection's owner Lease.
	OwnerConflict Code = Prefix + "owner_conflict"
	// Unavailable: the backend cannot do it yet (a part of the Kubernetes
	// backend that has not landed, or a build without it).
	Unavailable Code = Prefix + "unavailable"
)

// classes is every class, in Classes' order.
var classes = []Code{
	Unreachable, Unauthorized, Forbidden, AdmissionDenied, Quota, Unsupported, Misconfigured,
	ImageBuild, ImagePush, ImagePull, PVCPending, Unschedulable, CrashLoop, OOMKilled, Timeout,
	ClusterMissing, Interrupted, RebuildFailed, Tampered, OwnerConflict, Unavailable,
}

// Classes lists every class.
func Classes() []Code { return append([]Code(nil), classes...) }

// Valid reports whether c starts with one of the classes (narrowed or not).
func (c Code) Valid() bool {
	_, ok := c.class()
	return ok
}

// Class is the code's class: kubernetes:unsupported for
// kubernetes:unsupported:flag:--link. An unknown code is its own class.
func (c Code) Class() Code {
	if k, ok := c.class(); ok {
		return k
	}
	return c
}

func (c Code) class() (Code, bool) {
	s := string(c)
	if !strings.HasPrefix(s, Prefix) {
		return "", false
	}
	name, _, _ := strings.Cut(s[len(Prefix):], ":")
	k := Code(Prefix + name)
	for _, known := range classes {
		if known == k {
			return k, true
		}
	}
	return "", false
}

// Is reports whether c is class (or a narrowing of it).
func (c Code) Is(class Code) bool { return c.Class() == class.Class() }

// Narrow is c with one more detail: Unsupported.Narrow("compose").
func (c Code) Narrow(detail string) Code { return c + ":" + Code(detail) }

// Error is a coded error the backend made. (Errors of the backend's other
// packages carry their code through a Code() string method instead.)
type Error struct {
	Code Code
	Err  error
	// Hint says what to do about it ("" when the message already does).
	Hint string
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	return msg
}

// Unwrap is the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// New is an error with code and a formatted message.
func New(code Code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// Wrap gives err code, unless it carries a code already (Of): the most
// specific code wins, so a deadline's kubernetes:pvc_pending is never
// re-labelled by the step that waited for it. A nil err is nil.
func Wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := Of(err); ok {
		return err
	}
	return &Error{Code: code, Err: err}
}

// WithHint is Wrap with a hint.
func WithHint(code Code, err error, hint string) error {
	if err == nil {
		return nil
	}
	if _, ok := Of(err); ok {
		return err
	}
	return &Error{Code: code, Err: err, Hint: hint}
}

// Of is err's code: the first error in its chain that knows its code (an
// Error, or any error with a Code() method returning a kubernetes: code) or
// whose message starts with one. In a join (errors.Join, several %w) the
// first coded error leads, as the backend's joined refusals do. ok is false
// for an error without a code.
func Of(err error) (Code, bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		switch x := e.(type) {
		case *Error:
			return x.Code, true
		case interface{ Code() string }:
			if c := Code(x.Code()); c.Valid() {
				return c, true
			}
		}
		if c, _, ok := Parse(e.Error()); ok {
			return c, true
		}
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range multi.Unwrap() {
				if c, ok := Of(inner); ok {
					return c, true
				}
			}
			return "", false
		}
	}
	return "", false
}

// Parse reads the code a message starts with: "kubernetes:<class>[:<detail>]:
// <text>" (its first line). The code is everything before the first ": "
// after the prefix; detail is the rest of the first line. ok is false when
// the message does not start with a known class.
func Parse(msg string) (code Code, detail string, ok bool) {
	line, _, _ := strings.Cut(msg, "\n")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, Prefix) {
		return "", "", false
	}
	head, rest, found := strings.Cut(line, ": ")
	if !found {
		head = line
	}
	c := Code(head)
	if !c.Valid() {
		return "", "", false
	}
	return c, rest, true
}

// hints are what to do about each class, for a recovery dialog that has
// nothing better to say.
var hints = map[Code]string{
	Unreachable:     "check that the cluster is up and reachable from the daemon host, then try again",
	Unauthorized:    "log in to the cluster again (Settings → Kubernetes → login on fleetd)",
	Forbidden:       "give the connection's identity the permissions `fleet cluster test` lists",
	AdmissionDenied: "use a namespace whose admission allows the pod, or change the devcontainer",
	Quota:           "free room in the namespace's quota or the connection's caps, or raise them",
	Unsupported:     "change the devcontainer, or run this instance on the docker backend",
	Misconfigured:   "fix the connection's settings in Settings → Kubernetes",
	ImageBuild:      "fix the devcontainer build; the creation log has the build's output",
	ImagePush:       "check the image delivery settings and the daemon host's registry login",
	ImagePull:       "check that the nodes can reach the image (registry, gateway, pull secret)",
	PVCPending:      "check the storage class; `kubectl describe pvc` says why the disk does not bind",
	Unschedulable:   "free or add a node that fits the instance, or lower its resources",
	CrashLoop:       "the instance keeps exiting; read `fleet logs`, fix it, then start it again",
	OOMKilled:       "raise memory_limit in Settings → Kubernetes, then start the instance again",
	Timeout:         "the wait's FLEET_KUBE_*_TIMEOUT can be raised if the cluster is just slow",
	ClusterMissing:  "reconnect the cluster in Settings → Kubernetes, or destroy the instance",
	Interrupted:     "destroy the instance (d) and create it again",
	RebuildFailed:   "the previous generation was restored; fix the devcontainer and rebuild again",
	Tampered:        "someone changed the instance's pod or StatefulSet; destroy the instance and create it again",
	OwnerConflict:   "another daemon owns this connection; stop it, or wait for its Lease to expire",
	Unavailable:     "this version of fleet cannot do that on Kubernetes yet",
}

// Hint is what to do about code's class.
func Hint(code Code) string { return hints[code.Class()] }
