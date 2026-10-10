//go:build !nok8s

package podspec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// What refuses a render, and how fleet names it. The API server refuses an
// object at admission (pod security, a webhook, an admission policy, the
// LimitRange and ResourceQuota plugins), or before it (RBAC, validation, a
// missing namespace). A server-side dry-run (DryRun) returns the refusal
// before anything is created; after an apply, the StatefulSet and Job
// controllers record the same refusal as a FailedCreate event on their
// object when they create its pod (ClassifyFailedCreate).

// DenialKind says what refused an object.
type DenialKind string

const (
	// DeniedPodSecurity is the namespace's pod security admission.
	DeniedPodSecurity DenialKind = "pod-security"
	// DeniedWebhook is a validating or mutating admission webhook (a policy
	// engine such as Gatekeeper or Kyverno), or the failure to call one.
	DeniedWebhook DenialKind = "webhook"
	// DeniedPolicy is a ValidatingAdmissionPolicy.
	DeniedPolicy DenialKind = "admission-policy"
	// DeniedAdmission is another admission plugin.
	DeniedAdmission DenialKind = "admission"
	// DeniedQuota is a ResourceQuota without room, or one that needs a
	// request or limit the object does not set.
	DeniedQuota DenialKind = "quota"
	// DeniedLimitRange is a LimitRange's minimum, maximum or ratio.
	DeniedLimitRange DenialKind = "limit-range"
	// DeniedServiceAccount is a service_account the namespace lacks.
	DeniedServiceAccount DenialKind = "service-account"
	// DeniedNamespace is a namespace that does not exist or is being
	// deleted.
	DeniedNamespace DenialKind = "namespace"
	// DeniedRBAC is fleet's credential lacking a permission.
	DeniedRBAC DenialKind = "rbac"
	// DeniedInvalid is an object the API server finds invalid.
	DeniedInvalid DenialKind = "invalid"
)

// Denial is one object the API server refused.
type Denial struct {
	Kind DenialKind
	// Object is what was refused, as "Kind name" ("Pod fi-web-x-0").
	Object string
	// Message is the API server's message, as it gave it.
	Message string
}

// Policy reports whether an admission policy refused the object (pod
// security, a webhook, an admission policy, another admission plugin): the
// denials dropping the debug grants may get past. Quotas, limit ranges and
// the rest refuse whatever the grants.
func (d *Denial) Policy() bool {
	switch d.Kind {
	case DeniedWebhook:
		// A webhook that could not be called decided nothing about the
		// grants.
		return !strings.Contains(d.Message, "failed calling webhook")
	case DeniedPodSecurity, DeniedPolicy, DeniedAdmission:
		return true
	}
	return false
}

// Code is the denial's error code.
func (d *Denial) Code() string {
	switch d.Kind {
	case DeniedQuota:
		return CodeQuota
	case DeniedServiceAccount, DeniedNamespace:
		return CodeSettings
	case DeniedRBAC:
		return CodeForbidden
	case DeniedInvalid:
		return CodeInvalidSpec
	}
	return CodeAdmissionDenied
}

// Refusal is the denial as a create's error: its code, what was refused and
// what to change.
func (d *Denial) Refusal() *Refusal {
	detail := denialDetail(d.Message)
	var reason string
	switch d.Kind {
	case DeniedPodSecurity:
		level := "its level"
		if m := psaLevelRE.FindStringSubmatch(d.Message); m != nil {
			level = m[1]
		}
		reason = fmt.Sprintf("%s violates the namespace's pod security (%s): %s. Use a namespace whose pod-security.kubernetes.io/enforce level admits it, or take what it names out of the devcontainer", d.Object, level, psaViolations(d.Message))
	case DeniedWebhook:
		reason = fmt.Sprintf("%s was refused by an admission webhook: %s. The cluster's policy decides; ask its owner, or take what it names out of the devcontainer", d.Object, detail)
	case DeniedPolicy:
		reason = fmt.Sprintf("%s was refused by an admission policy: %s. The cluster's policy decides; ask its owner, or take what it names out of the devcontainer", d.Object, detail)
	case DeniedQuota:
		reason = fmt.Sprintf("%s does not fit the namespace's ResourceQuota: %s. Stop or destroy instances there, lower the connection's resources, or raise the quota", d.Object, detail)
	case DeniedLimitRange:
		reason = fmt.Sprintf("%s is outside the namespace's LimitRange: %s. Lower memory_limit or the devcontainer's -m/--cpus to fit, or raise the LimitRange", d.Object, detail)
	case DeniedServiceAccount:
		reason = fmt.Sprintf("%s: %s. Set the connection's service_account to one the namespace has", d.Object, detail)
	case DeniedNamespace:
		reason = fmt.Sprintf("%s: %s. Point the connection at a namespace that exists", d.Object, detail)
	case DeniedRBAC:
		reason = fmt.Sprintf("fleet's credential for the connection may not do what %s needs: %s", d.Object, detail)
	case DeniedInvalid:
		reason = fmt.Sprintf("the API server finds %s invalid: %s", d.Object, detail)
	default:
		reason = fmt.Sprintf("%s was refused at admission: %s", d.Object, detail)
	}
	return &Refusal{Code: d.Code(), Reason: reason}
}

