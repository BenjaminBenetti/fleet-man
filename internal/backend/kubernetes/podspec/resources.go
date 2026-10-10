package podspec

import (
	"encoding/csv"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// resourceAsks is what the run's resource options asked for, in base units
// (bytes, millicores, microseconds); zero is not asked.
type resourceAsks struct {
	memLimit       int64
	memReservation int64
	cpuLimit       int64
	cpuQuota       int64
	cpuPeriod      int64
	gpus           int64
}

func (t *translation) optMemory(vals []string) Outcome {
	n, err := dockerBytes(last(vals))
	if err != nil || n <= 0 {
		t.refuse(refuseOption("memory", fmt.Sprintf("invalid memory limit %q", last(vals))))
		return ""
	}
	t.res.memLimit = n
	return ""
}

func (t *translation) optMemoryReservation(vals []string) Outcome {
	n, err := dockerBytes(last(vals))
	if err != nil || n <= 0 {
		t.refuse(refuseOption("memory-reservation", fmt.Sprintf("invalid memory reservation %q", last(vals))))
		return ""
	}
	t.res.memReservation = n
	return ""
}

func (t *translation) optCPUs(vals []string) Outcome {
	m, err := dockerCPUMillis(last(vals))
	if err != nil || m <= 0 {
		t.refuse(refuseOption("cpus", fmt.Sprintf("invalid CPU count %q", last(vals))))
		return ""
	}
	if _, quota := t.run.Value("cpu-quota"); quota {
		t.refuse(refuseOption("cpus", "--cpus and --cpu-quota cannot both be set"))
		return ""
	}
	t.res.cpuLimit = m
	return ""
}

func (t *translation) optCPUPeriod(vals []string) Outcome {
	n, err := strconv.ParseInt(last(vals), 10, 64)
	if err != nil || n < 1000 || n > 1000000 {
		t.refuse(refuseOption("cpu-period", fmt.Sprintf("invalid CPU period %q: between 1000 and 1000000 microseconds", last(vals))))
		return ""
	}
	t.res.cpuPeriod = n
	if _, quota := t.run.Value("cpu-quota"); !quota {
		// A period alone changes nothing on docker either.
		return Ignore
	}
	return ""
}

func (t *translation) optCPUQuota(vals []string) Outcome {
	n, err := strconv.ParseInt(last(vals), 10, 64)
	if err != nil || (n != -1 && n < 1000) {
		t.refuse(refuseOption("cpu-quota", fmt.Sprintf("invalid CPU quota %q: at least 1000 microseconds, or -1", last(vals))))
		return ""
	}
	if n == -1 {
		return Ignore
	}
	t.res.cpuQuota = n
	return ""
}

func (t *translation) optGPUs(vals []string) Outcome {
	v := last(vals)
	n, err := gpuCount(v)
	if err != nil {
		t.refuse(refuseOption("gpus", err.Error()))
		return ""
	}
	if t.in.Facts.GPUResource == "" {
		t.refuse(refuseOption("gpus", fmt.Sprintf("--gpus %s: %s offers no GPU resource on its nodes", v, t.clusterName())))
		return ""
	}
	if n < 0 {
		t.warn("--gpus %s: a pod asks for a number of GPUs; it gets one", v)
		n = 1
	}
	t.res.gpus = n
	return ""
}

// gpuCount reads docker's --gpus: "all", a count, or a list of options
// (count=N, device=a,b, driver=..., capabilities=...). -1 is all.
func gpuCount(v string) (int64, error) {
	if v == "all" {
		return -1, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("invalid GPU count %q", v)
		}
		return n, nil
	}
	fields, err := csv.NewReader(strings.NewReader(v)).Read()
	if err != nil {
		return 0, fmt.Errorf("invalid --gpus %q: %v", v, err)
	}
	count := int64(-1)
	for _, field := range fields {
		key, val, _ := strings.Cut(field, "=")
		switch key {
		case "count":
			if val == "all" {
				count = -1
				continue
			}
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid GPU count %q", val)
			}
			count = n
		case "device":
			count = int64(len(strings.Split(val, ",")))
		case "driver", "capabilities", "options":
		default:
			return 0, fmt.Errorf("invalid --gpus %q", v)
		}
	}
	return count, nil
}

