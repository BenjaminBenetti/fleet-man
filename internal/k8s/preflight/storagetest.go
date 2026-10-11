//go:build !nok8s

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
)

// storagetest.go is the multi-node storage test: whether a StorageClass
// holds a fleet's share for pods on any node. A StorageClass does not say
// which access modes its volumes take, so fleet tries: a small
// ReadWriteMany volume of the class, mounted by two pods kept on different
// nodes (a required pod anti-affinity on the hostname), which then check the
// file system behaves for a fleet's agent dirs across them: a new file is
// seen from the other node, a rename, a symlink, modes and owners set in one
// pod read the same in the other, file owners are the uids that made them
// (no squashing, no fixed owner), a unix socket binds on it, appends from
// both pods land in order, and a lock one pod holds is held for the other.
// Everything it made is deleted before it returns. It takes about a minute,
// is cached per class (storagecache.go) and run again by the deep test.
// SMB Azure Files and EFS access points fail it (fixed owners); NFS, CephFS
// and Longhorn RWX typically pass.

// StorageTestTimeout bounds one class's multi-node test.
const StorageTestTimeout = 3 * time.Minute

// storageBatchTimeout bounds a full run's tests together (tests shorten it).
var storageBatchTimeout = StorageTestTimeout

// The test's phases (tests shorten them): the volume and the first pod,
// the second pod, the checks.
var (
	storageTestStart = 90 * time.Second
	storageTestMount = 75 * time.Second
	storageTestRun   = 75 * time.Second
	storageTestPoll  = 2 * time.Second
)

// storageSecondWait is how many seconds the second test pod waits to see
// the first's files: a network file system lists a new file from another
// node in milliseconds.
const storageSecondWait = 30

// storageTestParallel is how many classes are tested at once.
const storageTestParallel = 3

// storageTestImage is the test pods' image: python (its standard library
// does every check), pinned.
const storageTestImage = "docker.io/library/python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a"

// storageTestOwner is the uid:gid the test chowns a file to (as root).
const storageTestOwner = 4321

// The file system checks, in the order a failure is named. "steps" is no
// check: a pod that stopped seeing the other's later steps (the other
// stopped, or a lock held it up), which leaves the test without a verdict.
var storageChecks = []string{"visible", "rename", "symlink", "chmod", "uid", "chown", "unix_socket", "append", "flock", "steps"}

// storageCheckWhat says what each check is about, for its failure line.
var storageCheckWhat = map[string]string{
	"visible":     "a new file",
	"rename":      "a rename",
	"symlink":     "a symlink",
	"chmod":       "file modes",
	"uid":         "file owners",
	"chown":       "a changed owner",
	"unix_socket": "a unix socket",
	"append":      "appends",
	"flock":       "a lock",
}

// blockOnlyProvisioners make block disks only: their volumes never mount on
// two nodes at once, so Auto does not test them.
var blockOnlyProvisioners = map[string]bool{
	"ebs.csi.aws.com":                 true,
	"kubernetes.io/aws-ebs":           true,
	"pd.csi.storage.gke.io":           true,
	"kubernetes.io/gce-pd":            true,
	"disk.csi.azure.com":              true,
	"kubernetes.io/azure-disk":        true,
	"rbd.csi.ceph.com":                true,
	"kubernetes.io/rbd":               true,
	"cinder.csi.openstack.org":        true,
	"kubernetes.io/cinder":            true,
	"dobs.csi.digitalocean.com":       true,
	"linodebs.csi.linode.com":         true,
	"csi.hetzner.cloud":               true,
	"csi.scaleway.com":                true,
	"blockvolume.csi.oraclecloud.com": true,
}

// largeMinimum are provisioners every volume of which is at least the size
// named: Auto asks before it tests one (its test volume costs as much).
var largeMinimum = map[string]string{
	"filestore.csi.storage.gke.io": "1Ti",
}

// LargeMinimum is the smallest volume class's provisioner makes when that
// is large enough that Auto asks before testing it ("" when it is not).
func LargeMinimum(provisioner string) string { return largeMinimum[provisioner] }

// storageTestRequest is one class to test.
type storageTestRequest struct {
	class string
	// sc is the class (nil when StorageClasses are not readable).
	sc  *storagev1.StorageClass
	key string
	// size of the test volume.
	size string
}

// runStorageTest runs one test (tests replace it).
var runStorageTest = func(r *run, ctx context.Context, req storageTestRequest) StorageTest {
	return r.storageTest(ctx, req)
}

