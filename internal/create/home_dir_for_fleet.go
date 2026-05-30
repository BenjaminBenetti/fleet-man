package create

import "github.com/BenjaminBenetti/fleet-man/internal/state"

// homeDirForFleet looks up the named fleet's persisted HomeDir
// setting, returning empty when state can't be loaded or the fleet
// isn't recorded yet. Callers pass the result straight to
// fleetlaunch.EnsureFleetRC, which substitutes its DefaultHomeDir
// when given empty — so missing state silently falls back rather
// than failing the stage.
func homeDirForFleet(fleetName string) string {
	st, err := state.Load()
	if err != nil {
		return ""
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return ""
	}
	return f.Settings.HomeDir
}
