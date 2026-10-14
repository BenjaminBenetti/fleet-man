//go:build !nok8s

package kubernetes

import (
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// prependReactor is cs.PrependReactor under the clientset's lock: the fake's
// own takes none, while every call reads the chain under it, so a reactor
// added once a rig's goroutines (the job harness's pod maker, a pool's
// watchers) use cs races them.
func prependReactor(cs *fake.Clientset, verb, resource string, reaction k8stesting.ReactionFunc) {
	cs.Lock()
	defer cs.Unlock()
	cs.PrependReactor(verb, resource, reaction)
}
