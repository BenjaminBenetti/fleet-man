package output

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

var lookPath = exec.LookPath
var goos = runtime.GOOS
var probe = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// Detect and enumerate together so a client without working playback does not
// take Auto away from a client with speakers. Never offer fleet's own sink.
func devices() (string, []Device, error) {
	if goos == "darwin" {
		if _, err := lookPath("sox"); err != nil {
			return "", nil, fmt.Errorf("install SoX for audio output (brew install sox)")
		}
		out, err := probe("system_profiler", "-json", "SPAudioDataType")
		if err != nil {
			return "", nil, err
		}
		var tree any
		if err := json.Unmarshal(out, &tree); err != nil {
			return "", nil, err
		}
		list := mic.CleanDevices(coreAudioDevices(tree))
		if len(list) == 0 {
			return "", nil, fmt.Errorf("no CoreAudio output devices available")
		}
		return "coreaudio", list, nil
	}
	if _, err := lookPath("paplay"); err == nil {
		if out, err := probe("pactl", "list", "sinks"); err == nil {
			list, virtual := pulseDevices(string(out))
			if len(list) > 0 {
				return "pulse", mic.CleanDevices(list), nil
			}
			if virtual {
				return "", nil, fmt.Errorf("this client is inside a fleet instance; its virtual output cannot play to itself")
			}
		}
	}
	if _, err := lookPath("aplay"); err == nil {
		out, err := probe("aplay", "-L")
		if err == nil {
			list := alsaDevices(string(out))
			if len(list) > 0 {
				return "alsa", mic.CleanDevices(list), nil
			}
		}
	}
	return "", nil, fmt.Errorf("no audio output device; install pulseaudio-utils (PulseAudio/PipeWire) or alsa-utils")
}

func Devices() ([]Device, error) { _, list, err := devices(); return list, err }

func pulseDevices(text string) (list []Device, virtual bool) {
	var name, label string
	flush := func() {
		if name == VirtualSink || name == "fleetnull" {
			virtual = true
			return
		}
		if name != "" {
			if label == "" {
				label = name
			}
			list = append(list, Device{ID: "pulse:" + name, Label: label})
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Sink #") {
			flush()
			name, label = "", ""
		}
		if value, ok := strings.CutPrefix(line, "Name:"); ok {
			name = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "Description:"); ok {
			label = strings.TrimSpace(value)
		}
	}
	flush()
	return
}

func alsaDevices(text string) (list []Device) {
	for line := range strings.SplitSeq(text, "\n") {
		// Hardware PCMs support playback. Avoid the null and pulse plugins:
		// a broken Pulse server is not an independent ALSA playback device.
		if strings.HasPrefix(line, "plughw:") || strings.HasPrefix(line, "sysdefault:CARD=") {
			list = append(list, Device{ID: "alsa:" + line, Label: line})
		}
	}
	return
}

func coreAudioDevices(node any) (list []Device) {
	switch node := node.(type) {
	case []any:
		for _, child := range node {
			list = append(list, coreAudioDevices(child)...)
		}
	case map[string]any:
		channels, _ := node["coreaudio_device_output"].(float64)
		if name, ok := node["_name"].(string); ok && channels > 0 {
			list = append(list, Device{ID: "coreaudio:" + name, Label: name})
		}
		for _, child := range node {
			list = append(list, coreAudioDevices(child)...)
		}
	}
	return
}

// Device IDs are validated against this client's fresh enumeration before
// reaching any command. A removed headset falls back to the system default.
func playbackArgs(tool, device string, list []Device) (argv []string, fallback bool) {
	native := ""
	for _, d := range list {
		if d.ID == device {
			_, native, _ = strings.Cut(d.ID, ":")
			break
		}
	}
	fallback = device != "" && native == ""
	switch tool {
	case "pulse":
		argv = []string{"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=40", "--client-name=fleet"}
		if native != "" {
			argv = append(argv, "--device="+native)
		}
	case "alsa":
		argv = []string{"aplay", "-q", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "2"}
		if native != "" {
			argv = append(argv, "-D", native)
		}
	case "coreaudio":
		if native == "" {
			native = "default"
		}
		argv = []string{"sox", "-q", "-t", "raw", "-r", "48000", "-e", "signed", "-b", "16", "-c", "2", "-L", "-", "-t", "coreaudio", native}
	}
	return
}
