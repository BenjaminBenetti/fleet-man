package startup

import "fmt"

// Error describes a single startup script failure. The script's stdout
// and stderr are not included — they are captured to the script's log
// file inside the container so users can inspect details there.
type Error struct {
	// ScriptName is the Name of the failed Script.
	ScriptName string

	// LogPath is the path *inside the container* where the script's
	// full output was captured.
	LogPath string

	// Err is the underlying execution error (typically a non-zero exit
	// status from the install command).
	Err error
}

// Error implements the error interface with a single-line summary
// suitable for the TUI's warning banner.
func (e Error) Error() string {
	return fmt.Sprintf("startup script %q failed: %v (see %s inside instance)", e.ScriptName, e.Err, e.LogPath)
}
