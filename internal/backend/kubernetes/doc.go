// Package kubernetes is fleet's Kubernetes backend: instances run as pods on
// the daemon's cluster connections (internal/fleet.KubeCluster).
//
// The Pool is fleetd's side of every connection and of every pod it talks
// to. It keeps one pinned client per connection (kubeexec.Load, reloaded
// when the record or its kubeconfig changes), the credential issuer the
// daemon-host kube-exec shim gets its transport from (IssueExecCredential),
// and one long-lived agent session per running pod: a non-TTY exec of `fleet
// pod-agent serve` whose stdio carries a yamux session (internal/podwire),
// plus a bulk session opened on demand for large transfers and closed once
// idle, so polls, scripts, PCM and small files never cost an API server
// exec upgrade of their own.
//
// Before any exec, a pod is verified (VerifyPod): it must run the image
// fleetd delivered, have exactly the spec fleetd rendered (a hash fleetd
// recomputes from its own render, never the pod's annotation) and be owned
// by the StatefulSet fleetd created. A substituted pod gets nothing:
// kubernetes:tampered. A verified pod is then bootstrapped (EnsureAgent):
//
//  1. a probe exec (arch, uid, tools, the pod's fleet build), skipped on the
//     fast path when the same pod (UID) was probed before;
//  2. a stage exec only when the pod's fleet is missing or speaks no
//     protocol fleetd does, or lazily when the caller asks for a restage (the
//     daemon's build is then staged at the moments docker's EnsureFresh would
//     restage: a start, a mic or output open, an idle pod); the pod build
//     comes from the BinStore (internal/backend/kubernetes/binstore) and is
//     content-addressed in the pod, the previous one kept for pod-init's
//     fallback;
//  3. the agent exec, whose handshake checks the agent reports the verified
//     pod's UID.
//
// Bootstraps are limited per connection (at most 4 at once, interactive
// pods first) and a pod that fails one waits a full-jitter backoff before
// the next, so a daemon restart or an API server rollout reconnects every pod
// without a storm. While a pod has no session, FallbackExec runs commands
// through a rate-limited kube exec of `fleet exec-as` (the request framed on
// stdin, never in argv), and RawScript through `sh` alone when the pod has no
// fleet binary.
//
// Jobs are durable. A create, clone or rebuild writes an intent journal
// (journal.go) before each API server write and mirrors its step on its
// anchor object, so a daemon that dies mid-job leaves a record the next one's
// reconciler fails or rolls back (UndoRebuild, UndoClone), never resumes; a
// journal the live objects no longer match is never acted on. A destroy
// tombstones the instance's key (footprint.go) before DownByKey deletes
// every object of it, so what a teardown misses is known garbage. Every
// deletion goes through Scope, under the owner check (SetOwnerGate). Waits
// on the cluster have deadlines (deadlines.go, FLEET_KUBE_*_TIMEOUT) and fail
// naming what they were stuck on (WaitReady, RunJob, the agent handshake).
// RunJob drives the jobboot Jobs (seed, rollback, tool) to their result.
//
// ForInstance is the backend handle of one Kubernetes instance (what
// backendutil.NewForInstance returns for one): bound to the instance's
// connection, key, fleet and creation time, which name every object of it,
// and to fleetd's record of what it rendered and delivered
// (instance_record.go), which the instance's pod is verified against. Its
// UpContext is the create (create.go): capture-up on the daemon host, image
// delivery, admission, the seed, the StatefulSet, the agent and the
// devcontainer's lifecycle hooks, every API server write journaled. Stop and
// Start scale the StatefulSet (the disk keeps the instance), Down and
// DownByKey delete every object of the key (lifecycle.go). The daemon hands
// the handles its image delivery and each connection's preflight findings
// (SetEnvironment).
//
// Everything that needs client-go builds only in the full binary (!nok8s);
// this file and the pure ones beside it (journal, footprint, deadlines, the
// instance record) build in the pod build too, so the package always
// exists, and ForInstance there answers that the build has no Kubernetes
// backend.
package kubernetes
