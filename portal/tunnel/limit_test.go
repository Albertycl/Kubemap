package tunnel

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := newLimiter(3, 10*time.Second)
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !l.allow("a", t0) {
			t.Fatalf("burst %d refused", i)
		}
	}
	if l.allow("a", t0) {
		t.Error("the fourth at once must be refused")
	}
	if !l.allow("b", t0) {
		t.Error("another key has its own bucket")
	}
	if l.allow("a", t0.Add(9*time.Second)) || !l.allow("a", t0.Add(10*time.Second)) {
		t.Error("one more token every 10 s")
	}
	// Full buckets are forgotten when the map is full.
	l.forget(t0.Add(time.Hour))
	if len(l.buckets) != 0 {
		t.Errorf("%d buckets left", len(l.buckets))
	}
}
