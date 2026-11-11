//go:build unix

package csi

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// aliased returns a regular file under root that has more than one hard link, or "" if there is
// none. A pull that takes a base over changes its files in place; a second link to one of them
// (a snapshot, a copy made with cp -l) would change with it, outside the artifact and outside
// every lease.
func aliased(root string) (string, error) {
	found := ""
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}
