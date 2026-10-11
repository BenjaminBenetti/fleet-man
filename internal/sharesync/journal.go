package sharesync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// journalVersion is the state format this fleet writes. A file of a newer
// version is never read or rewritten (ErrNewerState); older ones would be
// read through a compatibility step here.
const journalVersion = 1

// metaKey is the key of the journal's metadata record.
const metaKey = ""

// journal persists a set of keyed JSON records: a snapshot (a header line,
// then one record per line) and a log of the records written since, one per
// line. Writing a record is one append to the log, so the state a hub acked
// is on disk before the ack leaves (a crashed process loses nothing it
// answered for; like the rest of fleet it does not fsync). The log is folded
// into a new snapshot once it outgrows it.
type journal struct {
	path   string
	log    *os.File
	seq    uint64
	logged int
	// pending holds the lines of the batch being built.
	pending []journalLine
}

// journalHead is a snapshot's first line.
type journalHead struct {
	Version int    `json:"v"`
	Seq     uint64 `json:"seq"`
}

// journalLine is one record, in the snapshot or the log. Key "" is the
// journal's metadata.
type journalLine struct {
	Seq uint64          `json:"q,omitempty"`
	Key string          `json:"k"`
	Del bool            `json:"d,omitempty"`
	Val json.RawMessage `json:"v,omitempty"`
}

// openJournal opens the journal at path (path.log is its log), creating its
// folder, and returns the records it holds. A snapshot that cannot be read
// is moved aside as path.corrupt-<time> and the journal starts empty, which
// its owner treats as lost state (a new epoch); a torn or unreadable log
// tail is dropped from the first bad line. A newer version is ErrNewerState,
// with nothing touched.
func openJournal(path string) (*journal, map[string]json.RawMessage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	records := map[string]json.RawMessage{}
	j := &journal{path: path}
	head, err := readSnapshot(path, records)
	switch {
	case errors.Is(err, ErrNewerState):
		return nil, nil, err
	case errors.Is(err, errCorrupt):
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102T150405.000000000"))
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, nil, fmt.Errorf("%w (and moving it aside: %v)", err, rerr)
		}
		clear(records)
		os.Remove(path + ".log")
		head = journalHead{}
	case err != nil:
		return nil, nil, err
	}
	j.seq = head.Seq
	good, err := j.replay(records, head.Seq)
	if err != nil {
		return nil, nil, err
	}
	log, err := os.OpenFile(path+".log", os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	// Drop a torn tail, so the next line starts on a line of its own.
	if err := log.Truncate(good); err != nil {
		log.Close()
		return nil, nil, err
	}
	if _, err := log.Seek(good, 0); err != nil {
		log.Close()
		return nil, nil, err
	}
	j.log = log
	return j, records, nil
}

// errCorrupt marks a snapshot that cannot be read.
var errCorrupt = errors.New("sync state is corrupt")

// readSnapshot reads the snapshot at path into records. A missing one is an
// empty journal.
func readSnapshot(path string, records map[string]json.RawMessage) (journalHead, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return journalHead{}, nil
	}
	if err != nil {
		return journalHead{}, err
	}
	first, rest, _ := bytes.Cut(data, []byte("\n"))
	var head journalHead
	if err := json.Unmarshal(first, &head); err != nil {
		return journalHead{}, fmt.Errorf("%s: %w: header: %v", path, errCorrupt, err)
	}
	if head.Version > journalVersion {
		return journalHead{}, fmt.Errorf("%s: %w (version %d, this fleet reads %d)", path, ErrNewerState, head.Version, journalVersion)
	}
	if head.Version < 1 {
		return journalHead{}, fmt.Errorf("%s: %w: version %d", path, errCorrupt, head.Version)
	}
	for n := 2; len(rest) > 0; n++ {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		if len(line) == 0 {
			continue
		}
		var rec journalLine
		if err := json.Unmarshal(line, &rec); err != nil {
			return journalHead{}, fmt.Errorf("%s: %w: line %d: %v", path, errCorrupt, n, err)
		}
		records[rec.Key] = rec.Val
	}
	return head, nil
}

// replay applies the log's lines written after seq to records and returns
// the length of the log up to its last good line.
func (j *journal) replay(records map[string]json.RawMessage, seq uint64) (int64, error) {
	f, err := os.Open(j.path + ".log")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var good int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// A last line without its newline was cut short.
			return good, nil
		}
		var rec journalLine
		if json.Unmarshal(line, &rec) != nil || rec.Seq == 0 {
			return good, nil
		}
		good += int64(len(line))
		j.logged++
		if rec.Seq <= seq {
			continue
		}
		j.seq = max(j.seq, rec.Seq)
		if rec.Del {
			delete(records, rec.Key)
		} else {
			records[rec.Key] = rec.Val
		}
	}
}

// put adds a record to the batch being built.
func (j *journal) put(key string, val any) error {
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	j.pending = append(j.pending, journalLine{Key: key, Val: data})
	return nil
}

// del adds a record's removal to the batch being built.
func (j *journal) del(key string) {
	j.pending = append(j.pending, journalLine{Key: key, Del: true})
}

// commit appends the batch to the log in one write.
func (j *journal) commit() error {
	if len(j.pending) == 0 {
		return nil
	}
	if j.log == nil {
		return ErrClosed
	}
	var buf bytes.Buffer
	for _, line := range j.pending {
		j.seq++
		line.Seq = j.seq
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	n := len(j.pending)
	j.pending = j.pending[:0]
	if _, err := j.log.Write(buf.Bytes()); err != nil {
		return err
	}
	j.logged += n
	return nil
}

// due reports whether the log has outgrown a snapshot of size records.
func (j *journal) due(size int) bool {
	return j.logged > max(1024, 2*size)
}

// compact writes a snapshot of records (key to value, the metadata at "")
// and empties the log.
func (j *journal) compact(records map[string]any) error {
	if j.log == nil {
		return ErrClosed
	}
	if err := j.commit(); err != nil {
		return err
	}
	var buf bytes.Buffer
	head, _ := json.Marshal(journalHead{Version: journalVersion, Seq: j.seq})
	buf.Write(head)
	buf.WriteByte('\n')
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		val, err := json.Marshal(records[key])
		if err != nil {
			return err
		}
		line, _ := json.Marshal(journalLine{Key: key, Val: val})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := atomicfile.Write(j.path, buf.Bytes(), 0o600); err != nil {
		return err
	}
	// The snapshot holds every line of the log now (their seqs are at most
	// its own), so a crash before the truncate only replays nothing.
	if err := j.log.Truncate(0); err != nil {
		return err
	}
	if _, err := j.log.Seek(0, 0); err != nil {
		return err
	}
	j.logged = 0
	return nil
}

// close closes the log.
func (j *journal) close() error {
	if j.log == nil {
		return nil
	}
	err := j.commit()
	if cerr := j.log.Close(); err == nil {
		err = cerr
	}
	j.log = nil
	return err
}
