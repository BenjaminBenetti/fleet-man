// Package names is the one definition of the names, labels and annotations
// fleet gives the objects it creates in a Kubernetes namespace, and of the
// hashes those names are built from. Every renderer, informer, GC pass and
// test derives them here, so they cannot drift apart.
//
// It is pure Go with no client-go dependency, so any package may import it,
// the client-go-free pod build (-tags nok8s) included.
//
// Every name fits the strictest Kubernetes rule its object kind meets:
// instance names fit a StatefulSet (52 characters, so the pod's
// controller-revision-hash label stays a valid label value), fleet object
// names a Service (an RFC 1035 label), and every label value the label value
// grammar. FuzzK8sName holds them to it for any fleet or instance name.
package names

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Labels every object fleet creates carries. Informers and GC select on
// managed-by, owner and cluster together (Selector), so two daemons, or two
// connections of one daemon to the same namespace, never see each other's
// objects.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelOwner holds Owner(daemonID).
	LabelOwner = "fleet.dev/owner"
	// LabelCluster holds the KubeCluster record id.
	LabelCluster = "fleet.dev/cluster"
	// LabelFleet holds FleetHash, not the fleet name, which is not a valid
	// label value in general.
	LabelFleet = "fleet.dev/fleet"
	// LabelKey holds an instance's InstanceKey (Instance.KubeKey).
	LabelKey = "fleet.dev/key"
	// LabelRole holds a Role.
	LabelRole = "fleet.dev/role"
	// LabelShareGroup holds the FleetHash of the share an instance mounts.
	LabelShareGroup = "fleet.dev/share-group"

	// ManagedBy is LabelManagedBy's value.
	ManagedBy = "fleet"
)

// Annotations fleet sets. They hold hashes and non-secret fields only.
const (
	AnnotationContainerCreated = "fleet.dev/container-created"
	AnnotationRebuildGen       = "fleet.dev/rebuild-gen"
	AnnotationSpecHash         = "fleet.dev/spec-hash"
	AnnotationImageID          = "fleet.dev/image-id"
	// AnnotationOp mirrors the job journal: "<op>@<step>".
	AnnotationOp = "fleet.dev/op"
	// AnnotationInfo is the PVC's non-secret summary.
	AnnotationInfo = "fleet.dev/info"
	// AnnotationHolderHost names the host of the daemon holding an owner
	// Lease, for the OWNER_CONFLICT message.
	AnnotationHolderHost = "fleet.dev/holder-host"
)

// Role is LabelRole's value: what an object is for.
type Role string

const (
	RoleInstance Role = "instance"
	RoleSeed     Role = "seed"
	RoleCopier   Role = "copier"
	RoleTool     Role = "tool"
	// RoleOwner marks a connection's owner Lease (OwnerLease).
	RoleOwner Role = "owner"
)

// Base returns the labels every object of one connection carries: managed-by,
// owner and cluster. Callers add role, fleet and key labels to the map.
func Base(owner, clusterID string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedBy,
		LabelOwner:     owner,
		LabelCluster:   clusterID,
	}
}

// Selector is the label selector, in the API's string form, that matches
// every object of one connection and nothing else.
func Selector(owner, clusterID string) string {
	return LabelManagedBy + "=" + ManagedBy + "," + LabelOwner + "=" + owner + "," + LabelCluster + "=" + clusterID
}

// --- hashes ---

// HashLen is the length of InstanceKey and FleetHash values.
const HashLen = 8

// OwnerLen is the length of an Owner value.
const OwnerLen = 12

// hashEncoding spells hashes in lowercase RFC 4648 base32 (a-z, 2-7): every
// character is valid in a DNS label, and 8 characters carry 40 bits.
var hashEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// hash8 hashes a domain tag and fields, each length-prefixed so no two field
// lists hash the same input, to HashLen characters.
func hash8(domain string, fields ...string) string {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, f := range append([]string{domain}, fields...) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(f)))])
		h.Write([]byte(f))
	}
	return hashEncoding.EncodeToString(h.Sum(nil))[:HashLen]
}

