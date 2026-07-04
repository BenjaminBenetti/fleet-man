package coder

// Option configures a CoderBackend.
type Option func(*CoderBackend)

// WithVerbose enables verbose output.
func WithVerbose(verbose bool) Option {
	return func(coderBackend *CoderBackend) { coderBackend.verbose = verbose }
}

// WithTemplate sets the Coder template to use when creating workspaces.
func WithTemplate(template string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.template = template }
}

// WithPreset sets the Coder preset to use when creating workspaces.
func WithPreset(preset string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.preset = preset }
}

// WithParameters sets the resolved parameter key-value pairs for workspace creation.
func WithParameters(params map[string]string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.parameters = params }
}

// WithWorkspaceName sets the explicit (already sanitized) workspace name Up
// creates, instead of deriving one from the workspace dir path. See
// WorkspaceNameFor.
func WithWorkspaceName(name string) Option {
	return func(coderBackend *CoderBackend) { coderBackend.workspaceName = name }
}
