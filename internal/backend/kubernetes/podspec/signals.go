package podspec

import (
	"fmt"
	"strconv"
	"strings"
)

// linuxSignals are Linux's signal numbers by name (without SIG), with the
// aliases docker's signal parser takes. The table is Linux's whatever the
// daemon host runs, since the signal is sent in the pod.
var linuxSignals = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6,
	"BUS": 7, "FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12,
	"PIPE": 13, "ALRM": 14, "TERM": 15, "STKFLT": 16, "CHLD": 17, "CLD": 17,
	"CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23,
	"XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29,
	"POLL": 29, "PWR": 30, "SYS": 31, "UNUSED": 31,
}

// The real-time signal range on Linux (glibc's SIGRTMIN, the kernel's
// SIGRTMAX).
const (
	sigRTMin = 34
	sigRTMax = 64
)

// canonicalSignal reads a signal as docker does ("SIGTERM", "TERM", "15",
// "SIGRTMIN+3") and returns its canonical name: SIG plus the primary name, or
// SIGRTMIN+n for a real-time signal.
func canonicalSignal(s string) (string, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return signalName(n)
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if n, ok := linuxSignals[name]; ok {
		return signalName(n)
	}
	for _, rt := range []struct {
		prefix string
		base   int
		sign   int
	}{{"RTMIN+", sigRTMin, 1}, {"RTMAX-", sigRTMax, -1}} {
		if off, ok := strings.CutPrefix(name, rt.prefix); ok {
			if k, err := strconv.Atoi(off); err == nil && k >= 0 {
				return signalName(rt.base + rt.sign*k)
			}
		}
	}
	switch name {
	case "RTMIN":
		return signalName(sigRTMin)
	case "RTMAX":
		return signalName(sigRTMax)
	}
	return "", fmt.Errorf("invalid signal: %s", s)
}

// signalName is the canonical name of signal number n.
func signalName(n int) (string, error) {
	if n >= sigRTMin && n <= sigRTMax {
		if n == sigRTMin {
			return "SIGRTMIN", nil
		}
		return "SIGRTMIN+" + strconv.Itoa(n-sigRTMin), nil
	}
	best := ""
	for name, num := range linuxSignals {
		// The primary name sorts after its alias in no particular order, so
		// prefer the one that is not an alias.
		if num == n && (best == "" || signalAlias[best]) {
			best = name
		}
	}
	if best == "" {
		return "", fmt.Errorf("invalid signal: %d", n)
	}
	return "SIG" + best, nil
}

// signalAlias marks the alias names in linuxSignals.
var signalAlias = map[string]bool{"IOT": true, "CLD": true, "POLL": true, "UNUSED": true}
