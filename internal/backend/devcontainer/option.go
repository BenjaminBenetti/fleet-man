package devcontainer

// Option configures a DevcontainerBackend.
type Option func(*DevcontainerBackend)

// WithVerbose enables verbose output (sends devcontainer stderr to os.Stderr).
func WithVerbose(verbose bool) Option {
	return func(devcontainerBackend *DevcontainerBackend) { devcontainerBackend.verbose = verbose }
}
