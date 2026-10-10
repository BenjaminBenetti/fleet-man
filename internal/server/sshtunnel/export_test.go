package sshtunnel

// export_test.go lends the in-process SSH server harness (e2e_test.go) to
// this directory's external tests, which drive a real fleetd through it
// (kube_remote_e2e_test.go): package server imports this one, so they cannot
// live inside it.

// TestSSHServer is the in-process SSH server; Addr is where it listens.
type TestSSHServer = testSSHServer

func (s *testSSHServer) Addr() string { return s.addr }

var (
	StartTestSSHServer = startTestSSHServer
	ClientSSHHome      = clientSSHHome
)
