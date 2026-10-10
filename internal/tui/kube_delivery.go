package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// kube_delivery.go is the cluster editor's Image group: the Delivery
// selector ([Auto→Gateway] [Registry] [Gateway]) with Gateway greyed out,
// and the reason shown, while the gateway serves no images for this daemon,
// and the lines under it: what Auto picked and why, the last pull through the
// gateway with its size, time and pace, the image cache, and a registry that
// keeps images past retention. Everything comes from the connection's pushed
// status (KubeCapabilities.image_delivery, whose gateway fields fleetd fills
// live), so a remote TUI shows exactly what a local one does.

// The delivery modes as KubeImageSettings.mode spells them ("" is Auto).
const (
	kubeDeliveryAuto     = ""
	kubeDeliveryNodeLoad = "node-load"
	kubeDeliveryRegistry = "registry"
	kubeDeliveryGateway  = "gateway"
)

// kubeDeliveryOption is one choice of the Delivery selector.
type kubeDeliveryOption struct {
	// Mode is what choosing it saves as image.mode.
	Mode  string
	Label string
	// Selected marks the connection's setting; Disabled a choice that
	// cannot work now, Reason why.
	Selected, Disabled bool
	Reason             string
}

// kubeDeliveryModeLabel names a mode (a resolved one included) for the
// selector.
func kubeDeliveryModeLabel(mode string) string {
	switch mode {
	case kubeDeliveryNodeLoad:
		return "Node load"
	case kubeDeliveryRegistry:
		return "Registry"
	case kubeDeliveryGateway:
		return "Gateway"
	case "none":
		return "none"
	}
	return mode
}

// kubeDeliverySetting is the connection's image.mode, "auto" read as "".
func kubeDeliverySetting(st *fleetgrpc.KubeClusterStatus) string {
	mode := st.GetCluster().GetImage().GetMode()
	if mode == "auto" {
		return kubeDeliveryAuto
	}
	return mode
}

// kubeDeliveryOptions are the Delivery selector's choices for a
// connection: Auto (labelled with what it resolved to), Registry and
// Gateway, plus Node load when the connection is set to it. Gateway is
// disabled, with the reason, while the gateway serves no images for this
// daemon (no gateway, Enable Image Registry off, a gateway too old or
// without --session-key, a public URL that is not https or has a path).
func kubeDeliveryOptions(st *fleetgrpc.KubeClusterStatus) []kubeDeliveryOption {
	setting := kubeDeliverySetting(st)
	del := st.GetCapabilities().GetImageDelivery()
	auto := "Auto"
	if setting == kubeDeliveryAuto && del.GetMode() != "" {
		auto += "→" + kubeDeliveryModeLabel(del.GetMode())
	}
	opts := []kubeDeliveryOption{{Mode: kubeDeliveryAuto, Label: auto}}
	if setting == kubeDeliveryNodeLoad {
		opts = append(opts, kubeDeliveryOption{Mode: kubeDeliveryNodeLoad, Label: "Node load"})
	}
	opts = append(opts,
		kubeDeliveryOption{Mode: kubeDeliveryRegistry, Label: "Registry"},
		kubeDeliveryOption{Mode: kubeDeliveryGateway, Label: "Gateway"})
	for i := range opts {
		opts[i].Selected = opts[i].Mode == setting
		if opts[i].Mode == kubeDeliveryGateway && del != nil && !del.GetGatewayRegistry() {
			opts[i].Disabled, opts[i].Reason = true, del.GetGatewayReason()
		}
	}
	return opts
}

// kubeDeliveryNext is the choice ←/→ (dir -1/+1) moves to from the current
// setting, skipping disabled ones; false when there is none to move to.
func kubeDeliveryNext(st *fleetgrpc.KubeClusterStatus, dir int) (string, bool) {
	opts := kubeDeliveryOptions(st)
	cur := 0
	for i, o := range opts {
		if o.Selected {
			cur = i
		}
	}
	for i := cur + dir; i >= 0 && i < len(opts); i += dir {
		if !opts[i].Disabled {
			return opts[i].Mode, true
		}
	}
	return "", false
}

