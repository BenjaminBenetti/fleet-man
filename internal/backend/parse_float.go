package backend

import (
	"strconv"
	"strings"
)

// ParseFloat parses a trimmed string into a float64.
func ParseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