// storageTest runs the multi-node test of one class.
func (r *run) storageTest(ctx context.Context, req storageTestRequest) StorageTest {
	res := StorageTest{Class: req.class, Key: req.key, At: r.t.now()}
	if req.sc != nil {
		res.Provisioner = req.sc.Provisioner
	}
	ctx, cancel := context.WithTimeout(ctx, StorageTestTimeout)
	defer cancel()
	out := r.probeStorage(ctx, req)
	res.Nodes, res.Mounted = out.nodes, out.mounted
	res.MultiNode = len(out.nodes) == 2 && out.nodes[0] != out.nodes[1]
	switch {
	case out.inconclusive != "":
		res.Inconclusive, res.RetryAt = true, res.At.Add(inconclusiveRetry)
		res.Detail = "the test could not tell: " + out.inconclusive
	case !out.mounted:
		res.Detail = out.why
		res.Fix = classFix(req.sc, "")
	case out.failed != "":
		res.Failed = out.failed
		res.Detail = fmt.Sprintf("%s does not behave across the pods: %s", storageCheckWhat[out.failed], out.failDetail)
		res.Fix = classFix(req.sc, out.failed)
	case res.MultiNode:
		res.Detail = fmt.Sprintf("pods on %s and %s shared a ReadWriteMany volume, and its files behaved for fleet shares", out.nodes[0], out.nodes[1])
	case r.multiNode:
		// Both pods ran on one node of several: across nodes is not
		// proven (the scheduler could place them only there).
		res.Inconclusive, res.RetryAt = true, res.At.Add(inconclusiveRetry)
		res.Detail = fmt.Sprintf("the test's pods could run only on %s, so sharing across nodes was not tested; its files behaved on one node", out.nodes[0])
	default:
		res.Detail = "its files behaved for fleet shares on the one node (across nodes not tested: the cluster has one)"
	}
	return res
}

// storageOutcome is what the test's pods found.
type storageOutcome struct {
	// nodes are where the two pods ran (as many as did).
	nodes []string
	// mounted: both pods mounted the volume and ran the checks; why not.
	mounted bool
	why     string
	// failed is the first check that failed, with its detail.
	failed     string
	failDetail string
	// inconclusive says why the test could not tell.
	inconclusive string
}

// probeStorage makes the test's volume and pods, waits for their checks
// and reads them; everything it made is deleted before it returns.
func (r *run) probeStorage(ctx context.Context, req storageTestRequest) storageOutcome {
	ns := r.t.Cluster.Namespace
	cs := r.t.Client
	id := probeID()
	claim := "fleet-probe-mn-" + id
	first, second := claim+"-a", claim+"-b"
	pods := cs.CoreV1().Pods(ns)
	pvcs := cs.CoreV1().PersistentVolumeClaims(ns)
	zero := int64(0)
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		_ = pods.Delete(dctx, second, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = pods.Delete(dctx, first, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = pvcs.Delete(dctx, claim, metav1.DeleteOptions{})
	}()
	size, err := resource.ParseQuantity(req.size)
	if err != nil {
		return storageOutcome{inconclusive: fmt.Sprintf("test volume size %q: %v", req.size, err)}
	}
	class := req.class
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: ns, Labels: r.probeLabels(id + "-mn")},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	since := time.Now().Add(-time.Second)
	cctx, cancel := call(ctx)
	_, err = pvcs.Create(cctx, pvc, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return storageOutcome{inconclusive: "creating the test volume: " + Message(err)}
	}
	group := id + "-mn-pod"
	a, err := r.storagePod(first, group, "a", claim)
	if err != nil {
		return storageOutcome{inconclusive: err.Error()}
	}
	cctx, cancel = call(ctx)
	_, err = pods.Create(cctx, a, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return storageOutcome{inconclusive: "creating the first test pod: " + Message(err)}
	}
	sctx, scancel := context.WithTimeout(ctx, storageTestStart)
	nodeA, o := r.waitStoragePod(sctx, first, claim, since, true)
	scancel()
	if nodeA == "" {
		return o
	}
	out := storageOutcome{nodes: []string{nodeA}}
	b, err := r.storagePod(second, group, "b", claim)
	if err != nil {
		return storageOutcome{nodes: out.nodes, inconclusive: err.Error()}
	}
	cctx, cancel = call(ctx)
	_, err = pods.Create(cctx, b, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return storageOutcome{nodes: out.nodes, inconclusive: "creating the second test pod: " + Message(err)}
	}
	mctx, mcancel := context.WithTimeout(ctx, storageTestMount)
	nodeB, o := r.waitStoragePod(mctx, second, claim, since, false)
	mcancel()
	if nodeB == "" {
		o.nodes = out.nodes
		return o
	}
	out.nodes = append(out.nodes, nodeB)
	rctx, rcancel := context.WithTimeout(ctx, storageTestRun)
	defer rcancel()
	// The second pod's checks decide: it reads what the first wrote. One
	// that never sees the first pod's files ends at once, and the first is
	// not waited for (it waits for the second's, which never come).
	bLines, err := r.podChecks(rctx, second)
	if err != nil {
		out.inconclusive = err.Error()
		return out
	}
	out.mounted = true
	if failed, detail := firstFailure(bLines); failed == "visible" {
		// The first pod may have said why it wrote nothing (it could not
		// write the volume): its log so far, not waited for.
		if lines := r.peekChecks(rctx, first); len(lines) > 0 {
			if f, d := firstFailure(lines); f != "" && f != "steps" {
				out.failed, out.failDetail = f, d
				return out
			}
		}
		out.mounted, out.why = false, fmt.Sprintf("pods on %s and %s do not see each other's files on the volume (%s)", nodeA, nodeB, detail)
		if nodeA == nodeB {
			out.mounted, out.failed, out.failDetail = true, failed, detail
		}
		return out
	}
	aLines, err := r.podChecks(rctx, first)
	if err != nil {
		out.inconclusive = err.Error()
		return out
	}
	out.failed, out.failDetail = firstFailure(append(aLines, bLines...))
	if out.failed == "steps" {
		// No check failed: the pods lost step (an NFS server in its grace
		// period holds a lock for a minute and a half).
		out.failed, out.inconclusive = "", "the test's pods lost step with each other: "+out.failDetail
		out.failDetail = ""
	}
	return out
}

