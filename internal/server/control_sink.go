package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// control_sink.go: where the daemon writes the files it keeps in an
// instance's control directory (/fleet-mounts/control inside), such as the
// status mod's. Either the host control directory provisioning bind-mounted
// into the instance (hostSink), or the control channel the instance's backend
// carries (channelSink, the plan's podSink).

// controlChannelRepairInterval is how long a file sent through a control
// channel is taken to still be there. The daemon can't look inside to check,
// so after this long an unchanged file is sent again, which repairs one the
// instance deleted or missed.
const controlChannelRepairInterval = 30 * time.Second

// controlSink is where one instance's control-directory files go.
type controlSink interface {
	// key names the destination, for remembering what was last written there.
	key() string
	// reaches reports whether a file written here is seen inside the
	// instance.
	reaches() bool
	// write makes the file name hold data, with mode. now is when, as the
	// caller tells time.
	write(name string, data []byte, mode fs.FileMode, now time.Time) (controlStamp, error)
	// intact reports whether name still holds what the write that returned
	// stamp put there, as far as the sink can tell at now. False has the
	// caller write it again.
	intact(name string, stamp controlStamp, now time.Time) bool
	// remove removes name. A file that is not there is not an error.
	remove(name string) error
}

// controlStamp is a sink's record of one write.
type controlStamp struct {
	// at is when it was written.
	at time.Time
	// file is the file as it stood right after the write, where the sink can
	// see it (the host control directory).
	file os.FileInfo
}

// controlSinkFor returns where inst's control files go: its backend's control
// channel when it carries one, else its host control directory. A package var
// so tests can stand a channel in.
var controlSinkFor = func(fleetName string, inst *fleet.Instance) controlSink {
	if channel, ok := controlChannelFor(inst); ok {
		return channelSink{name: runtimeKey(fleetName, inst.Name), channel: channel}
	}
	return hostSink{dir: state.ControlDir(fleetName, inst.Name)}
}

// hostSink is the instance's host control directory. The instance can write
// there too, so a file is written by temp+rename (atomicfile) and removed by
// unlink: a link planted at its name is replaced or removed, never followed.
type hostSink struct{ dir string }

func (s hostSink) key() string { return s.dir }

// reaches reports whether provisioning bind-mounted the directory into the
// instance: until then it is just a directory on the host.
func (s hostSink) reaches() bool { return controlDirMounted(s.dir) }

func (s hostSink) write(name string, data []byte, mode fs.FileMode, now time.Time) (controlStamp, error) {
	path := filepath.Join(s.dir, name)
	if err := atomicfile.Write(path, data, mode); err != nil {
		return controlStamp{}, err
	}
	written, _ := os.Lstat(path)
	return controlStamp{at: now, file: written}, nil
}

// intact reports whether the file is still the one written: the same file,
// size and modification time (the instance can delete, replace or overwrite
// it).
func (s hostSink) intact(name string, stamp controlStamp, _ time.Time) bool {
	info, err := os.Lstat(filepath.Join(s.dir, name))
	return err == nil && stamp.file != nil && os.SameFile(stamp.file, info) &&
		info.Size() == stamp.file.Size() && info.ModTime().Equal(stamp.file.ModTime())
}

func (s hostSink) remove(name string) error {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// channelSink is the control channel the instance's backend carries. Every
// write goes through at once; the channel delivers it.
type channelSink struct {
	name    string
	channel backend.ControlChannel
}

func (s channelSink) key() string { return "channel:" + s.name }

// reaches is always true: the channel is the instance's own control
// directory.
func (s channelSink) reaches() bool { return true }

func (s channelSink) write(name string, data []byte, mode fs.FileMode, now time.Time) (controlStamp, error) {
	if err := s.channel.WriteControlFile(name, data, mode); err != nil {
		return controlStamp{}, err
	}
	return controlStamp{at: now}, nil
}

// intact can't look inside the instance, so a file counts as there until it
// is controlChannelRepairInterval old.
func (s channelSink) intact(_ string, stamp controlStamp, now time.Time) bool {
	return now.Sub(stamp.at) < controlChannelRepairInterval
}

func (s channelSink) remove(name string) error { return s.channel.RemoveControlFile(name) }
