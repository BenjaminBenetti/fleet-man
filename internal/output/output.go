// Package output plays instance audio on a connected client. It has no access
// to daemon state or backends, including when the daemon happens to be local.
package output

import (
	"encoding/binary"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

const (
	SampleRate  = 48000
	Channels    = 2
	ChunkBytes  = SampleRate * Channels * 2 / 50 // 20 ms, s16le stereo
	VirtualSink = "fleetoutput"
	MaxStreams  = 128
)

type Device = mic.Device

// SilenceThreshold is the loudest sample a frame may hold and still count as
// silence: about -72 dBFS, so the dither a player adds to the digital silence
// it plays while idle is silence too. The instance's source sends no silent
// frames past a short tail (internal/micsink), and the daemon judges audio
// flowing (which paces a cluster pod's bulk transfers) by the same test.
const SilenceThreshold = 8

// Silent reports whether pcm (s16le) holds no sample louder than
// SilenceThreshold.
func Silent(pcm []byte) bool {
	for i := 0; i+1 < len(pcm); i += 2 {
		if v := int16(binary.LittleEndian.Uint16(pcm[i:])); v > SilenceThreshold || v < -SilenceThreshold {
			return false
		}
	}
	return true
}

// Prebuffer is the client's jitter buffer: an instance's audio starts playing
// once this much of it has arrived, or its first frame has waited this long,
// so frames that reach the client a little apart (a cluster's API server, a
// remote daemon) do not underrun the player. An underrun starts it again.
const Prebuffer = 80 * time.Millisecond

// mixer retains at most 200 ms per instance. When a network stalls, discard
// stale audio instead of replaying a growing backlog. Samples remain aligned.
type mixer map[string]*mixStream

// mixStream is one instance's queued audio: playing once primed (Prebuffer
// arrived, or waited for), or once its source ended.
type mixStream struct {
	pcm    []byte
	since  time.Time // when the oldest unplayed frame arrived
	primed bool
	ended  bool
}

func (m mixer) push(key string, pcm []byte, now time.Time) {
	if len(pcm) == 0 || len(pcm)%4 != 0 {
		return
	}
	s, ok := m[key]
	if !ok {
		if len(m) >= MaxStreams {
			return
		}
		s = &mixStream{since: now}
		m[key] = s
	}
	const limit = 10 * ChunkBytes
	if len(pcm) > limit {
		pcm = pcm[len(pcm)-limit:]
	}
	old := s.pcm
	if len(old)+len(pcm) > limit {
		old = old[len(old)+len(pcm)-limit:]
	}
	s.pcm, s.ended = append(old, pcm...), false
}

// end says key's source ended (or went quiet): what it queued plays out
// without waiting to be primed.
func (m mixer) end(key string) {
	if s, ok := m[key]; ok {
		s.ended = true
	}
}

// prebufferBytes is Prebuffer of audio.
const prebufferBytes = int(Prebuffer * SampleRate / time.Second * Channels * 2)

func (m mixer) next(now time.Time) []byte {
	if len(m) == 0 {
		return nil
	}
	sums := make([]int32, ChunkBytes/2)
	played := false
	for key, s := range m {
		if !s.primed && (s.ended || len(s.pcm) >= prebufferBytes || now.Sub(s.since) >= Prebuffer) {
			s.primed = true
		}
		if !s.primed {
			continue
		}
		played = true
		n := min(len(s.pcm), ChunkBytes)
		for i := 0; i < n; i += 2 {
			sums[i/2] += int32(int16(binary.LittleEndian.Uint16(s.pcm[i:])))
		}
		if n == len(s.pcm) {
			// Drained: the next frame primes the buffer again.
			delete(m, key)
		} else {
			s.pcm = s.pcm[n:]
		}
	}
	if !played {
		return nil
	}
	out := make([]byte, ChunkBytes)
	for i, value := range sums {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(max(-32768, min(32767, value)))))
	}
	return out
}
