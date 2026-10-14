//go:build !unix

package execas

import "io/fs"

// mayExecute has no owner to compare against here: any execute bit counts.
func mayExecute(fi fs.FileInfo, _ Identity) bool {
	return fi.Mode().Perm()&0o111 != 0
}
