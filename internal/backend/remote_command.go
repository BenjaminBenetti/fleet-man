package backend

// AppendRemoteCommand appends a remote command to an SSH-style argument
// list. Several CLIs (coder ssh, gh codespace ssh) concatenate everything
// after the command separator and pass it to a remote shell, so a
// "sh -c 'script'" invocation must be collapsed to just "script" to avoid
// double-shell wrapping. Any other command is appended verbatim.
//
// An empty command leaves args unchanged.
func AppendRemoteCommand(args, command []string) []string {
	if len(command) == 0 {
		return args
	}
	if len(command) == 3 && command[0] == "sh" && command[1] == "-c" {
		return append(args, command[2])
	}
	return append(args, command...)
}
