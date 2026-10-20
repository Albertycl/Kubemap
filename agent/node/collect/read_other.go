//go:build !linux

package collect

import "os"

// readSmall reads up to 4 KiB of path into buf, reused between calls.
func readSmall(path string, buf *[]byte) ([]byte, error) {
	if cap(*buf) < 4096 {
		*buf = make([]byte, 0, 4096)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.Read((*buf)[:4096])
	if n == 0 && err != nil {
		return nil, err
	}
	return (*buf)[:n], nil
}
