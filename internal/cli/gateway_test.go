package cli

import (
	"strings"
	"testing"
)

// flagValue parses args against a fresh gateway command, applies the
// FLEET_GATEWAY_* environment, and returns the named flag's final value.
func gatewayFlagAfterEnv(t *testing.T, args []string, name string) string {
	t.Helper()
	cmd := newGatewayCmd()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	if err := applyGatewayEnv(cmd.Flags()); err != nil {
		t.Fatalf("applyGatewayEnv: %v", err)
	}
	return cmd.Flags().Lookup(name).Value.String()
}

func TestGatewayEnvFillsUnsetFlags(t *testing.T) {
	t.Setenv("FLEET_GATEWAY_PUBLIC_URL", "https://gw.example.com")
	t.Setenv("FLEET_GATEWAY_MAX_SESSIONS", "42")
	t.Setenv("FLEET_GATEWAY_SESSION_KEY", "s3cret")

	if got := gatewayFlagAfterEnv(t, nil, "public-url"); got != "https://gw.example.com" {
		t.Errorf("public-url = %q, want env value", got)
	}
	if got := gatewayFlagAfterEnv(t, nil, "max-sessions"); got != "42" {
		t.Errorf("max-sessions = %q, want 42", got)
	}
	if got := gatewayFlagAfterEnv(t, nil, "session-key"); got != "s3cret" {
		t.Errorf("session-key = %q, want env value", got)
	}
}

func TestGatewayEnvFillsPublicGRPCURL(t *testing.T) {
	t.Setenv("FLEET_GATEWAY_PUBLIC_GRPC_URL", "https://gw.example.com:50051")

	if got := gatewayFlagAfterEnv(t, nil, "public-grpc-url"); got != "https://gw.example.com:50051" {
		t.Errorf("public-grpc-url = %q, want env value", got)
	}
}

func TestGatewayFlagWinsOverEnv(t *testing.T) {
	t.Setenv("FLEET_GATEWAY_PUBLIC_URL", "https://from-env.example.com")

	got := gatewayFlagAfterEnv(t, []string{"--public-url", "https://from-flag.example.com"}, "public-url")
	if got != "https://from-flag.example.com" {
		t.Errorf("public-url = %q, want flag value to win over env", got)
	}
}

func TestGatewayEnvEmptyValueApplies(t *testing.T) {
	// An empty env value is still "set" — it must be able to clear a defaulted
	// flag (an empty --grpc-addr disables the gRPC listener).
	t.Setenv("FLEET_GATEWAY_GRPC_ADDR", "")

	if got := gatewayFlagAfterEnv(t, nil, "grpc-addr"); got != "" {
		t.Errorf("grpc-addr = %q, want empty from env", got)
	}
}

func TestGatewayEnvBadValueNamesVariable(t *testing.T) {
	t.Setenv("FLEET_GATEWAY_MAX_SESSIONS", "not-a-number")

	cmd := newGatewayCmd()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	err := applyGatewayEnv(cmd.Flags())
	if err == nil {
		t.Fatal("applyGatewayEnv: want error for non-numeric FLEET_GATEWAY_MAX_SESSIONS")
	}
	if !strings.Contains(err.Error(), "FLEET_GATEWAY_MAX_SESSIONS") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}
