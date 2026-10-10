package deps

// HostBuildTools returns the install status of the tools fleetd runs on its
// own host to build instances: docker and the devcontainer CLI. The daemon
// reports them to clients (ListDeployTargets) for the fleetd rows of Settings
// -> Tool Status, so a remote client sees the daemon host's tools rather than
// its own. lookPath resolves a binary on the daemon's PATH (exec.LookPath).
func HostBuildTools(lookPath func(file string) (string, error)) []ToolStatus {
	tools := []ToolStatus{
		{
			Name:        "docker",
			Binary:      "docker",
			Description: "Builds and runs devcontainer instances.",
			InstallURL:  "https://docs.docker.com/engine/install/",
		},
		{
			Name:        "devcontainer",
			Binary:      "devcontainer",
			Description: "Creates devcontainer instances.",
			InstallURL:  "https://github.com/devcontainers/cli#npm-install",
		},
	}
	for i := range tools {
		_, err := lookPath(tools[i].Binary)
		tools[i].Found = err == nil
	}
	return tools
}