// InstanceKey is an instance's key8: the hash of the daemon, fleet, instance
// and creation time, persisted as Instance.KubeKey and carried by every object
// of the instance (LabelKey). The creation time makes a re-created instance of
// the same name a new key, so it never adopts its predecessor's leftovers.
func InstanceKey(daemonID, fleet, instance string, createdAt time.Time) string {
	return hash8("fleet.dev/key", daemonID, fleet, instance, createdAt.UTC().Format(time.RFC3339Nano))
}

// FleetHash is a fleet's h8 on one connection: the hash of the daemon, the
// KubeCluster record id and the fleet. The record id keeps the per-fleet
// objects of two connections to one namespace apart.
func FleetHash(daemonID, clusterID, fleet string) string {
	return hash8("fleet.dev/fleet", daemonID, clusterID, fleet)
}

// ValidHash reports whether s has the shape of an InstanceKey or FleetHash.
func ValidHash(s string) bool {
	if len(s) != HashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHashChar(s[i]) {
			return false
		}
	}
	return true
}

func isHashChar(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '2' && c <= '7') }

// Owner is the owner label value of a daemon: the first OwnerLen characters
// of its daemon id, lowercased.
func Owner(daemonID string) string {
	id := strings.ToLower(daemonID)
	return id[:min(OwnerLen, len(id))]
}

// --- object names ---

// MaxInstanceName is the longest StatefulSet name: its pods carry a
// controller-revision-hash label of the name plus 11 characters, and a label
// value holds at most 63.
const MaxInstanceName = 52

const (
	instancePrefix  = "fi-"
	maxFleetSlug    = 16
	maxInstanceSlug = 24
)

// Instance is the name of an instance's StatefulSet and PVC:
// "fi-<fleet>-<instance>-<key8>", at most MaxInstanceName characters. The
// fleet part is at most 16 characters and the instance part at most 24, cut
// shorter when both are long; the key keeps the name unique. key must be an
// InstanceKey.
func Instance(fleet, instance, key string) string {
	f := Slug(fleet, maxFleetSlug)
	room := MaxInstanceName - len(instancePrefix) - len(f) - len(key) - 2
	return instancePrefix + f + "-" + Slug(instance, min(maxInstanceSlug, room)) + "-" + key
}

// Pod is the name of the one pod of an instance's StatefulSet.
func Pod(statefulSet string) string { return statefulSet + "-0" }

// MaxGeneration is the highest root generation (rebuild-gen) an instance's
// generation-named objects take: "<sts>-seed-g9999" is the longest Job name
// whose pods' job-name label still fits a label value.
const MaxGeneration = 9999

// ValidGeneration reports whether gen is a root generation the
// generation-named objects can carry: 1 to MaxGeneration.
func ValidGeneration(gen int) bool { return gen >= 1 && gen <= MaxGeneration }

// SpecSecret is the name of the Secret holding generation gen's spec files
// (pid1.json, exec.json, ...): "<sts>-spec-g<gen>". Each generation has its
// own, so a failed rebuild re-applies the previous one.
func SpecSecret(statefulSet string, gen int) string {
	return statefulSet + "-spec-g" + strconv.Itoa(gen)
}

// EnvSecret is the name of the Secret holding generation gen's container
// environment values: "<sts>-env-g<gen>".
func EnvSecret(statefulSet string, gen int) string {
	return statefulSet + "-env-g" + strconv.Itoa(gen)
}

// SeedJob is the name of the Job that seeds generation gen of an instance's
// root: "<sts>-seed-g<gen>", at most 63 characters for a valid gen.
func SeedJob(statefulSet string, gen int) string {
	return statefulSet + "-seed-g" + strconv.Itoa(gen)
}

// The suffixes of the per-(fleet, connection) objects (FleetObject).
const (
	SuffixAnchor          = "anchor"
	SuffixShare           = "share"
	SuffixNet             = "net"
	SuffixAptCache        = "aptcache"
	SuffixImageCache      = "imgcache"
	SuffixBuildkit        = "buildkit"
	SuffixInstancesPolicy = "instances"
	SuffixCachesPolicy    = "caches"
	SuffixEgressPolicy    = "egress"
)

const (
	fleetPrefix        = "fs-"
	maxFleetObjectSlug = 24
	// MaxFleetObjectSuffix is the longest suffix FleetObject takes, so every
	// fleet object name is an RFC 1035 label (a Service name).
	MaxFleetObjectSuffix = 63 - len(fleetPrefix) - maxFleetObjectSlug - HashLen - 2
)

