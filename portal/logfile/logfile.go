// Package logfile is a size-rotated log file for the portal when it is asked to
// log to a file instead of the terminal.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// retryRotation is how long the writer keeps appending to the current file
// after a rotation failed, before it tries again.
const retryRotation = time.Minute

const (
	// DefaultMaxBytes is the size at which the file is rotated.
	DefaultMaxBytes = 10 << 20
	// DefaultKeep is how many rotated files are kept (portal.log.1 ... .N).
	DefaultKeep = 5
)

// Writer appends to path and rotates it when it would grow past MaxBytes:
// path.N-1 becomes path.N, ..., path becomes path.1. It is safe for
// concurrent use.
type Writer struct {
	path     string
	maxBytes int64
	keep     int

	mu      sync.Mutex
	f       *os.File
	size    int64
	retryAt time.Time        // no rotation before this, after one failed
	now     func() time.Time // tests replace it
}

// Open opens (or creates) the log file with mode 0600, creating its folder.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep < 1 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("logfile: %w", err)
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep, now: time.Now}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logfile: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logfile: %w", err)
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write appends p, rotating first if needed. A single write larger than
// MaxBytes still goes into one file.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes && !w.now().Before(w.retryAt) {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("logfile: %w", err)
	}
	w.f = nil
	os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// The rename can fail while another program holds the file open. Keep appending
		// to it and try again later, rather than stop logging until the
		// next restart (review KM-19 N2).
		w.retryAt = w.now().Add(retryRotation)
	}
	return w.open()
}

// Close closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