func (d *Denial) Error() string { return d.Refusal().Error() }

// Decide is Denied for a classified denial: a policy denial follows the
// connection's debug-capabilities policy for pod (rerender: render without
// the debug grants), any other is the create's error as it is. Like Denied,
// it cannot tell a denial of the debug grants alone from one of something
// else as well; Admit, which asks again without them, can.
func (d *Denial) Decide(cluster *fleet.KubeCluster, pod *PodSpec) (rerender bool, err error) {
	if !d.Policy() {
		return false, d.Refusal()
	}
	return Denied(cluster, pod, d.Object+": "+denialDetail(d.Message))
}

// DenialOf classifies the error the API server answered object's create or
// apply with; nil when err is no refusal (a dropped connection, a timeout,
// an object that already exists).
func DenialOf(object string, err error) *Denial {
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return nil
	}
	msg := status.Status().Message
	if msg == "" {
		msg = err.Error()
	}
	switch {
	case admissionRefusal(msg):
		// Whatever status it chose: a webhook or an admission policy may
		// answer 400 or 422 (a policy's reason Invalid).
		return classifyMessage(object, msg)
	case apierrors.IsInvalid(err):
		return &Denial{Kind: DeniedInvalid, Object: object, Message: msg}
	case apierrors.IsNotFound(err):
		if d := status.Status().Details; d != nil && d.Kind == "namespaces" {
			return &Denial{Kind: DeniedNamespace, Object: object, Message: msg}
		}
		return nil
	case apierrors.IsForbidden(err), apierrors.IsBadRequest(err):
		return classifyMessage(object, msg)
	case apierrors.IsInternalError(err) && strings.Contains(msg, "failed calling webhook"):
		return &Denial{Kind: DeniedWebhook, Object: object, Message: msg}
	}
	return nil
}

// ClassifyFailedCreate classifies a FailedCreate event the StatefulSet or
// Job controller recorded on object; nil when the event is no refusal (the
// controller failed for another reason, which it retries).
func ClassifyFailedCreate(object, message string) *Denial {
	msg := message
	for _, re := range failedCreatePrefixes {
		if m := re.FindStringSubmatch(msg); m != nil {
			msg = m[1]
			break
		}
	}
	if namespaceGoneRE.MatchString(msg) {
		return &Denial{Kind: DeniedNamespace, Object: object, Message: msg}
	}
	if strings.Contains(msg, "failed calling webhook") {
		return &Denial{Kind: DeniedWebhook, Object: object, Message: msg}
	}
	if admissionRefusal(msg) {
		return classifyMessage(object, msg)
	}
	if strings.Contains(msg, " is invalid: ") {
		return &Denial{Kind: DeniedInvalid, Object: object, Message: msg}
	}
	if !strings.Contains(msg, " is forbidden: ") && !strings.Contains(msg, "denied the request") {
		return nil
	}
	return classifyMessage(object, msg)
}

