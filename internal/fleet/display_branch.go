package fleet

// DisplayBranch picks the branch to show for an instance. live is the branch
// a live workspace read returned (InstanceRuntime.current_branch), nil when no
// read was possible. A live read wins even when it is empty: HEAD is detached,
// or the workspace is not a git repository, and that is what the user should
// see. Without one, the last branch a live read saw (Instance.LastBranch) is
// shown, then the branch the instance was created from (Instance.Branch).
//
// Every surface that shows a branch (the TUI row, `fleet ls`, the MCP
// fleet_list tool) goes through this so they never disagree.
func DisplayBranch(live *string, lastBranch, requested string) string {
	if live != nil {
		return *live
	}
	if lastBranch != "" {
		return lastBranch
	}
	return requested
}
