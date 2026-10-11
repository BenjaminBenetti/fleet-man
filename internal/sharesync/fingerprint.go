package sharesync

import (
	"io/fs"
)

// fingerprint is what a scan compares to tell a path changed without reading
// it: a file that keeps its fingerprint is taken to keep its content. A
// folder's covers only its mode and identity, since its times change with
// every entry made in it.
type fingerprint struct {
	Size  int64  `json:"s,omitempty"`
	MTime int64  `json:"t,omitempty"`
	Mode  uint32 `json:"m"`
	Ino   uint64 `json:"i,omitempty"`
	Dev   uint64 `json:"d,omitempty"`
	CTime int64  `json:"c,omitempty"`
}

// fingerprintOf returns info's fingerprint.
func fingerprintOf(info fs.FileInfo) fingerprint {
	ino, dev, ctime := sysStat(info)
	fp := fingerprint{Mode: uint32(info.Mode()), Ino: ino, Dev: dev}
	if !info.IsDir() {
		fp.Size, fp.MTime, fp.CTime = info.Size(), info.ModTime().UnixNano(), ctime
	}
	return fp
}

// kindOf returns the kind of entry info describes, or false for one
// sharesync does not sync (FIFOs, sockets, devices).
func kindOf(info fs.FileInfo) (Kind, bool) {
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		return File, true
	case mode.IsDir():
		return Dir, true
	case mode&fs.ModeSymlink != 0:
		return Symlink, true
	}
	return Gone, false
}
