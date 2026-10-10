package backendtest

// Kind says how a case uses instances.
type Kind uint8

const (
	// Method cases check one Backend method, or a small group of them,
	// mostly on the instance the method cases share.
	Method Kind = iota
	// Scenario cases drive a lifecycle on instances of their own.
	Scenario
)

// String is the kind's subtest prefix.
func (k Kind) String() string {
	if k == Scenario {
		return "scenario"
	}
	return "method"
}

// Case is one conformance check.
type Case struct {
	// Name identifies the case in subtest names and in Target.Skip.
	Name string
	Kind Kind
	Run  func(c *C)
}

// Cases lists every conformance case in the order Run runs them: the method
// cases, then the scenarios.
//
// Not covered yet, each waiting on the unit that brings what it needs: the
// daemon-level scenarios (a create interrupted by a fleetd kill then
// rebuilt, a daemon kill mid-rebuild reconciled), rebuild-failure rollback,
// clone consistency while the source writes (T1/T2 tiers), the DinD volume
// and the image-cache mirror across stop/start and rebuild, the mic sink and
// audio output EOF, CopyFile while audio output streams, and the fleet binary
// after a rebuild (provisioning, above the Backend).
func Cases() []Case {
	return []Case{
		{Name: "up", Kind: Method, Run: caseUp},
		{Name: "up-again", Kind: Method, Run: caseUpAgain},
		{Name: "capabilities", Kind: Method, Run: caseCapabilities},
		{Name: "status", Kind: Method, Run: caseStatus},
		{Name: "exec-exit-codes", Kind: Method, Run: caseExecExitCodes},
		{Name: "exec-streams", Kind: Method, Run: caseExecStreams},
		{Name: "exec-stdin", Kind: Method, Run: caseExecStdin},
		{Name: "exec-user", Kind: Method, Run: caseExecUser},
		{Name: "exec-quiet", Kind: Method, Run: caseExecQuiet},
		{Name: "exec-sync", Kind: Method, Run: caseExecSync},
		{Name: "exec-tty", Kind: Method, Run: caseExecTTY},
		{Name: "exec-tar", Kind: Method, Run: caseExecTar},
		{Name: "exec-pipe-out", Kind: Method, Run: caseExecPipeOut},
		{Name: "copy-file", Kind: Method, Run: caseCopyFile},
		{Name: "stats", Kind: Method, Run: caseStats},
		{Name: "logs", Kind: Method, Run: caseLogs},
		{Name: "sessions", Kind: Method, Run: caseSessions},
		{Name: "tool-probe", Kind: Method, Run: caseToolProbe},
		{Name: "forward", Kind: Method, Run: caseForward},
		{Name: "editor-uri", Kind: Method, Run: caseEditorURI},

		{Name: "stop-start", Kind: Scenario, Run: caseStopStart},
		{Name: "rebuild", Kind: Scenario, Run: caseRebuild},
		{Name: "rebuild-stopped", Kind: Scenario, Run: caseRebuildStopped},
		{Name: "clone", Kind: Scenario, Run: caseClone},
		{Name: "failed-create", Kind: Scenario, Run: caseFailedCreate},
	}
}

// caseByName finds a case (for the suite's own tests).
func caseByName(name string) (Case, bool) {
	for _, tc := range Cases() {
		if tc.Name == name {
			return tc, true
		}
	}
	return Case{}, false
}
