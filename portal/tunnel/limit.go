package tunnel

import (
	"sync"
	"time"
)

// Node agent registrations are not authenticated until the cluster agent
// has reviewed their token, and each review is a request to the cluster
// agent and to its API server. They are rate-limited per client address
// and per cluster before any review is asked for (review-km-44 L6). A
// healthy node registers about once an hour (its token's lifetime, L7) and
// after restarts, so the buckets leave room for many nodes at once.
const (
	nodeRegPerIPBurst       = 6
	nodeRegPerIPEvery       = 10 * time.Second // one more every 10 s
	nodeRegPerClusterBurst  = 30
	nodeRegPerClusterEvery  = time.Second
	nodeRegLimiterMaxBucket = 10000 // the limiter forgets idle buckets beyond this
)

// bucket is a token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter keeps one token bucket per key.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	every   time.Duration
	buckets map[string]*bucket
}

func newLimiter(burst int, every time.Duration) *limiter {
	return &limiter{burst: float64(burst), every: every, buckets: map[string]*bucket{}}
}

// allow takes one token for key, if there is one.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= nodeRegLimiterMaxBucket {
			l.forget(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += float64(now.Sub(b.last)) / float64(l.every)
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// forget drops the buckets that are full again: they limit nothing.
func (l *limiter) forget(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+float64(now.Sub(b.last))/float64(l.every) >= l.burst {
			delete(l.buckets, k)
		}
	}
}
