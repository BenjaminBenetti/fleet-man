package landingpage

// Config holds the resolved inputs the server needs to run.
type Config struct {
	// Addr is the address to listen on; "" listens on every interface. A
	// backend fleetd dials the instance's own loopback on (Kubernetes) binds
	// it to 127.0.0.1.
	Addr string
	// Port is the TCP port to listen on.
	Port int
	// WorkspaceDir is the directory whose devcontainer.json supplies the
	// landing-page site list. The injected process runs with its working
	// directory at the instance's workspace folder, so this is "." in the
	// normal case.
	WorkspaceDir string
}
