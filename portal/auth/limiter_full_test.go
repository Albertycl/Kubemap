package auth

import (
	"fmt"
	"testing"
	"time"
)

// A limiter full of live entries refuses new addresses instead of growing
// (review km-16-fixes N-c); known addresses keep working.
func TestLimiterFull(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	for i := 0; i < maxTracked; i++ {
		l.reserve(fmt.Sprintf("10.0.%d.%d", i/256, i%256), now)
	}
	if ok, _ := l.reserve("192.0.2.1", now); ok {
		t.Fatal("a new address was accepted into a full limiter")
	}
	if len(l.entries) != maxTracked {
		t.Fatalf("%d entries", len(l.entries))
	}
	if ok, _ := l.reserve("10.0.0.1", now); !ok {
		t.Fatal("a known address was refused")
	}
	if ok, _ := l.reserve("192.0.2.1", now.Add(FailureWindow)); !ok {
		t.Fatal("still refused after the window")
	}
}
