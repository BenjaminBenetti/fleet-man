package execas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// lookPath finds file the way the command's user would: a name with a slash is
// used as given (relative to dir), a bare name is searched on pathEnv (an
// empty entry is dir itself), and only a regular file id may execute counts.
// The failures carry docker's exit codes: 127 when there is nothing to run,
// 126 when it may not be run.
func lookPath(file, pathEnv, dir string, id Identity) (string, error) {
	if strings.Contains(file, "/") {
		fi, err := os.Stat(inDir(dir, file))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", notFound(fmt.Errorf("exec: %q: no such file or directory", file))
		case err != nil:
			return "", cannotRun(fmt.Errorf("exec: %q: %w", file, err))
		case !fi.Mode().IsRegular() || !mayExecute(fi, id):
			return "", cannotRun(fmt.Errorf("exec: %q: permission denied", file))
		}
		return file, nil
	}
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		candidate := filepath.Join(d, file)
		fi, err := os.Stat(inDir(dir, candidate))
		if err == nil && fi.Mode().IsRegular() && mayExecute(fi, id) {
			if !filepath.IsAbs(candidate) && !strings.HasPrefix(candidate, ".") {
				candidate = "./" + candidate
			}
			return candidate, nil
		}
	}
	return "", notFound(fmt.Errorf("exec: %q: executable file not found in $PATH", file))
}

// inDir is p as seen from dir.
func inDir(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
