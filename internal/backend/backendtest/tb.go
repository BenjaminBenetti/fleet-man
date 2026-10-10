package backendtest

// TB is the part of *testing.T the suite and Stub use. It is an interface of
// its own (testing.TB can't be implemented outside the testing package) so the
// suite's tests can run a case against a recorder and check that it failed.
type TB interface {
	Helper()
	Name() string
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
	Failed() bool
	Cleanup(func())
	TempDir() string
}
