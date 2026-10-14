//go:build !linux

package execas

// runTestHelper has no roles off linux, where no test starts a command.
func runTestHelper(string) {}
