//go:build !nok8s

package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// kube_validate.go holds the rules every KubeCluster record keeps, however
// it was written (AddKubeCluster, any FieldMask of UpdateKubeCluster), and
// the FieldMask application itself. Errors are INVALID_ARGUMENT naming the
// field, so a remote CLI or TUI shows exactly what to fix.

// daemonOwnedPaths are the KubeCluster fields only the daemon writes.
var daemonOwnedPaths = map[string]bool{
	"id": true, "server": true, "cluster_uid": true,
	"created_at": true, "updated_at": true, "verified_at": true, "revision": true,
}

func invalidf(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// validateKubeCluster checks a record after its defaults are applied (a
// namespace and kubeconfig paths are set).
func validateKubeCluster(c *fleet.KubeCluster) error {
	if strings.TrimSpace(c.Context) == "" {
		return invalidf("context is required")
	}
	if err := checkText("display_name", c.DisplayName, 63); err != nil {
		return err
	}
	if err := checkText("context", c.Context, 253); err != nil {
		return err
	}
	if len(c.KubeconfigPaths) == 0 {
		return invalidf("kubeconfig_paths: at least one kubeconfig file is required")
	}
	for _, p := range c.KubeconfigPaths {
		if err := checkAbsPath(p); err != nil {
			return invalidf("kubeconfig_paths: %v", err)
		}
	}
	if !names.IsDNS1123Label(c.Namespace) {
		return invalidf("namespace %q is not a valid namespace name (lower-case letters, digits and '-', at most 63)", c.Namespace)
	}
	if err := fleet.ValidateExtraPath(c.ExtraPath); err != nil {
		return invalidf("extra_path: %v", err)
	}
	if err := fleet.ValidatePluginEnv(c.PluginEnv); err != nil {
		return invalidf("plugin_env: %v", err)
	}
	for field, name := range map[string]string{
		"storage_class": c.StorageClass, "snapshot_class": c.SnapshotClass, "shared_storage_class": c.SharedStorageClass,
		"cache_storage_class": c.CacheStorageClass, "runtime_class": c.RuntimeClass, "service_account": c.ServiceAccount,
	} {
		if name != "" && len(validation.IsDNS1123Subdomain(name)) > 0 {
			return invalidf("%s %q is not a valid object name", field, name)
		}
	}
	for field, q := range map[string]string{
		"disk_size": c.DiskSize, "share_size": c.ShareSize, "memory_limit": c.MemoryLimit, "max_total_disk": c.MaxTotalDisk,
		"resources.cpu_request": c.Resources.CPURequest, "resources.memory_request": c.Resources.MemoryRequest,
		"resources.autopilot_memory": c.Resources.AutopilotMemory,
		"cache_sizes.buildkit":       c.CacheSizes.Buildkit, "cache_sizes.debcache": c.CacheSizes.DebCache, "cache_sizes.imagecache": c.CacheSizes.ImageCache,
	} {
		if err := checkQuantity(field, q); err != nil {
			return err
		}
	}
	for field, ok := range map[string]bool{
		"clone_method":       c.CloneMethod.Valid(),
		"clone_consistency":  c.CloneConsistency.Valid(),
		"share_retention":    c.ShareRetention.Valid(),
		"credential_scope":   c.CredentialScope.Valid(),
		"debug_capabilities": c.DebugCapabilities.Valid(),
		"host_gateway":       c.HostGateway.Valid(),
	} {
		if !ok {
			return invalidf("%s: unknown value", field)
		}
	}
	for key, value := range c.NodeSelector {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return invalidf("node_selector key %q: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return invalidf("node_selector value %q: %s", value, strings.Join(errs, "; "))
		}
	}
	for i, t := range c.Tolerations {
		if err := checkToleration(t); err != nil {
			return invalidf("tolerations[%d]: %v", i, err)
		}
	}
	if err := checkPodOverrides(c.PodOverridesYAML); err != nil {
		return err
	}
	if c.MaxInstances < 0 {
		return invalidf("max_instances cannot be negative")
	}
	if c.AutomationMaxPerAgent != nil && *c.AutomationMaxPerAgent < 0 {
		return invalidf("automation_max_per_agent cannot be negative")
	}
	if c.AutomationFireInterval != "" && c.AutomationFireInterval != "0" {
		if d, err := time.ParseDuration(c.AutomationFireInterval); err != nil || d < 0 {
			return invalidf("automation_fire_interval %q is not a duration (\"30s\", or \"0\" for no limit)", c.AutomationFireInterval)
		}
	}
	return validateKubeImage(c.Image)
}

// validateKubeImage holds the image settings to the delivery strategies'
// rules (kubeimage.ValidateSettings) and checks the build platform.
func validateKubeImage(img fleet.KubeImageSettings) error {
	if err := kubeimage.ValidateSettings(img); err != nil {
		return invalidf("image: %v", err)
	}
	if img.Platform != "" && !platformPattern.MatchString(img.Platform) {
		return invalidf("image.platform %q is not os/arch[/variant] (\"linux/arm64\")", img.Platform)
	}
	return nil
}

var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9_]+(/[a-z0-9]+)?$`)

func checkText(field, s string, max int) error {
	if len(s) > max {
		return invalidf("%s is longer than %d bytes", field, max)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return invalidf("%s contains a control character", field)
		}
	}
	return nil
}

func checkAbsPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("an empty path")
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%q contains a NUL byte", p)
	case strings.HasPrefix(p, "~"):
		return fmt.Errorf("%q: use an absolute path ('~' is not expanded on the daemon host)", p)
	case !filepath.IsAbs(p):
		return fmt.Errorf("%q is not an absolute path on the daemon host", p)
	}
	return nil
}

func checkQuantity(field, s string) error {
	if s == "" {
		return nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return invalidf("%s %q is not a quantity (\"50Gi\", \"500m\")", field, s)
	}
	if q.Sign() <= 0 {
		return invalidf("%s must be more than zero", field)
	}
	return nil
}

func checkToleration(t fleet.KubeToleration) error {
	switch corev1.TolerationOperator(t.Operator) {
	case "", corev1.TolerationOpEqual:
		if t.Key == "" {
			return fmt.Errorf("an empty key needs operator Exists")
		}
	case corev1.TolerationOpExists:
		if t.Value != "" {
			return fmt.Errorf("operator Exists takes no value")
		}
	default:
		return fmt.Errorf("operator %q is not Equal or Exists", t.Operator)
	}
	if t.Key != "" {
		if errs := validation.IsQualifiedName(t.Key); len(errs) > 0 {
			return fmt.Errorf("key %q: %s", t.Key, strings.Join(errs, "; "))
		}
	}
	if errs := validation.IsValidLabelValue(t.Value); len(errs) > 0 {
		return fmt.Errorf("value %q: %s", t.Value, strings.Join(errs, "; "))
	}
	switch corev1.TaintEffect(t.Effect) {
	case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		return fmt.Errorf("effect %q is not NoSchedule, PreferNoSchedule or NoExecute", t.Effect)
	}
	if t.TolerationSeconds != nil && corev1.TaintEffect(t.Effect) != corev1.TaintEffectNoExecute {
		return fmt.Errorf("toleration_seconds needs effect NoExecute")
	}
	return nil
}

// checkPodOverrides accepts YAML (or JSON) that is a mapping, the shape a pod
// spec fragment has. Which fields it may set is decided where it is merged
// into the rendered pod.
func checkPodOverrides(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	data, err := yaml.YAMLToJSON([]byte(s))
	if err != nil {
		return invalidf("pod_overrides_yaml does not parse: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return invalidf("pod_overrides_yaml must be a mapping of pod spec fields")
	}
	return nil
}

// --- uniqueness ---

// kubeTupleKey is a record's (kubeconfig paths, context, namespace): two
// records may not share it, or they would be one connection twice.
func kubeTupleKey(c *fleet.KubeCluster) string {
	paths := make([]string, len(c.KubeconfigPaths))
	for i, p := range c.KubeconfigPaths {
		paths[i] = filepath.Clean(p)
	}
	return strings.Join(paths, "\x00") + "\x01" + c.Context + "\x01" + c.Namespace
}

// checkUnique refuses c when another record (any but c.ID) has its display
// name or its (paths, context, namespace).
func checkUnique(c *fleet.KubeCluster, all []fleet.KubeCluster) error {
	name := strings.ToLower(c.GetDisplayName())
	key := kubeTupleKey(c)
	for i := range all {
		other := &all[i]
		if other.ID == c.ID {
			continue
		}
		if c.DisplayName != "" && strings.ToLower(other.GetDisplayName()) == name {
			return status.Errorf(codes.AlreadyExists, "a Kubernetes connection is already named %q", c.GetDisplayName())
		}
		if kubeTupleKey(other) == key {
			return status.Errorf(codes.AlreadyExists, "connection %q already uses context %q in namespace %q of those kubeconfig files", other.GetDisplayName(), c.Context, c.Namespace)
		}
	}
	return nil
}

// --- FieldMask ---

// applyKubeMask returns cur with the fields mask names replaced by src's.
// Paths follow the proto field names; a message path replaces the whole
// message, a list or map path the whole collection, and an unset value in
// src clears the field.
func applyKubeMask(cur fleet.KubeCluster, src *fleetgrpc.KubeCluster, mask *fieldmaskpb.FieldMask) (fleet.KubeCluster, error) {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return cur, invalidf("update_mask is required: name the fields to change")
	}
	for _, p := range mask.GetPaths() {
		if daemonOwnedPaths[strings.SplitN(p, ".", 2)[0]] {
			return cur, invalidf("%s is set by the daemon, not by clients", p)
		}
	}
	if !mask.IsValid(&fleetgrpc.KubeCluster{}) {
		return cur, invalidf("update_mask %v names a field KubeCluster does not have", mask.GetPaths())
	}
	if src == nil {
		src = &fleetgrpc.KubeCluster{}
	}
	src = proto.Clone(src).(*fleetgrpc.KubeCluster)
	dst := protoconv.KubeClusterToProto(cur)
	for _, p := range mask.GetPaths() {
		if err := copyMaskPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(p, ".")); err != nil {
			return cur, invalidf("update_mask %s: %v", p, err)
		}
	}
	next := protoconv.KubeClusterFromProto(dst)
	// Never on the wire, so never in the mask: the stored value stays.
	next.Image.GatewayCredential = cur.Image.GatewayCredential
	return next, nil
}

func copyMaskPath(dst, src protoreflect.Message, path []string) error {
	fd := dst.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if fd == nil {
		return fmt.Errorf("no field %q", path[0])
	}
	if len(path) > 1 {
		if fd.Message() == nil || fd.IsList() || fd.IsMap() {
			return fmt.Errorf("%q has no sub-fields", path[0])
		}
		return copyMaskPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
	}
	if src.Has(fd) {
		dst.Set(fd, src.Get(fd))
	} else {
		dst.Clear(fd)
	}
	return nil
}
