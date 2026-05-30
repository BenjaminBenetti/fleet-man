package codespaces

// Option configures a CodespacesBackend.
type Option func(*CodespacesBackend)

// WithVerbose enables verbose output.
func WithVerbose(verbose bool) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.verbose = verbose }
}

// WithRepo sets the GitHub repository (owner/repo) for codespace creation.
func WithRepo(repo string) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.repo = repo }
}

// WithMachine sets the machine type for codespace creation.
func WithMachine(machine string) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.machine = machine }
}

// WithIdleTimeout sets the idle timeout duration string (e.g. "30m").
func WithIdleTimeout(timeout string) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.idleTimeout = timeout }
}

// WithDevcontainerPath sets the path to the devcontainer.json within the repo.
func WithDevcontainerPath(path string) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.devcontainerPath = path }
}

// WithBranch sets the repository branch the codespace is created from.
// An empty string lets GitHub pick the repository's default branch.
func WithBranch(branch string) Option {
	return func(codespacesBackend *CodespacesBackend) { codespacesBackend.branch = branch }
}
