package kubeshim

// ImageConfig is the part of `docker image inspect` the shim and the cluster
// backend read from a built or pulled image. The JSON names are docker's, so
// the inspect output decodes into it directly.
type ImageConfig struct {
	ID           string   `json:"Id"`
	RepoTags     []string `json:"RepoTags,omitempty"`
	RepoDigests  []string `json:"RepoDigests,omitempty"`
	Architecture string   `json:"Architecture,omitempty"`
	Os           string   `json:"Os,omitempty"`
	Variant      string   `json:"Variant,omitempty"`
	Config       struct {
		User         string              `json:"User,omitempty"`
		Env          []string            `json:"Env,omitempty"`
		Entrypoint   []string            `json:"Entrypoint,omitempty"`
		Cmd          []string            `json:"Cmd,omitempty"`
		WorkingDir   string              `json:"WorkingDir,omitempty"`
		Labels       map[string]string   `json:"Labels,omitempty"`
		StopSignal   string              `json:"StopSignal,omitempty"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
		// Volumes are the image's VOLUME paths, each an anonymous volume in
		// a container docker creates from it.
		Volumes map[string]struct{} `json:"Volumes,omitempty"`
	} `json:"Config"`
}
