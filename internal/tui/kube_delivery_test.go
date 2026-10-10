package tui

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

func deliveryStatus(mode string, del *fleetgrpc.KubeImageDelivery) *fleetgrpc.KubeClusterStatus {
	st := &fleetgrpc.KubeClusterStatus{Cluster: &fleetgrpc.KubeCluster{Id: "prod-eu", Image: &fleetgrpc.KubeImageSettings{Mode: mode}}}
	if del != nil {
		st.Capabilities = &fleetgrpc.KubeCapabilities{ImageDelivery: del}
	}
	return st
}

func optionLabels(opts []kubeDeliveryOption) string {
	var parts []string
	for _, o := range opts {
		p := o.Label
		if o.Selected {
			p = "*" + p
		}
		if o.Disabled {
			p += "(off: " + o.Reason + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

// The Delivery selector labels Auto with what it resolved to, marks the
// setting, and greys Gateway out with the reason while the gateway serves no
// images; Node load appears only for a connection set to it.
func TestKubeDeliveryOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *fleetgrpc.KubeClusterStatus
		want string
	}{
		{"auto resolved to the gateway", deliveryStatus("", &fleetgrpc.KubeImageDelivery{Mode: "gateway", GatewayRegistry: true}), "*Auto→Gateway Registry Gateway"},
		{"auto spelled out", deliveryStatus("auto", &fleetgrpc.KubeImageDelivery{Mode: "node-load", GatewayRegistry: true}), "*Auto→Node load Registry Gateway"},
		{"no gateway", deliveryStatus("registry", &fleetgrpc.KubeImageDelivery{Mode: "registry", GatewayReason: "Enable Image Registry is off"}),
			"Auto *Registry Gateway(off: Enable Image Registry is off)"},
		{"set to gateway, gateway gone", deliveryStatus("gateway", &fleetgrpc.KubeImageDelivery{Mode: "none", GatewayReason: "the gateway does not offer image delivery: no --session-key"}),
			"Auto Registry *Gateway(off: the gateway does not offer image delivery: no --session-key)"},
		{"node load set", deliveryStatus("node-load", &fleetgrpc.KubeImageDelivery{Mode: "node-load", GatewayRegistry: true}), "Auto *Node load Registry Gateway"},
		{"not checked yet", deliveryStatus("", nil), "*Auto Registry Gateway"},
	} {
		if got := optionLabels(kubeDeliveryOptions(tc.st)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// ←/→ move between the choices and skip a greyed-out Gateway.
func TestKubeDeliveryNext(t *testing.T) {
	off := deliveryStatus("", &fleetgrpc.KubeImageDelivery{Mode: "none", GatewayReason: "fleetd has no gateway"})
	if mode, ok := kubeDeliveryNext(off, 1); !ok || mode != "registry" {
		t.Fatalf("right from Auto: %q %v", mode, ok)
	}
	reg := deliveryStatus("registry", off.GetCapabilities().GetImageDelivery())
	if _, ok := kubeDeliveryNext(reg, 1); ok {
		t.Fatal("right from Registry must not land on a disabled Gateway")
	}
	if mode, ok := kubeDeliveryNext(reg, -1); !ok || mode != "" {
		t.Fatalf("left from Registry: %q %v", mode, ok)
	}
	on := deliveryStatus("registry", &fleetgrpc.KubeImageDelivery{Mode: "registry", GatewayRegistry: true})
	if mode, ok := kubeDeliveryNext(on, 1); !ok || mode != "gateway" {
		t.Fatalf("right from Registry with a gateway: %q %v", mode, ok)
	}
}

// The lines under the selector say what Auto picked, why Gateway is off, the
// last pull through the gateway (size, time, host, pace), the cache size, and
// a registry that keeps images past retention.
func TestKubeDeliveryLines(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	gw := deliveryStatus("", &fleetgrpc.KubeImageDelivery{
		Mode: "gateway", Reason: "no registry set; the gateway serves images", GatewayRegistry: true, GatewayHost: "gw.example.com",
		LastPull:          &fleetgrpc.KubeImagePull{Manifest: "sha256:abc", StartedAt: timestamppb.New(start), LastByteAt: timestamppb.New(start.Add(38 * time.Second)), Bytes: 412 << 20},
		CacheBytes:        3 << 30,
		UploadPacePercent: 80, UplinkBytesPerSecond: 1_250_000,
	})
	out := strings.Join(kubeDeliveryLines(gw), "\n")
	for _, want := range []string{
		"Auto picked Gateway: no registry set; the gateway serves images",
		"last pull 412 MB in 38s via gw.example.com · paced 80% of 10.0 Mbit/s",
		"gateway image cache 3.0 GB",
		"serializeImagePulls",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gateway lines miss %q:\n%s", want, out)
		}
	}

	fresh := deliveryStatus("gateway", &fleetgrpc.KubeImageDelivery{Mode: "gateway", GatewayRegistry: true, GatewayHost: "gw.example.com", GatewayFailedStep: "tls"})
	out = strings.Join(kubeDeliveryLines(fresh), "\n")
	if !strings.Contains(out, "no pull through the gateway yet via gw.example.com · unpaced") || !strings.Contains(out, "failed at the tls step") || strings.Contains(out, "Auto picked") {
		t.Errorf("a fresh explicit gateway:\n%s", out)
	}

	none := deliveryStatus("", &fleetgrpc.KubeImageDelivery{Mode: "none", Reason: "a built image cannot reach this cluster: no registry is set", GatewayReason: "Enable Image Registry is off"})
	out = strings.Join(kubeDeliveryLines(none), "\n")
	if !strings.Contains(out, "a built image cannot reach this cluster") || !strings.Contains(out, "Gateway unavailable: Enable Image Registry is off") {
		t.Errorf("no delivery:\n%s", out)
	}

	keeps := deliveryStatus("registry", &fleetgrpc.KubeImageDelivery{Mode: "registry", RegistryReachable: true, GatewayRegistry: true,
		Reason: "registry.example.com does not allow deleting images (HTTP 405): images past retention stay in the registry"})
	if out := strings.Join(kubeDeliveryLines(keeps), "\n"); !strings.Contains(out, "images past retention stay in the registry") {
		t.Errorf("a registry without DELETE:\n%s", out)
	}
	if out := strings.Join(kubeDeliveryLines(deliveryStatus("", nil)), "\n"); !strings.Contains(out, "once the connection has been checked") {
		t.Errorf("unchecked:\n%s", out)
	}
}

func TestKubeDeliverySelectorRendersEveryChoice(t *testing.T) {
	out := kubeDeliverySelector(kubeDeliveryOptions(deliveryStatus("", &fleetgrpc.KubeImageDelivery{Mode: "gateway", GatewayRegistry: true})))
	for _, want := range []string{"[Auto→Gateway]", "[Registry]", "[Gateway]"} {
		if !strings.Contains(out, want) {
			t.Errorf("selector %q misses %s", out, want)
		}
	}
}
