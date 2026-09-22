package coder

import "testing"

// No virtual microphone here yet: the capability answer and the command must
// agree, so nothing gets installed for a sink that can never be attached.
func TestNoMicSink(t *testing.T) {
	b := New()
	if b.SupportsMicSink() {
		t.Fatal("SupportsMicSink should be false")
	}
	if cmd, ok := b.MicSinkCommand("ws"); ok || cmd != nil {
		t.Fatalf("MicSinkCommand = %v, %v; want nil, false", cmd, ok)
	}
}
