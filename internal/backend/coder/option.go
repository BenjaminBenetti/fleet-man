package coder

// Option configures a CoderBackend.
type Option func(*CoderBackend)

// WithVerbose enables verbose output.
func WithVerbose(v bool) Option {
	return func(coderBackend *CoderBackend) { coderBackend.verbose = v }
}

// WithTemplate sets the Coder template to use when creating workspaces.
func WithTemplate(t string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.template = t }
}

// WithPreset sets the Coder preset to use when creating workspaces.
func WithPreset(p string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.preset = p }
}

// WithParameters sets the resolved parameter key-value pairs for workspace creation.
func WithParameters(params map[string]string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.parameters = params }
}