// podChecks waits for test pod name to finish its checks and reads them.
func (r *run) podChecks(ctx context.Context, name string) ([]storageLine, error) {
	if err := r.waitStorageDone(ctx, name); err != nil {
		return nil, err
	}
	log, err := clientLog(r, ctx, name)
	if err != nil {
		return nil, err
	}
	lines, err := parseStorageLog(log)
	if err != nil {
		return nil, fmt.Errorf("test pod %s: %v", name, err)
	}
	return lines, nil
}

// peekChecks reads the checks test pod name printed so far (nil when none
// failed and it has not finished, or its log cannot be read).
func (r *run) peekChecks(ctx context.Context, name string) []storageLine {
	log, err := clientLog(r, ctx, name)
	if err != nil {
		return nil
	}
	lines, err := parseStorageLog(log)
	if err != nil {
		return nil
	}
	return lines
}

// waitStoragePod waits for a test pod to run and returns its node; on ""
// the outcome says why it did not. first says it is the pod that binds the
// volume (whose provisioning failure is the class's refusal of RWX).
func (r *run) waitStoragePod(ctx context.Context, name, claim string, since time.Time, first bool) (string, storageOutcome) {
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	last := "waiting to be scheduled"
	for {
		gctx, cancel := call(ctx)
		p, err := pods.Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if p.Spec.NodeName != "" && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded) {
				return p.Spec.NodeName, storageOutcome{}
			}
			if p.Status.Phase == corev1.PodFailed {
				return "", storageOutcome{inconclusive: fmt.Sprintf("test pod %s failed: %s", name, p.Status.Message)}
			}
			if w := waitingOf(p); w != nil {
				last = w.Reason
				if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
					return "", storageOutcome{inconclusive: fmt.Sprintf("the test image %s did not pull: %s", storageTestImage, w.Message)}
				}
			} else if u := unschedulable(p); u != "" {
				last = "unschedulable: " + u
			}
		}
		// The provisioner refusing the access mode is the class's answer;
		// another failure may pass (a cloud share still being made): the
		// claim is waited for.
		if first {
			if msg := r.claimEvent(ctx, claim, since); msg != "" && refusesAccessMode(msg) {
				return "", storageOutcome{why: "no ReadWriteMany volume of the class: " + msg}
			}
		}
		select {
		case <-ctx.Done():
			// The wait's own context is spent: the last look gets its own.
			lctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			if !first {
				if ev := r.mountEvent(lctx, name); ev != "" {
					return "", storageOutcome{why: "a pod on a second node could not mount the volume: " + ev}
				}
				if strings.Contains(last, "anti-affinity") {
					return "", storageOutcome{inconclusive: "no second node could run the test's other pod (" + last + ")"}
				}
				if strings.HasPrefix(last, "Container") {
					return "", storageOutcome{why: "a pod on a second node could not mount the volume (still " + last + ")"}
				}
				return "", storageOutcome{inconclusive: "the second test pod did not run (" + last + ")"}
			}
			if bound := r.claimBound(claim); !bound {
				if msg := r.claimEvent(lctx, claim, since); msg != "" {
					if refusesAccessMode(msg) {
						return "", storageOutcome{why: "no ReadWriteMany volume of the class: " + msg}
					}
					return "", storageOutcome{inconclusive: "the class did not make the test volume: " + msg}
				}
				return "", storageOutcome{inconclusive: "no ReadWriteMany volume of the class bound (" + last + ")"}
			}
			return "", storageOutcome{inconclusive: "the first test pod did not run (" + last + ")"}
		case <-time.After(storageTestPoll):
		}
	}
}

