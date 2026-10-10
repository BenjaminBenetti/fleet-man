package devcli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// BuildContextWarnings checks a Dockerfile build's context for secrets that
// would end up in the image. On a cluster the image leaves the daemon host:
// registry delivery publishes its layers, and gateway delivery serves them to
// anyone holding the cluster's pull credential. So it warns when the context
// has no .dockerignore (or <Dockerfile>.dockerignore), and when it holds files
// that look like secrets (.env, id_* private keys, *.pem) that the
// .dockerignore does not exclude. An image-only configuration has no context
// of the user's and gets no warning.
func BuildContextWarnings(cfg *Config) []string {
	if cfg == nil || cfg.Build == nil {
		return nil
	}
	ctxDir := cfg.Build.Context
	ignore, found, err := readDockerignore(ctxDir, cfg.Build.Dockerfile)
	var warnings []string
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not read the image build context's .dockerignore: %v", err))
	}
	if !found {
		warnings = append(warnings, fmt.Sprintf("the image build context %s has no .dockerignore: every file in it can end up in the image, whose layers a registry publishes and the Fleet gateway serves to anyone with the cluster's pull credential", ctxDir))
	}
	if secrets := secretLookingFiles(ctxDir, ignore); len(secrets) > 0 {
		warnings = append(warnings, fmt.Sprintf("the image build context %s holds files that look like secrets (%s); exclude them in .dockerignore: the image's layers are published by a registry and served by the Fleet gateway to anyone with the cluster's pull credential", ctxDir, strings.Join(secrets, ", ")))
	}
	return warnings
}

// readDockerignore reads the build's ignore file: <Dockerfile>.dockerignore
// next to the Dockerfile (BuildKit's per-Dockerfile file), else .dockerignore
// at the context root.
func readDockerignore(ctxDir, dockerfile string) (*ignoreRules, bool, error) {
	candidates := []string{filepath.Join(ctxDir, ".dockerignore")}
	if dockerfile != "" {
		candidates = append([]string{dockerfile + ".dockerignore"}, candidates...)
	}
	for _, path := range candidates {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		rules, err := parseDockerignore(f)
		_ = f.Close()
		return rules, true, err
	}
	return &ignoreRules{}, false, nil
}

// maxContextEntries bounds the secret scan of a build context.
const maxContextEntries = 20000

// maxSecretsListed bounds how many files a warning names.
const maxSecretsListed = 5

// secretLookingFiles lists the context's files whose names look like secrets
// and that ignore does not exclude, relative to ctxDir.
func secretLookingFiles(ctxDir string, ignore *ignoreRules) []string {
	var found []string
	entries := 0
	_ = filepath.WalkDir(ctxDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		entries++
		if entries > maxContextEntries {
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(ctxDir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if looksSecret(d.Name()) && !ignore.excludes(rel) {
			found = append(found, rel)
		}
		return nil
	})
	sort.Strings(found)
	if len(found) > maxSecretsListed {
		found = append(found[:maxSecretsListed], fmt.Sprintf("%d more", len(found)-maxSecretsListed))
	}
	return found
}

// looksSecret reports whether a file name looks like a secret: an env file,
// an SSH private key (id_rsa, id_ed25519; not the .pub half), a PEM file.
func looksSecret(name string) bool {
	switch {
	case name == ".env":
		return true
	case strings.HasPrefix(name, "id_") && !strings.HasSuffix(name, ".pub"):
		return true
	case strings.HasSuffix(name, ".pem"):
		return true
	}
	return false
}

// ignoreRules are a .dockerignore's patterns, matched as docker's builder
// matches them (moby/patternmatcher): in order, the last matching pattern
// deciding, a "!" pattern re-including; a pattern also matches everything
// below a directory it matches; "**" spans directories, "*" and "?" stay
// within one.
type ignoreRules struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	re        *regexp.Regexp
	exclusion bool
}

// parseDockerignore reads .dockerignore lines.
func parseDockerignore(f *os.File) (*ignoreRules, error) {
	rules := &ignoreRules{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		exclusion := false
		if strings.HasPrefix(line, "!") {
			exclusion = true
			line = strings.TrimSpace(line[1:])
			if line == "" {
				continue
			}
		}
		line = filepath.ToSlash(filepath.Clean(line))
		if len(line) > 1 && line[0] == '/' {
			line = line[1:]
		}
		re, err := regexp.Compile(patternRegexp(line))
		if err != nil {
			return rules, fmt.Errorf("bad .dockerignore pattern %q: %w", line, err)
		}
		rules.patterns = append(rules.patterns, ignorePattern{re: re, exclusion: exclusion})
	}
	return rules, scanner.Err()
}

// patternRegexp translates a .dockerignore pattern to a regexp.
func patternRegexp(p string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
				}
				if i+1 >= len(p) {
					b.WriteString(".*")
				} else {
					b.WriteString("(.*/)?")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[', ']':
			b.WriteByte(c)
		case '\\':
			if i+1 < len(p) {
				i++
				b.WriteString(regexp.QuoteMeta(string(p[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// excludes reports whether rel (slash-separated, relative to the context) is
// left out of the build context.
func (r *ignoreRules) excludes(rel string) bool {
	excluded := false
	parents := strings.Split(rel, "/")
	for _, p := range r.patterns {
		match := p.re.MatchString(rel)
		for i := 1; !match && i < len(parents); i++ {
			match = p.re.MatchString(strings.Join(parents[:i], "/"))
		}
		if match {
			excluded = !p.exclusion
		}
	}
	return excluded
}