// FleetObject is the name of one of a fleet's objects on a connection:
// "fs-<fleet>-<h8>-<suffix>", where h8 is the FleetHash and suffix one of the
// Suffix constants (or another RFC 1035 label of at most
// MaxFleetObjectSuffix characters).
func FleetObject(fleet, h8, suffix string) string {
	return fleetPrefix + Slug(fleet, maxFleetObjectSlug) + "-" + h8 + "-" + suffix
}

// OwnerLease is the name of a connection's owner Lease, which holds the
// daemon incarnation that may adopt and garbage-collect the connection's
// objects: "fleet-owner-<owner12>-<cluster id>". It names the connection as
// well as the daemon, so two connections of one daemon to the same namespace
// each have their own.
func OwnerLease(daemonID, clusterID string) string {
	return "fleet-owner-" + Owner(daemonID) + "-" + clusterID
}

// Slug reduces s to at most limit (at least 1) characters of [a-z0-9-] that start and end
// with a letter or digit: ASCII letters are lowercased, every other run of
// characters becomes one "-". A name with no letter or digit at all becomes
// "x", so the result is never empty.
func Slug(s string, limit int) string {
	limit = max(limit, 1)
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		default:
			dash = b.Len() > 0
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > limit {
		out = strings.TrimRight(out[:limit], "-")
	}
	if out == "" {
		return "x"
	}
	return out
}

// --- connection ids ---

// MaxClusterID is the longest KubeCluster record id.
const MaxClusterID = 32

const (
	clusterIDSlug   = 20
	clusterIDRandom = 6
)

// reservedClusterIDs would collide with other entries of ~/.fleet/kube, where
// each connection has a directory named by its id.
var reservedClusterIDs = map[string]bool{"jobs": true}

// NewClusterID returns a new KubeCluster record id: a slug of hint (the
// context name, say) and a random suffix, "<slug>-<6 random>", which taken
// does not report as in use. taken may be nil.
func NewClusterID(hint string, taken func(string) bool) string {
	base := Slug(hint, clusterIDSlug)
	switch {
	case !strings.ContainsFunc(hint, isAlnumRune):
		base = "cluster"
	case base[0] < 'a' || base[0] > 'z':
		base = Slug("k-"+base, clusterIDSlug)
	}
	for {
		id := base + "-" + randomHash(clusterIDRandom)
		if taken == nil || !taken(id) {
			return id
		}
	}
}

func randomHash(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hashEncoding.EncodeToString(b)[:n]
}

// ValidateClusterID reports why id cannot be a KubeCluster record id. A valid
// id is an RFC 1035 label of at most MaxClusterID characters, so it is a valid
// label value, a valid name part and a single path segment.
func ValidateClusterID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("cluster id must not be empty")
	case len(id) > MaxClusterID:
		return fmt.Errorf("cluster id %q is longer than %d characters", id, MaxClusterID)
	case !IsDNS1035Label(id):
		return fmt.Errorf("cluster id %q must be lowercase letters, digits and '-', start with a letter and end with a letter or digit", id)
	case reservedClusterIDs[id]:
		return fmt.Errorf("cluster id %q is reserved", id)
	}
	return nil
}

// --- grammar ---

// IsDNS1123Label reports whether s is an RFC 1123 label: at most 63 of
// [a-z0-9-], starting and ending with a letter or digit.
func IsDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 || !isAlnum(s[0]) || !isAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// IsDNS1035Label reports whether s is an RFC 1035 label: an RFC 1123 label
// that starts with a letter.
func IsDNS1035Label(s string) bool {
	return IsDNS1123Label(s) && s[0] >= 'a' && s[0] <= 'z'
}

// IsLabelValue reports whether s is a valid label value: empty, or at most 63
// of [A-Za-z0-9-_.], starting and ending with a letter or digit.
func IsLabelValue(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 63 || !isAlnumAnyCase(s[0]) || !isAlnumAnyCase(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isAlnumAnyCase(c) && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

func isAlnumAnyCase(c byte) bool { return isAlnum(c) || (c >= 'A' && c <= 'Z') }

func isAlnumRune(r rune) bool { return r < 0x80 && isAlnumAnyCase(byte(r)) }