// waitStorageDone waits for test pod name to finish its checks.
func (r *run) waitStorageDone(ctx context.Context, name string) error {
	pods := r.t.Client.CoreV1().Pods(r.t.Cluster.Namespace)
	for {
		gctx, cancel := call(ctx)
		p, err := pods.Get(gctx, name, metav1.GetOptions{})
		cancel()
		if err == nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
			return nil
		}
		select {
		case <-ctx.Done():
			// A lock or a file that never shows keeps a check waiting: an
			// NFS server in its grace period after a restart holds locks
			// for a minute and a half.
			return fmt.Errorf("test pod %s did not finish its checks within %s", name, storageTestRun)
		case <-time.After(storageTestPoll):
		}
	}
}

// claimBound reports whether the test's claim is bound.
func (r *run) claimBound(claim string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	pvc, err := r.t.Client.CoreV1().PersistentVolumeClaims(r.t.Cluster.Namespace).Get(ctx, claim, metav1.GetOptions{})
	return err == nil && pvc.Status.Phase == corev1.ClaimBound
}

// claimEvent is the claim's latest ProvisioningFailed event since since
// ("" when none): the provisioner refused the volume (a block class asked
// for ReadWriteMany).
func (r *run) claimEvent(ctx context.Context, claim string, since time.Time) string {
	cctx, cancel := call(ctx)
	defer cancel()
	evs, err := r.t.Client.CoreV1().Events(r.t.Cluster.Namespace).List(cctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim,involvedObject.name=" + claim})
	if err != nil {
		return ""
	}
	var latest *corev1.Event
	for i := range evs.Items {
		ev := &evs.Items[i]
		if ev.Reason != "ProvisioningFailed" || eventTime(ev).Before(since) {
			continue
		}
		if latest == nil || eventTime(ev).After(eventTime(latest)) {
			latest = ev
		}
	}
	if latest == nil {
		return ""
	}
	return firstLines(latest.Message, 2)
}

// accessModeRefusals are what a provisioner's refusal of an access mode
// says (CSI's MULTI_NODE_MULTI_WRITER for ReadWriteMany): other failures,
// such as a timeout while a cloud file share is made, may pass.
var accessModeRefusals = []string{"access mode", "accessmode", "multi_node", "readwritemany", "capabilit", "not supported", "unsupported"}

// refusesAccessMode reports whether a ProvisioningFailed message is the
// provisioner refusing the volume's access mode.
func refusesAccessMode(msg string) bool { return containsAny(msg, accessModeRefusals) }

// eventTime is when an event last happened.
func eventTime(ev *corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// storagePod is one of the test's pods: role ("a" first, "b" second)
// mounting claim at /data, kept off the other pod's node, as root (with only
// what a share's layout takes: changing owners and modes) where the
// namespace admits root, else as the share uid; placed like the
// connection's instances, hardened, both at one SELinux level.
func (r *run) storagePod(name, group, role, claim string) (*corev1.Pod, error) {
	// The first pod waits for the second to start and run its checks; the
	// second finds the first's files written already.
	wait := int((storageTestMount + storageTestRun) / time.Second)
	if role == "b" {
		wait = storageSecondWait
	}
	pod, err := r.probePod(name, group, []string{"python3", "-c", storageScript, role, fmt.Sprint(wait), fmt.Sprint(storageTestOwner)})
	if err != nil {
		return nil, err
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	deadline := int64((StorageTestTimeout + time.Minute) / time.Second)
	pod.Spec.ActiveDeadlineSeconds = &deadline
	c := &pod.Spec.Containers[0]
	c.Image = storageTestImage
	c.Env = []corev1.EnvVar{{Name: "PYTHONDONTWRITEBYTECODE", Value: "1"}}
	c.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
	psc := pod.Spec.SecurityContext
	if r.storageAsRoot() {
		root, no := int64(0), false
		psc.RunAsNonRoot, psc.RunAsUser, psc.RunAsGroup = &no, &root, &root
		c.SecurityContext.RunAsNonRoot = &no
		c.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"}}
	} else {
		uid, gid := podspec.ShareIDs()
		u, g := int64(uid), int64(gid)
		psc.RunAsUser, psc.RunAsGroup, psc.FSGroup = &u, &g, &g
	}
	// The two pods on different nodes: required where the nodes are known
	// to be several, else only preferred (the nodes are not readable; where
	// they ran tells).
	term := corev1.PodAffinityTerm{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelProbe: group}}, TopologyKey: corev1.LabelHostname}
	switch {
	case r.multiNode:
		pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term}}}
	case !r.nodesKnown:
		pod.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: term}}}}
	}
	if err := podspec.Harden(&pod.ObjectMeta, &pod.Spec, podspec.HardenOptions{PinnedImages: true, ShareGroup: group}); err != nil {
		return nil, fmt.Errorf("the test pod: %w", err)
	}
	return pod, nil
}

