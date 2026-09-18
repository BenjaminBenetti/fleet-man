package fleet

import "testing"

func TestValidateFleetName(t *testing.T) {
	for _, bad := range []string{"", "a b", "a/b", `a\b`, ".", "..", "\tx"} {
		if err := ValidateFleetName(bad); err == nil {
			t.Errorf("ValidateFleetName(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"scratch", "my-proj", "proj_2", "a.b"} {
		if err := ValidateFleetName(good); err != nil {
			t.Errorf("ValidateFleetName(%q) = %v, want nil", good, err)
		}
	}
}
