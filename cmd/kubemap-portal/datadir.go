package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// secureDataDir keeps the data folder private to the server's user, on every
// start and in every command that opens it: new files are created without
// group or other access (umask 077), and the folder and everything already in
// it are set to 0700 and 0600. A file that was readable by others is
// reported, because it may have been copied.
func secureDataDir(dir string) ([]string, error) {
	syscall.Umask(0o077)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var warnings []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow a link out of the folder
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		mode := info.Mode().Perm()
		if mode&0o077 == 0 {
			return nil
		}
		warnings = append(warnings, fmt.Sprintf("%s had mode %o; now %o", p, mode, want))
		if err := os.Chmod(p, want); err != nil {
			return fmt.Errorf("make %s private: %w", p, err)
		}
		return nil
	})
	return warnings, err
}
