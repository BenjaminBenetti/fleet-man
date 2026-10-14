//go:build !nok8s

package kubernetes

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// digestOf is a digest of the real shape whose hex starts with prefix.
func digestOf(prefix string) string { return "sha256:" + prefix + strings.Repeat("0", 64-len(prefix)) }

// The digests of the bug report: two `fleet up`s of one repository with
// registry delivery, on docker's containerd image store. Each build is an OCI
// index of its own (a new provenance attestation beside one platform
// manifest and config), so the second instance is pinned to the second
// index; the node, which pulled the content for the first, reports the
// first's digest for it, and names the image by its config.
var (
	firstIndex   = digestOf("6df0e7")
	secondIndex  = digestOf("b4269be")
	sameManifest = digestOf("9a815c0f")
	sameConfig   = digestOf("beb64e")
	// otherIndex and otherConfig are another image fleet delivered on the
	// connection.
	otherIndex  = digestOf("0a0e1")
	otherConfig = digestOf("0c0f1")
)

const contentRepo = "registry.example.com/team/fleet/3fa2c91e/devcontainer"

// withDeliveries makes fleet's delivery records on handleCluster those of
// the report, once the first instance's image was collected (the digest it
// went under is kept while the content is held).
func withDeliveries(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	entry := func(index, config, holder string) kubeimage.Entry {
		return kubeimage.Entry{ImageID: index, Strategy: kubeimage.StrategyRegistry, Location: contentRepo, Ref: contentRepo + "@" + index, Digest: index, Config: config, Holders: []string{holder}}
	}
	second := entry(secondIndex, sameConfig, "inst-2")
	second.Manifest = sameManifest
	writeIndex(t, state.KubeImagesPath(handleCluster), kubeimage.Index{Version: 1,
		Images:  []kubeimage.Entry{second, entry(otherIndex, otherConfig, "other")},
		Configs: map[string][]string{sameConfig: {firstIndex, sameManifest, secondIndex}, otherConfig: {otherIndex}},
	})
	SetEnvironment(Environment{Images: &kubeimage.Manager{IndexPath: state.KubeImagesPath}})
	t.Cleanup(func() { SetEnvironment(Environment{}) })
}

// statusCase is a dev container status: the image the kubelet names and
// its imageID, and whether it is the delivered image.
type statusCase struct {
	image, imageID string
	ok             bool
}

// tamperCases are the statuses no instance of the report's content may
// show.
var tamperCases = map[string]statusCase{
	"another image fleet delivered":           {otherConfig, contentRepo + "@" + otherIndex, false},
	"another config under the delivered name": {digestOf("e011"), contentRepo + "@" + digestOf("e012"), false},
	"an unknown digest named by a tag":        {contentRepo + ":abc", contentRepo + "@" + digestOf("e013"), false},
	"the config digest in a reference":        {"evil.example/x@" + sameConfig, "evil.example/x@" + digestOf("e014"), false},
	"the config digest under a scheme":        {"docker://" + sameConfig, contentRepo + "@" + digestOf("e015"), false},
}

func checkStatuses(t *testing.T, rec InstanceRecord, cases map[string]statusCase) {
	t.Helper()
	set, pod := instanceObjects("ns", "fi-a-b-key", "pod-1")
	want := rec.expect()
	want.SpecHash, want.StatefulSetUID = testHash(&pod.Spec), set.UID
	for name, tc := range cases {
		pod.Status.ContainerStatuses[0].Image, pod.Status.ContainerStatuses[0].ImageID = tc.image, tc.imageID
		err := VerifyPod(pod, set, want, testHash)
		var tampered *TamperedError
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !tc.ok && (!errors.As(err, &tampered) || !strings.Contains(err.Error(), "not the image fleet delivered")):
			t.Errorf("%s: %v, want kubernetes:tampered", name, err)
		}
	}
}

func TestVerifyPodAcceptsTheDeliveredContentUnderAnotherDigest(t *testing.T) {
	withDeliveries(t)
	second := InstanceRecord{Cluster: handleCluster, ImageDigests: []string{secondIndex, secondIndex, sameManifest}, ImageConfig: sameConfig}
	cases := map[string]statusCase{
		// As the report has it: the runtime names the image by its config.
		"the first's digest, as containerd reports it": {sameConfig, contentRepo + "@" + firstIndex, true},
		// A node that also knows a tag of it names it by the tag; the first
		// digest is one fleet delivered this content under.
		"the first's digest, named by a tag": {contentRepo + ":abc", contentRepo + "@" + firstIndex, true},
		"its own digest":                     {contentRepo + "@" + secondIndex, "docker-pullable://" + contentRepo + "@" + secondIndex, true},
		"its platform manifest":              {contentRepo + ":abc", contentRepo + "@" + sameManifest, true},
	}
	for name, tc := range tamperCases {
		cases[name] = tc
	}
	checkStatuses(t, second, cases)

	// Without fleet's delivery records, the config alone still says it is
	// the content; a digest no record names is refused.
	SetEnvironment(Environment{})
	t.Setenv("HOME", t.TempDir())
	checkStatuses(t, second, map[string]statusCase{
		"by its config":                      {sameConfig, contentRepo + "@" + firstIndex, true},
		"the first's digest, named by a tag": {contentRepo + ":abc", contentRepo + "@" + firstIndex, false},
	})
}

