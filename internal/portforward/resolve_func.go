package portforward

// ResolveFunc returns a directly-reachable hostname for a container.
// Returns ("", false) when the container is not directly reachable.
type ResolveFunc func(containerID string) (string, bool)
