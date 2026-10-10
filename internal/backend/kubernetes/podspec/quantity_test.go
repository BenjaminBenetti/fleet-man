package podspec

import "testing"

func TestQuantityBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"2Gi", 2 << 30, false},
		{"512Mi", 512 << 20, false},
		{"1.5Gi", 3 << 29, false},
		{"8G", 8e9, false},
		{"1e3", 1000, false},
		{"1E3", 1000, false},
		{"1E", 1e18, false},
		{"100", 100, false},
		{"0.5", 1, false}, // rounded up
		{"100m", 1, false},
		{" 4Ki ", 4096, false},
		{"", 0, true},
		{"Gi", 0, true},
		{"2GB", 0, true},
		{"-1Gi", 0, true},
		{"1e40", 0, true},
	}
	for _, tt := range tests {
		got, err := quantityBytes(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("quantityBytes(%q) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestQuantityMillis(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"500m", 500}, {"2", 2000}, {"1.5", 1500}, {"7910m", 7910}, {"0.0001", 1}, {"1k", 1000000},
	}
	for _, tt := range tests {
		if got, err := quantityMillis(tt.in); err != nil || got != tt.want {
			t.Errorf("quantityMillis(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
}

func TestFormatQuantities(t *testing.T) {
	for n, want := range map[int64]string{
		2 << 30: "2Gi", 3 << 29: "1536Mi", 4096: "4Ki", 1000: "1000", 0: "0", 1 << 40: "1Ti",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %s, want %s", n, got, want)
		}
	}
	for m, want := range map[int64]string{500: "500m", 2000: "2", 1500: "1500m", 0: "0"} {
		if got := formatMillis(m); got != want {
			t.Errorf("formatMillis(%d) = %s, want %s", m, got, want)
		}
	}
}

// Docker's sizes are 1024-based whatever the suffix says, as go-units reads
// them.
func TestDockerBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"4g", 4 << 30, false},
		{"4G", 4 << 30, false},
		{"4gb", 4 << 30, false},
		{"4GiB", 4 << 30, false},
		{"256m", 256 << 20, false},
		{"1.5g", 3 << 29, false},
		{"64 k", 64 << 10, false},
		{"100", 100, false},
		{"100b", 100, false},
		{"lots", 0, true},
		{"-1g", 0, true},
		{"4x", 0, true},
	}
	for _, tt := range tests {
		got, err := dockerBytes(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("dockerBytes(%q) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestDockerCPUMillis(t *testing.T) {
	for in, want := range map[string]int64{"1.5": 1500, "2": 2000, "0.25": 250, "0.0005": 1} {
		if got, err := dockerCPUMillis(in); err != nil || got != want {
			t.Errorf("dockerCPUMillis(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "two", "-1"} {
		if _, err := dockerCPUMillis(bad); err == nil {
			t.Errorf("dockerCPUMillis(%q) did not fail", bad)
		}
	}
}

// The CLI writes merged hostRequirements sizes as byte counts;
// devcontainer.json spells them with tb/gb/mb/kb.
func TestDevcontainerBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"4294967296", 4 << 30, false},
		{"4gb", 4 << 30, false},
		{"32tb", 32 << 40, false},
		{"512mb", 512 << 20, false},
		{"8kb", 8 << 10, false},
		{"4GB", 0, true},
		{"4g", 0, true},
		{"99999999999tb", 0, true},
	}
	for _, tt := range tests {
		got, err := devcontainerBytes(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("devcontainerBytes(%q) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestCanonicalSignal(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"SIGTERM", "SIGTERM"}, {"term", "SIGTERM"}, {"15", "SIGTERM"},
		{"SIGIOT", "SIGABRT"}, {"6", "SIGABRT"}, {"CLD", "SIGCHLD"}, {"29", "SIGIO"},
		{"SIGRTMIN", "SIGRTMIN"}, {"RTMIN+3", "SIGRTMIN+3"}, {"37", "SIGRTMIN+3"},
		{"SIGRTMAX", "SIGRTMIN+30"}, {"SIGRTMAX-2", "SIGRTMIN+28"}, {"usr1", "SIGUSR1"},
	}
	for _, tt := range tests {
		if got, err := canonicalSignal(tt.in); err != nil || got != tt.want {
			t.Errorf("canonicalSignal(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"SIGNOPE", "0", "32", "65", "RTMIN+31", "RTMIN+-1", ""} {
		if got, err := canonicalSignal(bad); err == nil {
			t.Errorf("canonicalSignal(%q) = %q, want an error", bad, got)
		}
	}
}
