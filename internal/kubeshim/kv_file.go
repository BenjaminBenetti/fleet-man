package kubeshim

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// readKVFile reads a docker --env-file or --label-file the way docker's CLI
// does (opts.ParseEnvFile, opts.ParseKeyValueFile): one entry per line, leading
// whitespace dropped, blank lines and lines starting with '#' skipped, KEY=VALUE
// taken as written (no quotes, no trimming of the value), a UTF-8 BOM on the
// first line ignored. A line with only a name is passed to lookup, which says
// what it stands for: an env file looks the name up in the environment, a
// label file keeps it as a label with an empty value.
func readKVFile(path string, lookup func(name string) (string, bool)) ([]kvEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []kvEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; scanner.Scan(); n++ {
		raw := scanner.Bytes()
		if n == 1 {
			raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
		}
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("%s contains invalid utf8 bytes at line %d", path, n)
		}
		line := strings.TrimLeftFunc(string(raw), unicode.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, hasValue := strings.Cut(line, "=")
		if name == "" {
			return nil, fmt.Errorf("%s: no variable name on line '%s'", path, line)
		}
		if strings.ContainsAny(name, " \t\n\v\f\r") {
			return nil, fmt.Errorf("%s: variable '%s' contains whitespaces", path, name)
		}
		if !hasValue {
			v, ok := lookup(name)
			out = append(out, kvEntry{name: name, value: v, set: ok})
			continue
		}
		out = append(out, kvEntry{name: name, value: value, set: true})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return out, nil
}

// kvEntry is one line of a key/value file; set is false for a name-only env
// line whose variable is not in the environment.
type kvEntry struct {
	name, value string
	set         bool
}
