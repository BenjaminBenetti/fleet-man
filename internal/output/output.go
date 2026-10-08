// Package output plays instance audio on a connected client. It has no access
// to daemon state or backends, including when the daemon happens to be local.
package output

import (
	"encoding/binary"
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

// mixer retains at most 200 ms per instance. When a network stalls, discard
// stale audio instead of replaying a growing backlog. Samples remain aligned.
type mixer map[string][]byte

func (m mixer) push(key string, pcm []byte) {
	if len(pcm) == 0 || len(pcm)%4 != 0 {
		return
	}
	if _, ok := m[key]; !ok && len(m) >= MaxStreams {
		return
	}
	const limit = 10 * ChunkBytes
	if len(pcm) > limit {
		pcm = pcm[len(pcm)-limit:]
	}
	old := m[key]
	if len(old)+len(pcm) > limit {
		old = old[len(old)+len(pcm)-limit:]
	}
	m[key] = append(old, pcm...)
}

func (m mixer) next() []byte {
	if len(m) == 0 {
		return nil
	}
	sums := make([]int32, ChunkBytes/2)
	for key, pcm := range m {
		n := min(len(pcm), ChunkBytes)
		for i := 0; i < n; i += 2 {
			sums[i/2] += int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		}
		if n == len(pcm) {
			delete(m, key)
		} else {
			m[key] = pcm[n:]
		}
	}
	out := make([]byte, ChunkBytes)
	for i, value := range sums {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(max(-32768, min(32767, value)))))
	}
	return out
}
