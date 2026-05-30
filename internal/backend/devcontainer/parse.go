package devcontainer

import (
	"strconv"
	"strings"
)

func parseMemToMB(memText string) float64 {
	memText = strings.TrimSpace(memText)
	multiplier := 1.0

	if strings.HasSuffix(memText, "GiB") {
		multiplier = 1024
		memText = strings.TrimSuffix(memText, "GiB")
	} else if strings.HasSuffix(memText, "MiB") {
		multiplier = 1
		memText = strings.TrimSuffix(memText, "MiB")
	} else if strings.HasSuffix(memText, "KiB") {
		multiplier = 1.0 / 1024
		memText = strings.TrimSuffix(memText, "KiB")
	} else if strings.HasSuffix(memText, "B") {
		multiplier = 1.0 / (1024 * 1024)
		memText = strings.TrimSuffix(memText, "B")
	}

	value, _ := strconv.ParseFloat(strings.TrimSpace(memText), 64)
	return value * multiplier
}
