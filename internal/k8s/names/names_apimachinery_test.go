//go:build !nok8s

package names

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The API server's own validators agree with ours on every value the tests
// and FuzzK8sName generate.
func init() {
	crossCheck = func(t *testing.T, kind, value string) {
		t.Helper()
		ours := map[string]bool{
			"statefulset": IsDNS1123Label(value) && len(value) <= MaxInstanceName,
			"pod":         IsDNS1123Label(value),
			"job":         IsDNS1123Label(value) && IsLabelValue(value),
			"service":     IsDNS1035Label(value),
			"label value": IsLabelValue(value),
		}
		theirs := map[string]bool{
			"statefulset": len(validation.IsDNS1123Label(value)) == 0 && len(value) <= MaxInstanceName,
			"pod":         len(validation.IsDNS1123Label(value)) == 0,
			"job":         len(validation.IsDNS1123Label(value)) == 0 && len(validation.IsValidLabelValue(value)) == 0,
			"service":     len(validation.IsDNS1035Label(value)) == 0,
			"label value": len(validation.IsValidLabelValue(value)) == 0,
		}
		if kind == "grammar" {
			for _, k := range []string{"pod", "service", "label value"} {
				if ours[k] != theirs[k] {
					t.Errorf("%s(%q): ours %v, apimachinery %v", k, value, ours[k], theirs[k])
				}
			}
			return
		}
		if !theirs[kind] {
			t.Errorf("apimachinery rejects %s %q", kind, value)
		}
	}
}
