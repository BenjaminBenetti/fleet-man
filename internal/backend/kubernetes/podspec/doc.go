// Package podspec turns the `docker run` the devcontainer CLI asked for into
// the pod an instance runs as on Kubernetes.
//
// capture-up (devcli.CaptureUp) leaves a kubeshim.Capture: the run's argv
// with every value the CLI resolved (features, the start script, labels,
// mounts, runArgs). Translate reads it with the image it names, the merged
// configuration and the cluster connection, and produces a PodSpec: a pure,
// renderer-independent model of the instance pod (its security context,
// resources, mounts classified by where their data lives, ports, host
// aliases, DNS, the PID 1 spec pod-init runs) plus the contents of the two
// Secrets the pod reads, the spec files and the environment values. The
// StatefulSet, Job and PVC renderers build the Kubernetes objects from it.
//
// Every docker run option is in one table (Options), keyed by the canonical
// names kubeshim parses (kubeshim.RunFlagNames), and has exactly one
// outcome there:
//
//   - Map: translated onto the pod, sometimes with a warning where the
//     cluster cannot do exactly what docker does;
//   - Delta: an accepted difference from docker, always with a visible
//     warning;
//   - Ignore: an option with no effect on either side (CLI plumbing such as
//     --sig-proxy, or an option docker itself ignores on Linux);
//   - Refuse: an option a pod cannot honour; the create fails with
//     kubernetes:unsupported:flag:<option> and the reason.
//
// Nothing a run asks for is lost silently. Translate is pure apart from the
// host stat of bind sources, which goes through Input.Host.
//
// Two policies are applied here. Privileged pods follow the connection's
// "Allow privileged pods" (auto: only on a local cluster); a sysbox runtime
// class serves --privileged instead when the cluster offers one. Debug
// grants (SYS_PTRACE, unconfined seccomp or AppArmor) are requested, and the
// create drops them with a warning when admission denies only them
// (debug_capabilities auto) or fails (require): see Denied.
//
// Render turns a translation into one instance generation's objects: the
// instance disk (a PVC), the spec and env Secrets, the StatefulSet (its pod
// running pod-init as root over the disk's persisted root, or as one uid on
// a restricted cluster: Identity), an optional PodDisruptionBudget and the
// seed Job; RenderJob renders the instance's other Jobs. Every workload goes
// through Harden (no service account token or service links, seccomp
// RuntimeDefault, no service-mesh sidecar, OnRootMismatch, one SELinux level
// per share-group). SpecHash is the hash pod verification recomputes. The
// renderers are pure and build only in the full binary (!nok8s), as do
// DryRun, which has the API server admit a render without creating it, and
// Admit, which translates, renders and dry-runs with the debug-capabilities
// policy applied. ClassifyFailedCreate names the same refusals when a
// controller records them after an apply.
package podspec
