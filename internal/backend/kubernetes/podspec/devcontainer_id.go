package podspec

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
)

// devcontainerID is the ${devcontainerId} the devcontainer CLI substituted
// into the run, computed as the CLI computes it from the container's id
// labels (devcontainer.local_folder and devcontainer.config_file): the
// SHA-256 of the labels as JSON (keys sorted), in base 32, padded to 52
// characters. "" when the run has neither label.
//
// A volume named after it belongs to this one container (the
// docker-in-docker feature's /var/lib/docker volume is
// dind-var-lib-docker-${devcontainerId}).
func devcontainerID(labels map[string]string) string {
	id := map[string]string{}
	for _, k := range []string{devmeta.LocalFolderLabel, devmeta.ConfigFileLabel} {
		if v, ok := labels[k]; ok {
			id[k] = v
		}
	}
	if len(id) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(jsJSONObject(id)))
	s := new(big.Int).SetBytes(sum[:]).Text(32)
	return strings.Repeat("0", max(0, 52-len(s))) + s
}

// jsJSONObject writes m as JavaScript's JSON.stringify does with its keys
// sorted: no spaces, and only quotes, backslashes and control characters
// escaped (encoding/json also escapes <, >, & and U+2028/U+2029, which would
// change the hash).
func jsJSONObject(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		jsJSONString(&b, k)
		b.WriteByte(':')
		jsJSONString(&b, m[k])
	}
	b.WriteByte('}')
	return b.String()
}

func jsJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
