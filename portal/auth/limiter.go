package auth

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// MaxFailures failed logins from one client address within FailureWindow
	// block that address until the window ends.
	MaxFailures   = 5
	FailureWindow = 15 * time.Minute
	maxTracked    = 10000

	// SharedMaxFailures is the limit for a "shared" client key: the peer is
	// a trusted proxy and X-Forwarded-For gave no usable client (junk
	// header, or SNAT so every visitor shows the same node address). Many
	// visitors may share that key, so five bad logins must not block it.
	SharedMaxFailures = 50

	// MaxTOTPFailures wrong second-factor codes for one user, from any
	// address, within FailureWindow, refuse second-factor logins for that
	// user until the window ends.
	MaxTOTPFailures = 10
)

// limiter counts failed logins per client address, in memory.
type limiter struct {
	mu      sync.Mutex
	entries map[string]*failures
}

type failures struct {
	count int
	first time.Time
}

func newLimiter() *limiter { return &limiter{entries: map[string]*failures{}} }

// reserve counts one attempt for key before any password is checked, in a
// single locked step, so parallel requests cannot all pass the check. It
// returns false (and when the block ends) if key is already at MaxFailures.
// A successful attempt calls reset; one that never ran calls refund.
func (l *limiter) reserve(key string, now time.Time) (bool, time.Time) {
	_, ok, until := l.reserveN(key, now, MaxFailures)
	return ok, until
}

// reserveN is reserve with its own limit max. It also returns the attempt
// count including this one.
func (l *limiter) reserveN(key string, now time.Time, max int) (int, bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= maxTracked {
		for k, f := range l.entries {
			if !now.Before(f.first.Add(FailureWindow)) {
				delete(l.entries, k)
			}
		}
	}
	f := l.entries[key]
	if f == nil || !now.Before(f.first.Add(FailureWindow)) {
		// Still full of live entries after the sweep: a new address waits
		// instead of growing the map without bound (review km-16-fixes N-c).
		if f == nil && len(l.entries) >= maxTracked {
			return 0, false, now.Add(time.Minute)
		}
		f = &failures{first: now}
		l.entries[key] = f
	}
	if f.count >= max {
		return f.count, false, f.first.Add(FailureWindow)
	}
	f.count++
	return f.count, true, time.Time{}
}

// refund takes back a reserved attempt whose password was never checked.
func (l *limiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.entries[key]; f != nil && f.count > 0 {
		f.count--
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// clientKey is the limiter key of a client address. An IPv6 client usually
// has a whole /64, so the key is its /64 (review KM-44 L13).
func clientKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is6() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// Backoff after failed logins for one username, from any address (review
// KM-44 L13): the per-address limit alone lets many addresses keep
// guessing. A delay, not a block, so nobody can lock the admin out.
const (
	backoffFree = 5                // failures without delay
	backoffMax  = 30 * time.Second // longest delay
)

// backoff counts failed logins per username within FailureWindow.
type backoff struct{ l *limiter }

func newBackoff() *backoff { return &backoff{l: newLimiter()} }

// delay is how long a login for name waits before its password is checked.
func (b *backoff) delay(name string, now time.Time) time.Duration {
	b.l.mu.Lock()
	defer b.l.mu.Unlock()
	f := b.l.entries[name]
	if f == nil || !now.Before(f.first.Add(FailureWindow)) || f.count < backoffFree {
		return 0
	}
	d := time.Second << min(f.count-backoffFree, 5)
	return min(d, backoffMax)
}

// failed counts a failed login for name; succeeded clears it.
func (b *backoff) failed(name string, now time.Time) {
	b.l.mu.Lock()
	defer b.l.mu.Unlock()
	if len(b.l.entries) >= maxTracked {
		for k, f := range b.l.entries {
			if !now.Before(f.first.Add(FailureWindow)) {
				delete(b.l.entries, k)
			}
		}
		if len(b.l.entries) >= maxTracked {
			return
		}
	}
	f := b.l.entries[name]
	if f == nil || !now.Before(f.first.Add(FailureWindow)) {
		f = &failures{first: now}
		b.l.entries[name] = f
	}
	f.count++
}

func (b *backoff) succeeded(name string) { b.l.reset(name) }
