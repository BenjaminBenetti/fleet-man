package control

// CopyFilePayload is the body of a TypeCopyFile Envelope: a scp-style copy the
// in-instance `fc` is asking the connected host TUI to perform on its behalf
// (the container can't reach the host fleetd over gRPC, only this socket). Src
// and Dst are the two endpoints AS TYPED inside the instance — each either
// `[fleet/]instance:path`, a plain path on the host TUI's machine, or `:path`
// (the current instance). The TUI runs the generic copy engine over them.
type CopyFilePayload struct {
	// Src is the source endpoint as typed inside the instance.
	Src string `json:"src"`
	// Dst is the destination endpoint as typed inside the instance. Empty is the
	// 1-arg download shorthand (deliver to the TUI's downloads folder).
	Dst string `json:"dst,omitempty"`
	// Open asks the TUI to open the delivered file with its machine's default
	// application once the copy lands — the in-instance `fleet open` (fo). Only
	// meaningful when Dst is on the TUI's machine (or empty); the TUI refuses to
	// open anything else.
	Open bool `json:"open,omitempty"`
}
