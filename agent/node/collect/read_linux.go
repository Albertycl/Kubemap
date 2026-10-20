package collect

import "syscall"

// readSmall reads a small /proc file into buf, which is reused between
// calls. Files are cut at 4 KiB; stat and cgroup lines are far shorter. It
// uses raw syscalls: with hundreds of processes per sample, os.File's
// setup cost is a large part of the scan.
func readSmall(path string, buf *[]byte) ([]byte, error) {
	if cap(*buf) < 4096 {
		*buf = make([]byte, 0, 4096)
	}
	b := (*buf)[:4096]
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	n, err := syscall.Read(fd, b)
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}
