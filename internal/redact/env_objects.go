package redact

import "strings"

// envObjectKeys are the devcontainer properties whose object maps variable
// names to values that may be secrets.
var envObjectKeys = []string{"containerEnv", "remoteEnv"}

// EnvObjects hides every string value of a "containerEnv" or "remoteEnv"
// object in the devcontainer metadata JSON on line. The devcontainer CLI puts
// that JSON on the docker run it logs (-l devcontainer.metadata=[...]), the
// same values the -e flags carry, and into the Dockerfile it generates for a
// features build (LABEL devcontainer.metadata="[...]", every quote, backslash
// and dollar escaped with a backslash), which docker's classic builder echoes.
// Both forms are handled. A value that runs off the end of the line is hidden
// to the end of the line. Names, and values that are not strings (null), stay.
func EnvObjects(line string) string {
	if !strings.Contains(line, "Env") {
		return line
	}
	var b strings.Builder
	copied := 0
	for from := 0; ; {
		at, key := nextEnvObjectKey(line, from)
		if at < 0 {
			break
		}
		from = at + len(key)
		spans, end := envObjectValues(line, at, key)
		for _, sp := range spans {
			b.WriteString(line[copied:sp.start])
			b.WriteString(sp.quote + Placeholder)
			if !sp.open {
				b.WriteString(sp.quote)
			}
			copied = sp.end
		}
		if end > from {
			from = end
		}
	}
	if copied == 0 {
		return line
	}
	b.WriteString(line[copied:])
	return b.String()
}

// nextEnvObjectKey finds the next env-object key at or after from.
func nextEnvObjectKey(line string, from int) (int, string) {
	best, bestKey := -1, ""
	for _, key := range envObjectKeys {
		if i := strings.Index(line[from:], key); i >= 0 && (best < 0 || from+i < best) {
			best, bestKey = from+i, key
		}
	}
	return best, bestKey
}

// valueSpan is a string value to hide: line[start:end], its opening and
// closing quotes included; open marks one the line ends inside.
type valueSpan struct {
	start, end int
	quote      string
	open       bool
}

// envObjectValues parses the object following the key at line[at:] — the key
// must be a JSON member name, `"key": {` or its escaped form — and returns
// the spans of its string values and where parsing stopped. Anything that does
// not look like that object stops the parse; the values found so far are
// still returned.
func envObjectValues(line string, at int, key string) ([]valueSpan, int) {
	var esc bool
	switch {
	case at >= 2 && line[at-2:at] == `\"` && strings.HasPrefix(line[at+len(key):], `\"`):
		esc = true
	case at >= 1 && line[at-1] == '"' && strings.HasPrefix(line[at+len(key):], `"`):
	default:
		return nil, at + len(key)
	}
	quote := `"`
	if esc {
		quote = `\"`
	}
	r := &jsonReader{s: line, pos: at + len(key) + len(quote), esc: esc}
	r.skipSpaces()
	if r.next() != ':' {
		return nil, r.pos
	}
	r.skipSpaces()
	if r.next() != '{' {
		return nil, r.pos
	}
	var spans []valueSpan
	for {
		r.skipSpaces()
		switch r.peek() {
		case '}':
			r.next()
			return spans, r.pos
		case '"':
		default:
			return spans, r.pos
		}
		if !r.skipString() {
			return spans, r.pos
		}
		r.skipSpaces()
		if r.next() != ':' {
			return spans, r.pos
		}
		r.skipSpaces()
		if r.peek() == '"' {
			start := r.pos
			if !r.skipString() {
				return append(spans, valueSpan{start: start, end: len(line), quote: quote, open: true}), len(line)
			}
			spans = append(spans, valueSpan{start: start, end: r.pos, quote: quote})
		} else if !r.skipScalar() {
			return spans, r.pos
		}
		r.skipSpaces()
		switch r.next() {
		case ',':
		case '}':
			return spans, r.pos
		default:
			return spans, r.pos
		}
	}
}

// jsonReader walks JSON text one logical character at a time. In escaped mode
// (JSON written inside a backslash-escaped string) `\"`, `\\` and `\$` each
// stand for the character after the backslash.
type jsonReader struct {
	s   string
	pos int
	esc bool
}

// eof is what peek and next return past the end of the text.
const eof = -1

func (r *jsonReader) width() int {
	if r.esc && r.pos+1 < len(r.s) && r.s[r.pos] == '\\' && strings.IndexByte(`"\$`, r.s[r.pos+1]) >= 0 {
		return 2
	}
	return 1
}

func (r *jsonReader) peek() int {
	if r.pos >= len(r.s) {
		return eof
	}
	if r.width() == 2 {
		return int(r.s[r.pos+1])
	}
	return int(r.s[r.pos])
}

func (r *jsonReader) next() int {
	c := r.peek()
	if c != eof {
		r.pos += r.width()
	}
	return c
}

func (r *jsonReader) skipSpaces() {
	for c := r.peek(); c == ' ' || c == '\t'; c = r.peek() {
		r.next()
	}
}

// skipString moves past the JSON string at the current position (which starts
// with a quote). It reports false when the text ends inside it.
func (r *jsonReader) skipString() bool {
	r.next()
	for {
		switch r.next() {
		case eof:
			return false
		case '\\':
			if r.next() == eof {
				return false
			}
		case '"':
			return true
		}
	}
}

// skipScalar moves past a non-string value (null, a number, true/false).
// Nested objects and arrays are not env values; it reports false for those.
func (r *jsonReader) skipScalar() bool {
	for {
		switch c := r.peek(); c {
		case ',', '}', ' ', '\t':
			return true
		case '{', '[', '"', eof:
			return false
		default:
			r.next()
		}
	}
}