// finishResources decides the dev container's requests and limits and the
// instance disk:
//
//   - requests are the connection's (default 500m CPU, 2Gi), raised to
//     hostRequirements (clamped to the largest node) and to
//     --memory-reservation;
//   - -m and --cpus (or --cpu-quota) are the limits;
//   - without -m, a namespace LimitRange that would give the container a
//     default memory limit below the connection's memory_limit (default
//     8Gi) gets an explicit memory_limit instead;
//   - on GKE Autopilot limits equal requests, and the memory request is
//     raised to the connection's autopilot_memory (default 4Gi);
//   - the disk is disk_size (default 50Gi), raised to
//     hostRequirements.storage.
func (t *translation) finishResources() {
	cl := t.cluster
	cpuReq, ok := t.setting("resources.cpu_request", cl.Resources.CPURequest, fleet.DefaultKubeCPURequest, quantityMillis)
	if !ok {
		return
	}
	memReq, ok := t.setting("resources.memory_request", cl.Resources.MemoryRequest, fleet.DefaultKubeMemoryRequest, quantityBytes)
	if !ok {
		return
	}
	disk, ok := t.setting("disk_size", cl.DiskSize, fleet.DefaultKubeDiskSize, quantityBytes)
	if !ok {
		return
	}
	maxCPU, maxMem := t.largestNode()

	if hr := t.merged.HostRequirements; hr != nil {
		if hr.CPUs > 0 {
			want := int64(math.Ceil(hr.CPUs * 1000))
			if maxCPU > 0 && want > maxCPU {
				t.warn("hostRequirements.cpus %s: the largest node has %s allocatable; the pod requests that", strconv.FormatFloat(hr.CPUs, 'f', -1, 64), formatMillis(maxCPU))
				want = maxCPU
			}
			cpuReq = max(cpuReq, want)
		}
		if want, err := hr.size(hr.Memory); err != nil {
			t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("hostRequirements.memory: %v", err)})
		} else if want > 0 {
			if maxMem > 0 && want > maxMem {
				t.warn("hostRequirements.memory %s: the largest node has %s allocatable; the pod requests that", formatBytes(want), formatBytes(maxMem))
				want = maxMem
			}
			memReq = max(memReq, want)
		}
		if want, err := hr.size(hr.Storage); err != nil {
			t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("hostRequirements.storage: %v", err)})
		} else {
			disk = max(disk, want)
		}
		if hr.wantsGPU() && t.res.gpus == 0 {
			t.warn("hostRequirements.gpu: the devcontainer needs a GPU, and the pod gets none (%s offers no GPU resource)", t.clusterName())
		}
	}
	memReq = max(memReq, t.res.memReservation)

	memLimit := t.res.memLimit
	cpuLimit := t.res.cpuLimit
	if t.res.cpuQuota > 0 {
		period := t.res.cpuPeriod
		if period == 0 {
			period = 100000
		}
		cpuLimit = (t.res.cpuQuota*1000 + period - 1) / period
	}
	if t.res.memReservation > 0 && memLimit > 0 && t.res.memReservation > memLimit {
		t.refuse(refuseOption("memory-reservation", "the memory reservation is larger than the memory limit (-m)"))
		return
	}

	if t.in.Facts.Autopilot {
		autopilot, ok := t.setting("resources.autopilot_memory", cl.Resources.AutopilotMemory, fleet.DefaultKubeAutopilotMemory, quantityBytes)
		if !ok {
			return
		}
		switch {
		case memLimit > 0:
			memReq = memLimit
		default:
			memReq = max(memReq, autopilot)
		}
		if cpuLimit > 0 {
			cpuReq = cpuLimit
		}
		memLimit, cpuLimit = memReq, cpuReq
		t.warn("GKE Autopilot: limits equal requests (%s CPU, %s memory); a process past the memory is OOM-killed, which restarts the pod", formatMillis(cpuReq), formatBytes(memReq))
	} else if memLimit == 0 && t.in.Facts.LimitRangeMemoryLimit != "" {
		injected, err := quantityBytes(t.in.Facts.LimitRangeMemoryLimit)
		want, ok := t.setting("memory_limit", cl.MemoryLimit, fleet.DefaultKubeMemoryLimit, quantityBytes)
		if !ok {
			return
		}
		want = max(want, memReq)
		if err == nil && injected < want {
			memLimit = want
			t.warn("LimitRange: default memory limit %s → fleet sets %s explicitly (memory_limit on %s)", formatBytes(injected), formatBytes(want), t.clusterName())
		}
	}

	if memLimit > 0 && memReq > memLimit {
		t.warn("-m %s: the memory request is lowered from %s to the limit", formatBytes(memLimit), formatBytes(memReq))
		memReq = memLimit
	}
	if cpuLimit > 0 && cpuReq > cpuLimit {
		opt := "cpus"
		if t.res.cpuQuota > 0 {
			opt = "cpu-quota"
		}
		v, _ := t.run.Value(opt)
		t.warn("%s: the CPU request is lowered from %s to the limit, %s", flagText(opt, v), formatMillis(cpuReq), formatMillis(cpuLimit))
		cpuReq = cpuLimit
	}

	r := &t.spec.Resources
	r.Requests = map[string]string{"cpu": formatMillis(cpuReq), "memory": formatBytes(memReq)}
	if memLimit > 0 || cpuLimit > 0 || t.res.gpus > 0 {
		r.Limits = map[string]string{}
	}
	if memLimit > 0 {
		r.Limits["memory"] = formatBytes(memLimit)
	}
	if cpuLimit > 0 {
		r.Limits["cpu"] = formatMillis(cpuLimit)
	}
	if t.res.gpus > 0 {
		r.Limits[t.in.Facts.GPUResource] = strconv.FormatInt(t.res.gpus, 10)
	}
	if disk == mustBytes(cl.DiskSize, fleet.DefaultKubeDiskSize) {
		t.spec.DiskSize = orDefault(cl.DiskSize, fleet.DefaultKubeDiskSize)
	} else {
		t.spec.DiskSize = formatBytes(disk)
	}
}