// storageAsRoot: the namespace admits root containers (the unprivileged
// canary), as it does an instance's share layout.
func (r *run) storageAsRoot() bool {
	a := r.caps.GetAdmission()
	return a == nil || a.GetUnprivileged()
}

// storageLine is one check's result, as a test pod printed it.
type storageLine struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// parseStorageLog reads a test pod's results; a log without its "done"
// line did not finish.
func parseStorageLog(log string) ([]storageLine, error) {
	var out []storageLine
	done := false
	for _, raw := range strings.Split(log, "\n") {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		var l storageLine
		if json.Unmarshal([]byte(raw), &l) != nil || l.Check == "" {
			continue
		}
		if l.Check == "done" {
			done = true
			continue
		}
		out = append(out, l)
	}
	for _, l := range out {
		if !l.OK {
			return out, nil
		}
	}
	if !done {
		return nil, fmt.Errorf("the checks did not finish: %q", firstLines(log, 3))
	}
	return out, nil
}

// firstFailure is the first failed check, in storageChecks' order, with its
// detail ("" when all passed).
func firstFailure(lines []storageLine) (string, string) {
	failed := map[string]string{}
	for _, l := range lines {
		if !l.OK {
			if _, have := failed[l.Check]; !have {
				failed[l.Check] = l.Detail
			}
		}
	}
	for _, name := range storageChecks {
		if d, ok := failed[name]; ok {
			return name, d
		}
	}
	for name, d := range failed {
		return name, d
	}
	return "", ""
}

// classFix is the remedy for a class that failed the test at check ("" for
// one no ReadWriteMany volume of mounted on two nodes), for the classes
// whose failure is known.
func classFix(sc *storagev1.StorageClass, check string) string {
	if sc == nil {
		return ""
	}
	p := strings.ToLower(sc.Provisioner)
	params := map[string]string{}
	for k, v := range sc.Parameters {
		params[strings.ToLower(k)] = strings.ToLower(v)
	}
	switch {
	case strings.Contains(p, "file.csi.azure.com") || strings.Contains(p, "azure-file"):
		if params["protocol"] != "nfs" {
			return "Azure Files over SMB keeps no owners, modes or symlinks: use an NFS Azure Files class (protocol: nfs, as azurefile-csi-nfs)"
		}
	case p == "efs.csi.aws.com":
		if params["provisioningmode"] == "efs-ap" {
			return "EFS access points give every file one owner: use statically provisioned EFS (a PersistentVolume per share) instead"
		}
	}
	switch check {
	case "uid", "chown":
		return "the file system does not keep file owners (an NFS export with root_squash, or a fixed owner): export it with no_root_squash, or use a class that keeps owners"
	case "flock":
		return "the file system does not share locks across nodes (NFS needs its lock manager, or NFSv4)"
	case "":
		if blockOnlyProvisioners[sc.Provisioner] {
			return "its provisioner makes block disks, which mount on one node at a time"
		}
	}
	return ""
}

// noClassFix is the remedy when no class passed the multi-node test, by the
// cloud the nodes run on.
func noClassFix(nodes []*fleetgrpc.KubeNode, providers map[string]string) string {
	cloud := ""
	for _, n := range nodes {
		if p := providers[n.GetName()]; p != "" {
			cloud = p
			break
		}
	}
	switch {
	case strings.HasPrefix(cloud, "gce://"):
		return "enable the Filestore CSI driver (RWX; each share is at least 1Ti)"
	case strings.HasPrefix(cloud, "azure://"):
		return "add an NFS Azure Files class (azurefile-csi-nfs)"
	case strings.HasPrefix(cloud, "aws://"):
		return "install the EFS CSI driver and statically provisioned EFS"
	}
	return "a ReadWriteMany StorageClass that passes the multi-node test (NFS CSI, CephFS, Longhorn RWX)"
}

// --- deciding what to test ---

// storageSkip is why Auto does not test class sc ("" when it does); multi
// says the cluster has several nodes (else Auto tests nothing: every pod
// shares the one node), nodesKnown that the nodes were read.
func storageSkip(sc *fleetgrpc.KubeStorageClass, multi, nodesKnown, consented bool) string {
	switch {
	case sc.GetNodeLocal():
		return "not tested: its volumes are on one node's disk"
	case blockOnlyProvisioners[sc.GetProvisioner()]:
		return "not tested: its provisioner makes block disks only"
	case !multi && !nodesKnown:
		return "not tested: the nodes are not readable, so Auto cannot tell the cluster has several; test it by name to use it"
	case !multi:
		return "not tested: on a cluster of one node every pod shares the node"
	case largeMinimum[sc.GetProvisioner()] != "" && !consented:
		return fmt.Sprintf("not tested: each of its volumes is at least %s, so Auto asks first; test it by name to use it", largeMinimum[sc.GetProvisioner()])
	}
	return ""
}