// kubeDeliverySelector renders the choices, the selected one highlighted
// and the disabled ones dimmed.
func kubeDeliverySelector(opts []kubeDeliveryOption) string {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		label := "[" + o.Label + "]"
		switch {
		case o.Selected:
			parts = append(parts, selectedStyle.Render(label))
		case o.Disabled:
			parts = append(parts, dimStyle.Faint(true).Render(label))
		default:
			parts = append(parts, dimStyle.Render(label))
		}
	}
	return strings.Join(parts, " ")
}

// kubeDeliveryLines are the Image group's lines under the selector.
func kubeDeliveryLines(st *fleetgrpc.KubeClusterStatus) []string {
	del := st.GetCapabilities().GetImageDelivery()
	if del == nil {
		return []string{dimStyle.Render("delivery is shown once the connection has been checked")}
	}
	var lines []string
	setting := kubeDeliverySetting(st)
	switch {
	case del.GetMode() == "none":
		lines = append(lines, statusCreatingStyle.Render("✕ ")+del.GetReason())
	case setting == kubeDeliveryAuto && del.GetReason() != "":
		lines = append(lines, dimStyle.Render("Auto picked "+kubeDeliveryModeLabel(del.GetMode())+": "+del.GetReason()))
	}
	for _, o := range kubeDeliveryOptions(st) {
		if o.Disabled && o.Reason != "" {
			lines = append(lines, dimStyle.Render("Gateway unavailable: "+o.Reason))
		}
	}
	switch del.GetMode() {
	case kubeDeliveryRegistry:
		if del.GetRegistryReachable() && !del.GetRegistryDeleteSupported() {
			lines = append(lines, statusCreatingStyle.Render("▲ ")+"images past retention stay in the registry: "+del.GetReason())
		}
	case kubeDeliveryGateway:
		if step := del.GetGatewayFailedStep(); step != "" {
			lines = append(lines, statusCreatingStyle.Render("✕ ")+"the manifest check through the gateway failed at the "+step+" step")
		}
		lines = append(lines, kubeGatewayPullLine(del))
		if n := del.GetCacheBytes(); n > 0 {
			lines = append(lines, dimStyle.Render("gateway image cache "+kubeDeliveryBytes(n)+" on fleetd (shared by every cluster)"))
		}
		lines = append(lines, dimStyle.Render("a node pulls through the daemon host's upload link; a slow pull delays its other pulls (serializeImagePulls)"))
	}
	return lines
}

// kubeGatewayPullLine is "last pull 412 MB in 38s via gw.example.com · paced
// 80%".
func kubeGatewayPullLine(del *fleetgrpc.KubeImageDelivery) string {
	var b strings.Builder
	if p := del.GetLastPull(); p != nil {
		b.WriteString("last pull " + kubeDeliveryBytes(p.GetBytes()))
		if p.GetLastByteAt() != nil && p.GetStartedAt() != nil {
			b.WriteString(" in " + kubeDeliveryDuration(p.GetLastByteAt().AsTime().Sub(p.GetStartedAt().AsTime())))
		}
	} else {
		b.WriteString("no pull through the gateway yet")
	}
	if h := del.GetGatewayHost(); h != "" {
		b.WriteString(" via " + h)
	}
	b.WriteString(" · ")
	if pct := del.GetUploadPacePercent(); pct > 0 && pct < 100 {
		fmt.Fprintf(&b, "paced %d%%", pct)
		if r := del.GetUplinkBytesPerSecond(); r > 0 {
			fmt.Fprintf(&b, " of %.1f Mbit/s", float64(r)*8/1e6)
		}
	} else {
		b.WriteString("unpaced")
	}
	return dimStyle.Render(b.String())
}

// kubeDeliveryBytes renders a size the way the Image group does: "412 MB".
func kubeDeliveryBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// kubeDeliveryDuration renders a pull's time: "38s", "2m10s".
func kubeDeliveryDuration(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	return d.Round(time.Second).String()
}
