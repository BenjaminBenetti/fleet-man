package backend

// AudioGate is an optional capability for a backend whose instances may be
// unable to have audio (the virtual microphone and the audio output) for a
// reason of their own: a Kubernetes cluster that runs containers without
// root, on an image that lacks the audio stack, cannot install it. A backend
// that implements it folds the answer into SupportsMicSink and its output
// source's ok as well; what this adds is the reason, in words the user is
// shown.
type AudioGate interface {
	// AudioUnavailable says why the bound instance can have no audio, as the
	// line its banner shows ("audio unavailable on cluster <name>: ..."); ""
	// when it can. It reads what the backend keeps of the instance on the
	// daemon host, never the instance itself.
	AudioUnavailable() string
}

// AudioUnavailable is b's AudioGate answer: "" for a backend without one
// (every instance of it can have audio, as far as the backend knows).
func AudioUnavailable(b any) string {
	if g, ok := b.(AudioGate); ok {
		return g.AudioUnavailable()
	}
	return ""
}

// AudioFlows is an optional capability for a backend whose bulk transfers
// share a link with its instances' audio streams: a Kubernetes pod's bulk
// session (copies, staging, folder tars) shares the daemon host's uplink and
// the API server with the agent session that carries its PCM. The audio paths
// tell it when audio actually flows on an instance, and when one of its PCM
// frames arrived late; it paces the bulk transfers only then. Backends whose
// transfers do not compete with their audio do not implement it.
type AudioFlows interface {
	// AudioFlow is the instance's audio activity sink, nil when the backend
	// has none for containerID (no pod session was ever made for it).
	AudioFlow(containerID string) AudioFlow
}

// AudioFlow is one instance's audio activity, as its audio paths see it. Its
// methods are cheap and safe for concurrent use.
type AudioFlow interface {
	// MicDemand says whether something in the instance is recording from the
	// virtual microphone (its sink reported demand), so audio flows into it.
	MicDemand(recording bool)
	// OutputAudible says the instance's output source just delivered a frame
	// that was not silence: audio flows out of it, for a little while.
	OutputAudible()
	// Late says a PCM frame of the instance's arrived more than a frame's
	// tolerance late: the bulk transfers are crowding the audio out.
	Late()
}

// AudioFlowOf is b's AudioFlow for containerID: nil for a backend without
// AudioFlows, or without one for that instance.
func AudioFlowOf(b any, containerID string) AudioFlow {
	if f, ok := b.(AudioFlows); ok {
		return f.AudioFlow(containerID)
	}
	return nil
}