// A record an older fleet wrote names only its digests: its pod verifies by
// them as it did, and by its content once fleet's delivery records know it.
func TestVerifyPodOfAnOlderRecord(t *testing.T) {
	withDeliveries(t)
	first := InstanceRecord{Cluster: handleCluster, ImageDigests: []string{firstIndex, firstIndex}}
	cases := map[string]statusCase{
		"its own digest":              {contentRepo + "@" + firstIndex, contentRepo + "@" + firstIndex, true},
		"its own digest, by a config": {sameConfig, contentRepo + "@" + firstIndex, true},
		"the second's digest":         {sameConfig, contentRepo + "@" + secondIndex, true},
	}
	for name, tc := range tamperCases {
		cases[name] = tc
	}
	checkStatuses(t, first, cases)

	SetEnvironment(Environment{})
	t.Setenv("HOME", t.TempDir())
	checkStatuses(t, first, map[string]statusCase{
		"its own digest":      {contentRepo + "@" + firstIndex, contentRepo + "@" + firstIndex, true},
		"the second's digest": {sameConfig, contentRepo + "@" + secondIndex, false},
	})
}

// The seed Job of the report's second instance runs on the node that
// reports the first's digest: it gets the workspace; another image does
// not.
func TestCheckJobPodAcceptsTheDeliveredContent(t *testing.T) {
	withDeliveries(t)
	want := corev1.PodSpec{Containers: []corev1.Container{{Name: podspec.JobContainer, Image: contentRepo + "@" + secondIndex}}}
	del := &kubeimage.Delivery{Strategy: kubeimage.StrategyRegistry, Ref: contentRepo + "@" + secondIndex, ImageID: secondIndex, Digest: secondIndex, Manifest: sameManifest, Config: sameConfig}
	img := deliveredExpect(handleCluster, del)
	pod := func(image, imageID string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "ns"}, Spec: *want.DeepCopy(),
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: podspec.JobContainer, Image: image, ImageID: imageID}}}}
	}
	for _, p := range []*corev1.Pod{pod(sameConfig, contentRepo+"@"+firstIndex), pod(contentRepo+":abc", contentRepo+"@"+firstIndex)} {
		if learned, err := checkJobPod(p, &want, del, img); err != nil || learned != "" {
			t.Errorf("%s: learned %q, %v", p.Status.ContainerStatuses[0].ImageID, learned, err)
		}
	}
	var tampered *TamperedError
	for name, tc := range tamperCases {
		if _, err := checkJobPod(pod(tc.image, tc.imageID), &want, del, img); !errors.As(err, &tampered) {
			t.Errorf("%s: %v, want kubernetes:tampered", name, err)
		}
	}
}

// A record an older fleet wrote learns its image's config once its pod
// verified by a digest it names, and only then.
func TestLearnImageConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	running := func(image, imageID string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: DevContainer, Image: image, ImageID: imageID}}}}
	}
	old := InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: handleSTS(), Gen: 1, ImageDigests: []string{firstIndex, firstIndex}}
	saved := func(t *testing.T) InstanceRecord {
		t.Helper()
		r, err := LoadInstanceRecord(handleCluster, handleKey)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for name, pod := range map[string]*corev1.Pod{
		"named by a tag":       running(contentRepo+":abc", contentRepo+"@"+firstIndex),
		"another digest":       running(sameConfig, contentRepo+"@"+secondIndex),
		"a config in a ref":    running("x@"+sameConfig, contentRepo+"@"+firstIndex),
		"not running it yet":   running("", ""),
		"no dev container yet": {},
	} {
		if err := SaveInstanceRecord(old); err != nil {
			t.Fatal(err)
		}
		rec := old
		learnImageConfig(pod, &rec)
		if rec.ImageConfig != "" || saved(t).ImageConfig != "" {
			t.Errorf("%s: learned %q", name, rec.ImageConfig)
		}
	}
	// The record changed since it was read (a rebuild): left as it is.
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Key: handleKey, StatefulSet: handleSTS(), Gen: 2, ImageDigests: []string{secondIndex}}); err != nil {
		t.Fatal(err)
	}
	rec := old
	learnImageConfig(running(sameConfig, contentRepo+"@"+firstIndex), &rec)
	if got := saved(t); got.Gen != 2 || got.ImageConfig != "" {
		t.Errorf("a changed record became %+v", got)
	}

	if err := SaveInstanceRecord(old); err != nil {
		t.Fatal(err)
	}
	rec = old
	learnImageConfig(running(sameConfig, "docker-pullable://"+contentRepo+"@"+firstIndex), &rec)
	if got := saved(t); rec.ImageConfig != sameConfig || got.ImageConfig != sameConfig || got.Gen != 1 {
		t.Errorf("learned %q, saved %+v", rec.ImageConfig, got)
	}
}