// setting reads a connection setting (or its default) with parse; a
// setting that does not parse is refused.
func (t *translation) setting(name, value, def string, parse func(string) (int64, error)) (int64, bool) {
	n, err := parse(orDefault(value, def))
	if err != nil {
		t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("%s's %s %q: %v", t.clusterName(), name, value, err)})
		return 0, false
	}
	return n, true
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// mustBytes reads a size setting that setting already read successfully.
func mustBytes(v, def string) int64 {
	n, _ := quantityBytes(orDefault(v, def))
	return n
}

// largestNode returns the most allocatable CPU (millicores) and memory
// (bytes) of the nodes that can run the image; 0 when unknown.
func (t *translation) largestNode() (cpu, mem int64) {
	arch := t.in.Image.Architecture
	for _, n := range t.in.Facts.Nodes {
		if arch != "" && n.Arch != "" && n.Arch != arch {
			continue
		}
		if c, err := quantityMillis(n.AllocatableCPU); err == nil {
			cpu = max(cpu, c)
		}
		if m, err := quantityBytes(n.AllocatableMemory); err == nil {
			mem = max(mem, m)
		}
	}
	return cpu, mem
}

func (t *translation) optPlatform(vals []string) Outcome {
	v := last(vals)
	parts := strings.Split(v, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" {
		t.refuse(refuseOption("platform", fmt.Sprintf("--platform %s: the nodes run linux/<arch>", v)))
		return ""
	}
	if a := t.in.Image.Architecture; a != "" && a != parts[1] {
		t.refuse(refuseOption("platform", fmt.Sprintf("--platform %s, but the image is linux/%s", v, a)))
		return ""
	}
	t.nodeSelector = map[string]string{archLabel: parts[1]}
	return ""
}

// The node labels a pod is pinned with.
const (
	archLabel = "kubernetes.io/arch"
	osLabel   = "kubernetes.io/os"
)

// finishNodeSelector pins the pod to linux nodes of the image's
// architecture, with the connection's node_selector; a node_selector that
// pins another architecture is refused.
func (t *translation) finishNodeSelector() {
	sel := map[string]string{osLabel: "linux"}
	if a := t.in.Image.Architecture; a != "" {
		sel[archLabel] = a
	}
	for k, v := range t.nodeSelector {
		sel[k] = v
	}
	for k, v := range t.cluster.NodeSelector {
		if have, ok := sel[k]; ok && have != v {
			t.refuse(&Refusal{Code: CodeSettings, Reason: fmt.Sprintf("%s's node_selector %s=%s conflicts with the image, which needs %s=%s", t.clusterName(), k, v, k, have)})
			continue
		}
		sel[k] = v
	}
	t.spec.NodeSelector = sel
}