// testSize is the test volume's size for class sc: the class's minimum when
// it has a large one, else 1Gi.
func testSize(provisioner string) string {
	if m := largeMinimum[provisioner]; m != "" {
		return m
	}
	return "1Gi"
}

// storageResult is a class's test result as KubeStorageClass shows it.
func storageResult(t StorageTest) *fleetgrpc.KubeCheck {
	c := &fleetgrpc.KubeCheck{Name: "multi_node_test", Detail: t.Detail, Fix: t.Fix}
	switch {
	case t.Passed():
		c.Result = fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS
	case t.Inconclusive || (t.Mounted && !t.MultiNode && t.Failed == ""):
		c.Result = fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN
	default:
		c.Result = fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL
	}
	if !t.At.IsZero() {
		c.Detail += " (tested " + t.At.UTC().Format("2006-01-02 15:04") + " UTC)"
	}
	return c
}

// testStorage gives each class its multi-node test result for this run:
// the cached one, or a test run now for a class fleet shares could use
// (Auto's candidates on a cluster of several nodes, or the connection's own
// shared class) that has none, and for one the caller named; force runs the
// test again for those (the deep test). A class tested in this run already
// is not tested twice. New results go in the cache and on the classes the
// run lists.
func (r *run) testStorage(ctx context.Context, force bool) {
	todo := r.planStorageTests(force)
	if len(todo) > 0 {
		r.storageRunning(todo)
		// More classes than run at once share one test's time, which is
		// all FullTimeout counts for them: the run's later checks keep
		// theirs, and a class cut short is tested by a later run.
		tctx, cancel := context.WithTimeout(ctx, storageBatchTimeout)
		r.recordStorageTests(tctx, execStorageTests(r, tctx, todo))
		cancel()
	}
	r.showStorage()
}

// startStorageTests is the deep test's run of the multi-node tests again
// (testStorage with force), in the background: only the tests touch the
// cluster meanwhile; what it returns waits for them and records them.
func (r *run) startStorageTests(ctx context.Context) func() {
	todo := r.planStorageTests(true)
	if len(todo) == 0 {
		return func() {}
	}
	r.storageRunning(todo)
	done := make(chan []StorageTest, 1)
	go func() { done <- execStorageTests(r, ctx, todo) }()
	return func() {
		r.recordStorageTests(ctx, <-done)
		r.showStorage()
	}
}

// planStorageTests gives each class its cached result (or why it is not
// tested) and returns the tests to run (testStorage).
func (r *run) planStorageTests(force bool) []storageTestRequest {
	if r.storage == nil {
		r.storage = map[string]StorageTest{}
	}
	cache := r.t.StorageCache
	now := r.t.now()
	named := map[string]bool{}
	for _, n := range r.t.TestStorage {
		named[n] = true
	}
	picked := r.t.Cluster.SharedStorageClass
	canTest := cache != nil && r.t.Cluster.ID != "" && r.nsExists
	var todo []storageTestRequest
	consider := func(name string, sc *storagev1.StorageClass, provisioner string, skip string, wanted bool) {
		key := StorageTestKey(sc, name, r.multiNode)
		if r.storageFresh[key] {
			return
		}
		again := named[name] || (wanted && force)
		if !again {
			if t, ok := cache.Lookup(key, now); ok {
				r.storage[name] = t
				return
			}
			if t, ok := r.storage[name]; ok && t.Key == key {
				return
			}
		}
		if !wanted && !named[name] {
			delete(r.storage, name)
			r.skipped(name, skip)
			return
		}
		if canTest {
			todo = append(todo, storageTestRequest{class: name, sc: sc, key: key, size: testSize(provisioner)})
		}
	}
	listed := map[string]bool{}
	for _, c := range r.caps.StorageClasses {
		name := c.GetName()
		listed[name] = true
		var skip string
		var wanted bool
		switch {
		case name == picked:
			// The connection's own shared class is tested (picking it is
			// the consent a large one needs) unless its volumes never
			// mount on two nodes: then it simply holds shares in
			// single-node mode.
			if c.GetNodeLocal() || blockOnlyProvisioners[c.GetProvisioner()] {
				skip = storageSkip(c, true, true, true)
			} else {
				wanted = true
			}
		case picked != "":
			skip = "not tested: the connection names its own shared class"
		default:
			skip = storageSkip(c, r.multiNode, r.nodesKnown, false)
			wanted = skip == ""
		}
		consider(name, r.classObjs[name], c.GetProvisioner(), skip, wanted)
	}
	// StorageClasses are not readable: the class set (or named) is tested
	// by name, which the test still validates.
	if r.classesUnreadable {
		for _, name := range append([]string{picked}, r.t.TestStorage...) {
			if name != "" && !listed[name] {
				listed[name] = true
				consider(name, nil, "", "", true)
			}
		}
	}
	return todo
}

