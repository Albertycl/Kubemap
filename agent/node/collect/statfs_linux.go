package collect

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func statfs(path string) (fsUsage, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return fsUsage{}, err
	}
	bs := uint64(s.Frsize) // statfs block counts are in fragment-size units
	if bs == 0 {
		bs = uint64(s.Bsize)
	}
	return fsUsage{
		total: s.Blocks * bs, free: s.Bfree * bs, avail: s.Bavail * bs,
		inodesTotal: s.Files, inodesFree: s.Ffree,
	}, nil
}

// devOf returns the "major:minor" of the filesystem that holds path, in the
// form mountinfo uses.
func devOf(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev))), nil
}
