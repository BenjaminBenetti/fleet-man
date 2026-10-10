package backendtest

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSelections(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []Selection
		err   string
	}{
		{value: "", want: nil},
		{value: "  ", want: nil},
		{value: "devcontainer", want: []Selection{{Backend: "devcontainer"}}},
		{value: "devcontainer,kubernetes:minikube", want: []Selection{{Backend: "devcontainer"}, {Backend: "kubernetes", Qualifier: "minikube"}}},
		{value: " kubernetes , devcontainer ", want: []Selection{{Backend: "kubernetes"}, {Backend: "devcontainer"}}},
		{value: "kubernetes:kind-fleet,kubernetes:minikube", want: []Selection{{Backend: "kubernetes", Qualifier: "kind-fleet"}, {Backend: "kubernetes", Qualifier: "minikube"}}},
		{value: "kubernetes:arn:aws:eks:us-east-1:1:cluster/x", want: []Selection{{Backend: "kubernetes", Qualifier: "arn:aws:eks:us-east-1:1:cluster/x"}}},
		{value: "devcontianer", err: `unknown backend "devcontianer"`},
		{value: "devcontainer:x", err: "devcontainer takes no qualifier"},
		{value: "kubernetes:", err: "empty kube context"},
		{value: "devcontainer,,kubernetes", err: "empty entry"},
		{value: "devcontainer,devcontainer", err: "listed twice"},
		{value: "codespaces", err: "unknown backend"},
	} {
		got, err := ParseSelections(tc.value)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseSelections(%q) error = %v, want one containing %q", tc.value, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseSelections(%q) = %v, %v; want %v", tc.value, got, err, tc.want)
		}
	}
}

func TestSelectionString(t *testing.T) {
	if got := (Selection{Backend: "kubernetes", Qualifier: "minikube"}).String(); got != "kubernetes:minikube" {
		t.Errorf("String() = %q", got)
	}
	if got := (Selection{Backend: "devcontainer"}).String(); got != "devcontainer" {
		t.Errorf("String() = %q", got)
	}
}

func TestSelectedReadsTheEnvironment(t *testing.T) {
	t.Setenv(EnvVar, "devcontainer")
	got, err := Selected()
	if err != nil || len(got) != 1 || got[0].Backend != "devcontainer" {
		t.Errorf("Selected() = %v, %v", got, err)
	}
}
