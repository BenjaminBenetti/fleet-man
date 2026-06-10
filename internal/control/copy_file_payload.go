package control

// CopyFilePayload is the body of a TypeCopyFile Envelope: the in-instance file
// the host should copy out to the user's machine. The receiving client pulls
// the bytes itself (over the CopyFile RPC) — this message only names the file.
type CopyFilePayload struct {
	// Path is the absolute in-instance path of the file to copy. The sender
	// resolves it against its own working directory before sending, so the
	// host-side read needs no cwd context.
	Path string `json:"path"`
}