// skipped marks class name as not tested, saying why.
func (r *run) skipped(name, why string) {
	if why == "" {
		return
	}
	for _, c := range r.caps.StorageClasses {
		if c.GetName() == name {
			c.MultiNodeTest = &fleetgrpc.KubeCheck{Name: "multi_node_test", Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_SKIPPED, Detail: why}
		}
	}
}

// storageRunning reports the shared_storage check running for the tests.
func (r *run) storageRunning(todo []storageTestRequest) {
	names := make([]string, len(todo))
	for i, req := range todo {
		names[i] = req.class
	}
	sort.Strings(names)
	r.emit(&fleetgrpc.KubeCheck{Name: CheckSharedStorage, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING,
		Detail: "testing " + strings.Join(names, ", ") + " across two nodes (about a minute)"})
}

// execStorageTests runs the tests of todo, storageTestParallel at once; it
// touches nothing of the run but the cluster.
func execStorageTests(r *run, ctx context.Context, todo []storageTestRequest) []StorageTest {
	results := make([]StorageTest, len(todo))
	sem := make(chan struct{}, storageTestParallel)
	var wg sync.WaitGroup
	for i, req := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runStorageTest(r, ctx, req)
		}()
	}
	wg.Wait()
	return results
}

// recordStorageTests keeps results for the run and in the cache: one a
// cut-short run left inconclusive is no verdict, and is not cached.
func (r *run) recordStorageTests(ctx context.Context, results []StorageTest) {
	if r.storageFresh == nil {
		r.storageFresh = map[string]bool{}
	}
	for _, t := range results {
		r.storage[t.Class] = t
		r.storageFresh[t.Key] = true
		if ctx.Err() != nil && t.Inconclusive {
			continue
		}
		if err := r.t.StorageCache.Record(t); err != nil {
			r.warning("storage test cache: " + err.Error())
		}
	}
}

// showStorage puts each class's result on the classes the run lists.
func (r *run) showStorage() {
	for _, c := range r.caps.StorageClasses {
		if t, ok := r.storage[c.GetName()]; ok {
			c.MultiNodeTest = storageResult(t)
		}
	}
}

