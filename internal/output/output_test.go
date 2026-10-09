package output

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
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
	m.push("one", pcm(20000, -20000))
	m.push("two", pcm(20000, -20000))
	if got := m.next(); !bytes.Equal(got, pcm(32767, -32768)) {
		t.Fatal("mixed samples overflowed or lost stereo alignment")
	}
	if m.next() != nil {
		t.Fatal("finished audio repeated")
	}
	m.push("one", pcm(100, 200))
	m.push("two", pcm(-10, -20))
	if !bytes.Equal(m.next(), pcm(90, 180)) {
		t.Fatal("instances were not mixed")
	}
}
func TestMixerBoundsBacklogAndStreams(t *testing.T) {
	m := mixer{}
	m.push("one", bytes.Repeat([]byte{1, 2, 3, 4}, ChunkBytes*20))
	if len(m["one"]) != 10*ChunkBytes {
		t.Fatal("unbounded backlog")
	}
	m.push("one", []byte{9, 9, 9})
	if len(m["one"]) != 10*ChunkBytes {
		t.Fatal("accepted a partial stereo sample")
	}
	for i := 0; i < MaxStreams+10; i++ {
		m.push(string(rune(i+1)), []byte{1, 2, 3, 4})
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
