package backend

import "strings"

// AggregatePSStats parses the output of `ps -eo pcpu=,rss= --no-headers`
// and returns the aggregated container resource usage. Each non-empty
// line is expected to contain a CPU percentage and an RSS value (in KiB);
// lines with fewer than two fields are skipped. Per-process CPU
// percentages are summed and converted to millicores (x10), and RSS
// totals are converted from KiB to MiB (/1024).
func AggregatePSStats(out []byte) *ContainerStats {
	var totalCPU, totalMem float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		cpu, _ := ParseFloat(fields[0])
		mem, _ := ParseFloat(fields[1])
		totalCPU += cpu
		totalMem += mem
	}

	return &ContainerStats{
		CPUMillicores: totalCPU * 10, // convert percentage to millicores
		MemoryMB:      totalMem / 1024,
	}
}
