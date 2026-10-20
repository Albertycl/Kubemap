package logfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "portal.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	for _, name := range []string{"portal.log", "portal.log.1", "portal.log.2"} {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b) == 0 || len(b) > 100 {
			t.Fatalf("%s has %d bytes", name, len(b))
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept more than 2 rotated files")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
}

func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.log")
	w, _ := Open(path, 0, 0)
	w.Write([]byte("one\n"))
	w.Close()
	w, _ = Open(path, 0, 0)
	w.Write([]byte("two\n"))
	w.Close()
	if b, _ := os.ReadFile(path); string(b) != "one\ntwo\n" {
		t.Fatalf("content %q", b)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close succeeded")
	}
}

func TestConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.log")
	w, _ := Open(path, 1000, 3)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Write([]byte("0123456789\n"))
			}
		}()
	}
	wg.Wait()
	w.Close()
	total := 0
	for _, name := range []string{"portal.log", "portal.log.1", "portal.log.2", "portal.log.3"} {
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if strings.Count(string(b), "\n")*11 != len(b) {
			t.Fatalf("%s has a torn line", name)
		}
		total += len(b)
	}
	if total == 0 {
		t.Fatal("nothing written")
	}
}

// Review KM-19 N2: when the file cannot be renamed (on Windows: another
// program holds it open), logging goes on in the same file, and rotation is
// tried again a minute later.
func TestRotationFailureKeepsLogging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.log")
	// A non-empty folder named portal.log.1 makes the rename fail on any OS.
	if err := os.MkdirAll(filepath.Join(path+".1", "busy"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	line := strings.Repeat("x", 39) + "\n"
	for i := 0; i < 5; i++ { // 200 bytes: the rotation at 100 fails
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %d after a failed rotation: %v", i, err)
		}
	}
	if b, _ := os.ReadFile(path); len(b) != 200 {
		t.Fatalf("log has %d bytes, want all 200 in the current file", len(b))
	}

	// The other program lets go; a minute later the rotation works.
	os.RemoveAll(path + ".1")
	clock = clock.Add(30 * time.Second)
	w.Write([]byte(line))
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("rotation retried before the minute was over")
	}
	clock = clock.Add(31 * time.Second)
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path + ".1"); len(b) != 240 {
		t.Fatalf("rotated file has %d bytes, want 240", len(b))
	}
	if b, _ := os.ReadFile(path); string(b) != line {
		t.Fatalf("new file %q", b)
	}
}
