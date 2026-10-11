package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
)

// buildkit_credentials.go keeps the credentials instances hold for their
// fleet's shared Buildkit where their runtime authenticates each one (a
// cluster's: a client certificate valid for a week, see
// svcruntime.CredentialIssuer). Create, clone, rebuild and start issue one;
// the daemon renews each running instance's once a day, so builds keep
// working through a daemon outage of up to a week. An instance whose
// credential nears its end anyway (the renewal keeps failing, or the daemon
// was down for days) says so on its banner until it is renewed.

// When a credential is renewed, and when its instance's banner warns.
const (
	credentialRenewAge     = 24 * time.Hour
	credentialBannerWithin = 48 * time.Hour
)

// credentialBannerPrefix starts the banner line of a credential near or
// past its end.
const credentialBannerPrefix = "Buildkit credential "

// Seams for tests: the clock, and the renewal of one instance's credential.
var (
	credentialNow     = time.Now
	renewInstanceCred = func(rt svcruntime.Runtime, fleetName string, inst *fleet.Instance) error {
		_, err := svcruntime.ConfigureBuildkit(rt, backendutil.NewForInstance(inst, false), fleetName, inst.Name, inst.WorkspaceDir, svcruntime.BuildkitRenew)
		return err
	}
)

// credentialHolder is a running instance whose runtime issues it a shared
// Buildkit credential.
type credentialHolder struct {
	fleet  string
	inst   *fleet.Instance
	rt     svcruntime.Runtime
	issuer svcruntime.CredentialIssuer
}

// renewSharedServiceCredentials renews the shared Buildkit credential of
// every running instance that holds one older than credentialRenewAge (or
// none), on a reachable runtime, a few at a time, and keeps each one's
// expiry banner (credentialBanner) up to date. An instance whose runtime no
// longer serves the fleet's Buildkit (the cluster stopped allowing it) loses
// the banner; its start says why. Best-effort, logged.
func renewSharedServiceCredentials(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("buildkit credentials: load state failed", "err", err)
		return
	}
	holders := credentialHolders(st)
	reachable := map[string]bool{}
	for _, h := range holders {
		if _, ok := reachable[h.rt.Key()]; !ok {
			reachable[h.rt.Key()] = h.rt.Reachable(ctx)
		}
	}
	forEachBounded(len(holders), pollerWorkers, func(i int) {
		if ctx.Err() == nil {
			renewCredential(holders[i], reachable[holders[i].rt.Key()])
		}
	})
}

// credentialHolders are st's running instances, in fleet then instance
// order, of fleets with shared Buildkit on, whose runtime issues credentials.
func credentialHolders(st *state.State) []credentialHolder {
	names := make([]string, 0, len(st.Fleets))
	for name, f := range st.Fleets {
		if f != nil && f.Settings.BuildkitServer {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []credentialHolder
	for _, name := range names {
		for _, inst := range st.Fleets[name].Instances {
			if inst == nil || inst.Status != fleet.StatusRunning || inst.ContainerID == "" {
				continue
			}
			rt := sharedServicesFor(inst)
			issuer, ok := rt.(svcruntime.CredentialIssuer)
			if !ok {
				continue
			}
			out = append(out, credentialHolder{fleet: name, inst: inst, rt: rt, issuer: issuer})
		}
	}
	return out
}

// renewCredential renews h's credential when it is due and its runtime is
// reachable, then sets its banner by the credential it holds.
func renewCredential(h credentialHolder, reachable bool) {
	at := credentialNow()
	notBefore, notAfter, ok := h.issuer.InstalledCredential(h.fleet, h.inst.Name, fleet.ServiceBuildkit)
	if (!ok || at.Sub(notBefore) >= credentialRenewAge) && reachable {
		err := renewInstanceCred(h.rt, h.fleet, h.inst)
		switch {
		case errors.Is(err, svcruntime.ErrNoEndpoint):
			flog.Info("buildkit credentials: the runtime does not serve the fleet's Buildkit; not renewed", "runtime", h.rt.Key(), "fleet", h.fleet, "instance", h.inst.Name, "reason", err)
			state.ReplaceWarnLine(h.fleet, h.inst.Name, credentialBannerPrefix, "")
			return
		case err != nil:
			flog.Warn("buildkit credentials: renewal failed", "runtime", h.rt.Key(), "fleet", h.fleet, "instance", h.inst.Name, "err", err)
		default:
			flog.Info("buildkit credentials: renewed", "runtime", h.rt.Key(), "fleet", h.fleet, "instance", h.inst.Name)
		}
		_, notAfter, ok = h.issuer.InstalledCredential(h.fleet, h.inst.Name, fleet.ServiceBuildkit)
	}
	line := ""
	if ok {
		line = credentialBanner(notAfter.Sub(at))
	}
	state.ReplaceWarnLine(h.fleet, h.inst.Name, credentialBannerPrefix, line)
}

// credentialBanner is the banner line of a credential with left to its end
// ("" while more than credentialBannerWithin is).
func credentialBanner(left time.Duration) string {
	switch {
	case left >= credentialBannerWithin:
		return ""
	case left > 0:
		return fmt.Sprintf(credentialBannerPrefix+"expires in %s; it renews while the Fleet daemon runs", roughDuration(left))
	}
	return fmt.Sprintf(credentialBannerPrefix+"expired %s ago; shared Buildkit builds fail until the Fleet daemon renews it", roughDuration(-left))
}

// roughDuration is d in whole hours, or minutes under an hour.
func roughDuration(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", max(int(d/time.Minute), 1))
}
