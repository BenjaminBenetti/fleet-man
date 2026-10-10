// Package devmeta reads the metadata the devcontainer CLI stamps on the
// container it creates: the devcontainer.* labels and the merged
// devcontainer.metadata JSON they carry. It is pure (no docker, no exec, no
// I/O) so every backend derives the same answers from the same labels,
// wherever it gets them: `docker inspect` on the devcontainer backend, or the
// labels a cluster backend recorded from the CLI's `docker run -l` flags.
package devmeta

// Labels the devcontainer CLI sets on a container at `up` time.
const (
	// LocalFolderLabel holds the host workspace folder. With ConfigFileLabel it
	// is how the CLI finds the container for a workspace, so `devcontainer
	// exec` reports "Cannot find Dev Container" when it does not match.
	LocalFolderLabel = "devcontainer.local_folder"
	// ConfigFileLabel holds the host path of the devcontainer.json in use.
	ConfigFileLabel = "devcontainer.config_file"
	// MetadataLabel holds the JSON array of merged devcontainer metadata
	// (image, then each feature, then devcontainer.json). Its remoteUser is
	// the user `devcontainer exec` runs as.
	MetadataLabel = "devcontainer.metadata"
)
