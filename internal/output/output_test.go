package output

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMixerClipsAndPreservesStereo(t *testing.T) {
	pcm := func(left, right int16) []byte {
		out := make([]byte, ChunkBytes)
		for i := 0; i < len(out); i += 4 {
			binary.LittleEndian.PutUint16(out[i:], uint16(left))
			binary.LittleEndian.PutUint16(out[i+2:], uint16(right))
		}
		return out
	}
	m := mixer{}
	now := time.Unix(1000, 0)
	later := now.Add(Prebuffer)
	m.push("one", pcm(20000, -20000), now)
	m.push("two", pcm(20000, -20000), now)
	if got := m.next(later); !bytes.Equal(got, pcm(32767, -32768)) {
		t.Fatal("mixed samples overflowed or lost stereo alignment")
	}
	if m.next(later) != nil {
		t.Fatal("finished audio repeated")
	}
	m.push("one", pcm(100, 200), later)
	m.push("two", pcm(-10, -20), later)
	if !bytes.Equal(m.next(later.Add(Prebuffer)), pcm(90, 180)) {
		t.Fatal("instances were not mixed")
	}
}

// The jitter buffer: a stream starts once Prebuffer of it arrived, or its
// first frame waited Prebuffer, or its source ended; an underrun primes it
// again. Frames arriving a little apart then play without gaps.
func TestMixerPrebuffersEachStream(t *testing.T) {
	chunk := bytes.Repeat([]byte{1, 0, 1, 0}, ChunkBytes/4)
	now := time.Unix(1000, 0)
	m := mixer{}
	m.push("one", chunk, now)
	if m.next(now.Add(20*time.Millisecond)) != nil {
		t.Fatal("played before the jitter buffer filled")
	}
	for i := 1; i < 4; i++ { // Prebuffer is 4 frames
		m.push("one", chunk, now.Add(time.Duration(i)*5*time.Millisecond))
	}
	at := now.Add(30 * time.Millisecond)
	for i := range 4 {
		if m.next(at) == nil {
			t.Fatalf("a full jitter buffer did not play frame %d", i)
		}
	}
	if m.next(at) != nil {
		t.Fatal("played past the end of the stream")
	}

	// Underrun: the next frame waits again — or plays once it waited Prebuffer.
	m.push("one", chunk, at)
	if m.next(at.Add(Prebuffer/2)) != nil {
		t.Fatal("played at once after an underrun")
	}
	if m.next(at.Add(Prebuffer)) == nil {
		t.Fatal("a frame that waited Prebuffer did not play")
	}

	// A short sound whose source ended plays out at once.
	m.push("two", chunk, at)
	m.end("two")
	if m.next(at) == nil {
		t.Fatal("an ended stream waited for its jitter buffer")
	}
	m.end("gone") // ending a stream with nothing queued does nothing
	if len(m) != 0 {
		t.Fatalf("streams left: %d", len(m))
	}
}

func TestSilentAllowsDither(t *testing.T) {
	frame := func(v int16) []byte {
		out := make([]byte, ChunkBytes)
		for i := 0; i < len(out); i += 2 {
			binary.LittleEndian.PutUint16(out[i:], uint16(v))
		}
		return out
	}
	for _, tc := range []struct {
		v    int16
		want bool
	}{{0, true}, {SilenceThreshold, true}, {-SilenceThreshold, true}, {SilenceThreshold + 1, false}, {-SilenceThreshold - 1, false}, {20000, false}} {
		if got := Silent(frame(tc.v)); got != tc.want {
			t.Errorf("Silent(all %d) = %v, want %v", tc.v, got, tc.want)
		}
	}
	loud := frame(0)
	quiet := int16(-300)
	binary.LittleEndian.PutUint16(loud[len(loud)-2:], uint16(quiet))
	if Silent(loud) {
		t.Error("one loud sample is not silence")
	}
}
func TestMixerBoundsBacklogAndStreams(t *testing.T) {
	m := mixer{}
	now := time.Unix(1000, 0)
	m.push("one", bytes.Repeat([]byte{1, 2, 3, 4}, ChunkBytes*20), now)
	if len(m["one"].pcm) != 10*ChunkBytes {
		t.Fatal("unbounded backlog")
	}
	m.push("one", []byte{9, 9, 9}, now)
	if len(m["one"].pcm) != 10*ChunkBytes {
		t.Fatal("accepted a partial stereo sample")
	}
	for i := 0; i < MaxStreams+10; i++ {
		m.push(string(rune(i+1)), []byte{1, 2, 3, 4}, now)
	}
	if len(m) > MaxStreams {
		t.Fatal("unbounded stream count")
	}
}
func TestPlaybackDeviceValidation(t *testing.T) {
	list := []Device{{ID: "pulse:headset", Label: "Headset"}}
	argv, fallback := playbackArgs("pulse", "pulse:headset", list)
	if fallback || !strings.Contains(strings.Join(argv, " "), "--device=headset") {
		t.Fatalf("selected output ignored: %v", argv)
	}
	for _, bad := range []string{"pulse:gone", "--server=remote", "alsa:headset"} {
		argv, fallback = playbackArgs("pulse", bad, list)
		if !fallback || strings.Contains(strings.Join(argv, " "), "--device=") {
			t.Fatalf("unlisted device reached player: %v", argv)
		}
	}
}
func TestOutputEnumerationExcludesFleetDevices(t *testing.T) {
	list, virtual := pulseDevices("Sink #1\n Name: fleetoutput\n Description: FleetAudioOutput\nSink #2\n Name: usb\n Description: USB Speakers\nSink #3\n Name: fleetnull\n")
	if !virtual || len(list) != 1 || list[0].ID != "pulse:usb" || list[0].Label != "USB Speakers" {
		t.Fatalf("outputs: %+v virtual=%v", list, virtual)
	}
	alsa := alsaDevices("null\n    Discard all samples\npulse\nplughw:CARD=USB,DEV=0\n    USB\n")
	if len(alsa) != 1 || alsa[0].ID != "alsa:plughw:CARD=USB,DEV=0" {
		t.Fatalf("ALSA: %+v", alsa)
	}
}

func TestCoreAudioEnumerationOnlyOffersPlaybackDevices(t *testing.T) {
	var tree any
	if err := json.Unmarshal([]byte(`{"SPAudioDataType":[{"_items":[{"_name":"Speakers","coreaudio_device_output":2},{"_name":"Microphone","coreaudio_device_input":1},{"_name":"Headset","coreaudio_device_input":1,"coreaudio_device_output":2}]}]}`), &tree); err != nil {
		t.Fatal(err)
	}
	list := coreAudioDevices(tree)
	if len(list) != 2 || list[0].ID != "coreaudio:Speakers" || list[1].ID != "coreaudio:Headset" {
		t.Fatalf("CoreAudio outputs: %+v", list)
	}
}
