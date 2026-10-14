package kubeerr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		msg    string
		code   Code
		detail string
		ok     bool
	}{
		{"kubernetes:interrupted: interrupted by daemon restart during create", Interrupted, "interrupted by daemon restart during create", true},
		{"kubernetes:unsupported:compose: compose devcontainers are not supported", Unsupported.Narrow("compose"), "compose devcontainers are not supported", true},
		{"kubernetes:unsupported:flag:--link: --link is not supported\nsecond line", "kubernetes:unsupported:flag:--link", "--link is not supported", true},
		{"kubernetes:tampered", Tampered, "", true},
		{"  kubernetes:oomkilled: out of memory 3 times", OOMKilled, "out of memory 3 times", true},
		{"kubernetes:nonsense: something", "", "", false},
		{"cancelled: instance destroyed", "", "", false},
		{"", "", "", false},
		// A kube context named like a code is no code.
		{"kubernetes:arn:aws:eks:us-east-1: context", "", "", false},
	} {
		code, detail, ok := Parse(tc.msg)
		if code != tc.code || detail != tc.detail || ok != tc.ok {
			t.Errorf("Parse(%q) = %q, %q, %v; want %q, %q, %v", tc.msg, code, detail, ok, tc.code, tc.detail, tc.ok)
		}
	}
}

func TestClass(t *testing.T) {
	for code, want := range map[Code]Code{
		Unsupported.Narrow("flag").Narrow("--link"):  Unsupported,
		AdmissionDenied.Narrow("debug_capabilities"): AdmissionDenied,
		PVCPending:           PVCPending,
		"kubernetes:made_up": "kubernetes:made_up",
	} {
		if got := code.Class(); got != want {
			t.Errorf("%s.Class() = %s, want %s", code, got, want)
		}
	}
	if !Unsupported.Narrow("compose").Is(Unsupported) || Unsupported.Is(Quota) {
		t.Error("Is does not compare classes")
	}
	if Code("kubernetes:made_up").Valid() || !Timeout.Valid() {
		t.Error("Valid does not know the classes")
	}
}

// coded is an error of another package, carrying its code.
type coded struct{ code, msg string }

func (c coded) Error() string { return c.code + ": " + c.msg }
func (c coded) Code() string  { return c.code }

func TestOf(t *testing.T) {
	deep := fmt.Errorf("creating: %w", fmt.Errorf("waiting: %w", coded{"kubernetes:pvc_pending", "the disk did not bind"}))
	for name, tc := range map[string]struct {
		err  error
		code Code
		ok   bool
	}{
		"own error":       {New(Quota, "no room"), Quota, true},
		"wrapped coded":   {deep, PVCPending, true},
		"joined":          {errors.Join(errors.New("plain"), coded{"kubernetes:unsupported:mount", "x"}), "kubernetes:unsupported:mount", true},
		"message only":    {errors.New("kubernetes:tampered: pod x is not the pod fleet made"), Tampered, true},
		"plain":           {errors.New("exit status 1"), "", false},
		"nil":             {nil, "", false},
		"other Code()":    {coded{"E42", "not ours"}, "", false},
		"wrapped message": {fmt.Errorf("start: %w", errors.New("kubernetes:timeout: the pod did not start")), Timeout, true},
	} {
		code, ok := Of(tc.err)
		if code != tc.code || ok != tc.ok {
			t.Errorf("%s: Of = %q, %v; want %q, %v", name, code, ok, tc.code, tc.ok)
		}
	}
}

func TestWrapKeepsTheMostSpecificCode(t *testing.T) {
	inner := coded{"kubernetes:image_pull", "the image did not pull"}
	if got := Wrap(Timeout, inner); got != error(inner) {
		t.Errorf("Wrap re-labelled a coded error: %v", got)
	}
	plain := errors.New("connection refused")
	err := Wrap(Unreachable, plain)
	if code, _ := Of(err); code != Unreachable || !errors.Is(err, plain) {
		t.Errorf("Wrap(%v) = %v (%s)", plain, err, code)
	}
	if !strings.HasPrefix(err.Error(), string(Unreachable)+": ") {
		t.Errorf("a wrapped error does not start with its code: %q", err)
	}
	if Wrap(Quota, nil) != nil {
		t.Error("Wrap(nil) is not nil")
	}
	hinted := WithHint(OOMKilled, errors.New("out of memory"), "raise memory_limit")
	if hinted.Error() != "kubernetes:oomkilled: out of memory (raise memory_limit)" {
		t.Errorf("WithHint = %q", hinted)
	}
}

func TestEveryClassHasAHint(t *testing.T) {
	for _, c := range Classes() {
		if Hint(c) == "" {
			t.Errorf("%s has no hint", c)
		}
		if !strings.HasPrefix(string(c), Prefix) || strings.Count(string(c), ":") != 1 {
			t.Errorf("class %q is not a plain kubernetes: code", c)
		}
	}
	if Hint(Unsupported.Narrow("compose")) != Hint(Unsupported) {
		t.Error("a narrowed code does not get its class's hint")
	}
}