// failedCreatePrefixes are the controllers' wrappings of the API server's
// message: the StatefulSet controller's "create Pod <pod> in StatefulSet
// <sts> failed error: ..." and the Job controller's "Error creating: ...".
var failedCreatePrefixes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^create \S+ \S+ in StatefulSet \S+ failed error: (.*)$`),
	regexp.MustCompile(`(?s)^Error creating: (.*)$`),
}

var (
	psaLevelRE      = regexp.MustCompile(`violates PodSecurity "([a-z]+)(?::[^"]*)?"`)
	psaLeadRE       = regexp.MustCompile(`^violates PodSecurity "[^"]*":`)
	rbacRE          = regexp.MustCompile(`cannot [a-z]+ resource "`)
	namespaceGoneRE = regexp.MustCompile(`namespaces "[^"]*" not found|because it is being terminated`)
	limitRangeRE    = regexp.MustCompile(`(maximum|minimum) [a-z.-]+ usage per (Container|Pod|PersistentVolumeClaim)|limit to request ratio per (Container|Pod)`)
	forbiddenRE     = regexp.MustCompile(`^.*? is forbidden: `)
)

// admissionRefusal reports whether msg is an admission decision that names
// what made it: pod security, a webhook's denial, an admission policy's.
func admissionRefusal(msg string) bool {
	return strings.Contains(msg, `violates PodSecurity "`) ||
		(strings.Contains(msg, `admission webhook "`) && strings.Contains(msg, `denied the request`)) ||
		(strings.Contains(msg, "ValidatingAdmissionPolicy '") && strings.Contains(msg, "denied request"))
}

// classifyMessage classifies a refusal by its message.
func classifyMessage(object, msg string) *Denial {
	kind := DeniedAdmission
	switch {
	case strings.Contains(msg, "violates PodSecurity"):
		kind = DeniedPodSecurity
	case strings.Contains(msg, "admission webhook") && strings.Contains(msg, "denied the request"),
		strings.Contains(msg, "failed calling webhook"):
		kind = DeniedWebhook
	case strings.Contains(msg, "ValidatingAdmissionPolicy"):
		kind = DeniedPolicy
	case strings.Contains(msg, "exceeded quota") || strings.Contains(msg, "failed quota"):
		kind = DeniedQuota
	case limitRangeRE.MatchString(msg):
		kind = DeniedLimitRange
	case strings.Contains(msg, "error looking up service account") || strings.Contains(msg, "serviceaccount \""):
		kind = DeniedServiceAccount
	case namespaceGoneRE.MatchString(msg):
		kind = DeniedNamespace
	case rbacRE.MatchString(msg) && strings.Contains(msg, "User \""):
		kind = DeniedRBAC
	}
	return &Denial{Kind: kind, Object: object, Message: msg}
}

// denialDetail is a refusal's message without the API server's own prefix
// naming the object ("pods \"x\" is forbidden: "), which the denial names
// already.
func denialDetail(msg string) string {
	if loc := forbiddenRE.FindStringIndex(msg); loc != nil {
		return strings.TrimSpace(msg[loc[1]:])
	}
	return strings.TrimSpace(msg)
}

// psaViolations is what a pod security refusal lists after its lead
// ("violates PodSecurity \"baseline:latest\": "), which the reason words
// its own way.
func psaViolations(msg string) string {
	detail := denialDetail(msg)
	if loc := psaLeadRE.FindStringIndex(detail); loc != nil {
		return strings.TrimSpace(detail[loc[1]:])
	}
	return detail
}

// Denials returns every Denial in err (DryRun joins them).
func Denials(err error) []*Denial {
	var out []*Denial
	var walk func(error)
	walk = func(err error) {
		if d, ok := err.(*Denial); ok {
			out = append(out, d)
			return
		}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				walk(e)
			}
			return
		}
		if e := errors.Unwrap(err); e != nil {
			walk(e)
		}
	}
	if err != nil {
		walk(err)
	}
	return out
}

// denials is DryRun's error: every object refused, the first one's code
// leading.
type denials struct{ ds []*Denial }

func (e *denials) Error() string {
	parts := make([]string, len(e.ds))
	for i, d := range e.ds {
		parts[i] = d.Error()
	}
	return strings.Join(parts, "; ")
}

func (e *denials) Unwrap() []error {
	out := make([]error, len(e.ds))
	for i, d := range e.ds {
		out[i] = d
	}
	return out
}

// addDenial adds d to ds unless a denial of the same kind and detail is in
// already (the Pod and its StatefulSet refused for one reason).
func addDenial(ds []*Denial, d *Denial) []*Denial {
	for _, have := range ds {
		if have.Kind == d.Kind && denialDetail(have.Message) == denialDetail(d.Message) {
			return ds
		}
	}
	return append(ds, d)
}