// storageScript is what the test's pods run (python3 -c ... role wait
// owner [dir], dir /data by default): role a first, then role b on the other node; they step through
// the checks in turn, each step marked by a file the other waits for. Each
// check prints one JSON line; "done" ends a pod's checks.
const storageScript = `
import errno, fcntl, json, os, socket, stat, sys, time

role, first_wait, OWNER = sys.argv[1], float(sys.argv[2]), int(sys.argv[3])
other = "b" if role == "a" else "a"
root = os.getuid() == 0
os.chdir(sys.argv[4] if len(sys.argv) > 4 else "/data")

def say(name, ok, detail=""):
    print(json.dumps({"check": name, "ok": bool(ok), "detail": detail}), flush=True)

def put(name, data):
    with open(name, "w") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())

def seen(name, timeout, full=False):
    # The folder is listed first: a network file system lists a new file
    # well before it opens one by a name it looked up and missed. full waits
    # for its content too (another node may list it before its writes land).
    deadline = time.time() + timeout
    while True:
        try:
            if name in os.listdir("."):
                with open(name) as f:
                    data = f.read()
                if data or not full:
                    return data
        except OSError:
            pass
        if time.time() > deadline:
            return None
        time.sleep(0.2)

def mark(n, data=""):
    put(".step-" + role + n, data)

def step(n, timeout, full=False):
    data = seen(".step-" + other + n, timeout, full)
    if data is None:
        # The first step's file is the other pod's first: not seeing it is
        # not seeing its files. A later one's, after files were seen, is the
        # other pod stopping (its own log says why).
        if n == "1":
            say("visible", False, "a file the pod on the other node wrote was not seen here within %ds" % timeout)
        else:
            say("steps", False, "the pod on the other node did not reach step %s within %ds" % (n, timeout))
        sys.exit(0)
    return data

def check(name, fn):
    try:
        ok, detail = fn()
    except OSError as e:
        ok, detail = False, str(e)
    say(name, ok, detail)
    return ok

def setup(name, fn):
    # One pod's own half of a check: only a failure is reported.
    try:
        fn()
    except OSError as e:
        say(name, False, "on the first node: %s" % e)

def a_socket():
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.bind("sock")
    s.listen(1)
    c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    c.settimeout(5)
    c.connect("sock")
    conn, _ = s.accept()
    c.sendall(b"ping")
    got = conn.recv(4)
    return got == b"ping", "" if got == b"ping" else "a connection through a socket on the volume got %r" % got

def a_rename():
    put("rename.tmp", "renamed by a\n")
    os.rename("rename.tmp", "renamed.txt")

def a_modes():
    put("mode.txt", "m")
    os.chmod("mode.txt", 0o640)
    os.mkdir("mode.d")
    os.chmod("mode.d", 0o750)

def a_chown():
    put("owned.txt", "o")
    os.chown("owned.txt", OWNER, OWNER)

def append(line):
    with open("append.log", "a") as f:
        f.write(line + "\n")

def a_wait_b1():
    deadline = time.time() + 20
    while time.time() < deadline:
        os.listdir(".")
        with open("append.log") as f:
            if "b1" in f.read():
                return
        time.sleep(0.2)

if role == "a":
    setup("visible", lambda: put("new.txt", "written by a\n"))
    setup("rename", a_rename)
    setup("symlink", lambda: os.symlink("renamed.txt", "link"))
    setup("chmod", a_modes)
    if root:
        setup("chown", a_chown)
    setup("append", lambda: append("a1"))
    check("unix_socket", a_socket)
    mark("1", json.dumps({"uid": os.getuid(), "gid": os.getgid(), "root": root}))
    step("1", first_wait)
    setup("append", a_wait_b1)
    setup("append", lambda: append("a2"))
    # A lock this pod cannot take (NFS without its lock manager: ENOLCK)
    # is the lock check failing; the steps go on, so the other pod ends too.
    lock = None
    try:
        lock = open("lock", "a")
        fcntl.flock(lock, fcntl.LOCK_EX)
    except OSError as e:
        say("flock", False, "taking a lock on the first node: %s" % e)
        lock = None
    mark("2")
    step("2", 30)
    if lock is not None:
        setup("flock", lambda: fcntl.flock(lock, fcntl.LOCK_UN))
    mark("3")
    step("3", 30)
    say("done", True)
    sys.exit(0)

info = json.loads(step("1", first_wait, True))
body = seen("new.txt", 20)
check("visible", lambda: (body == "written by a\n", "a new file written on the other node read %r here" % body))

def b_rename():
    got = seen("renamed.txt", 10)
    gone = "rename.tmp" not in os.listdir(".")
    return got == "renamed by a\n" and gone, "after a rename on the other node renamed.txt read %r here, and the old name %s" % (got, "is gone" if gone else "is still there")
check("rename", b_rename)

def b_symlink():
    target = os.readlink("link")
    with open("link") as f:
        through = f.read()
    return target == "renamed.txt" and through == "renamed by a\n", "a symlink to renamed.txt made on the other node reads as %r here (%r through it)" % (target, through)
check("symlink", b_symlink)

def b_chmod():
    m = stat.S_IMODE(os.stat("mode.txt").st_mode)
    d = stat.S_IMODE(os.stat("mode.d").st_mode)
    return m == 0o640 and d == 0o750, "modes set on the other node read %o (file) and %o (folder) here, not 640 and 750" % (m, d)
check("chmod", b_chmod)

def b_uid():
    st = os.stat("new.txt")
    ok = st.st_uid == info["uid"] and st.st_gid == info["gid"]
    return ok, "a file made as %d:%d on the other node is owned by %d:%d here" % (info["uid"], info["gid"], st.st_uid, st.st_gid)
check("uid", b_uid)

if info["root"]:
    def b_chown():
        st = os.stat("owned.txt")
        return (st.st_uid, st.st_gid) == (OWNER, OWNER), "a file given to %d:%d on the other node is owned by %d:%d here" % (OWNER, OWNER, st.st_uid, st.st_gid)
    check("chown", b_chown)

append("b1")
mark("1")
step("2", 30)

def b_append():
    with open("append.log") as f:
        log = f.read()
    return log == "a1\nb1\na2\n", "appends from both pods read %r" % log
check("append", b_append)

lock, held, lock_err = None, False, ""
try:
    lock = open("lock", "a")
except OSError as e:
    lock_err = str(e)
if lock is not None:
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(lock, fcntl.LOCK_UN)
    except OSError as e:
        held = e.errno in (errno.EAGAIN, errno.EACCES)
        if not held:
            lock_err = str(e)
mark("2")
step("3", 30)
got = False
deadline = time.time() + 15
while held and time.time() < deadline and not got:
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        got = True
    except OSError:
        time.sleep(0.2)
if lock_err:
    say("flock", False, "taking a lock here: %s" % lock_err)
elif not held:
    say("flock", False, "this pod took a lock the pod on the other node held")
else:
    say("flock", got, "" if got else "a lock the other pod let go of stayed held here")
mark("3")
say("done", True)
`
