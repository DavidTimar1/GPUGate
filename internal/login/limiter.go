package login

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	maxFailures    = 5
	lockoutWindow  = 60 * time.Second
	failureMemory  = 15 * time.Minute
	maxTracked     = 1000
	backstopCount  = 50
	backstopWindow = 10 * time.Minute
)

type sourceState struct {
	failures    int
	last        time.Time
	lockedUntil time.Time
}

// limiter counts wrong passwords and wrong access keys per source address: maxFailures in a row
// lock that address for lockoutWindow, and failures older than failureMemory are forgotten, so a
// stranger's guesses never lock the owner out. A global backstop (backstopCount failures from
// anywhere in backstopWindow) locks everyone for lockoutWindow, which bounds a spread-out guesser.
type limiter struct {
	mu          sync.Mutex
	sources     map[string]*sourceState
	recent      []time.Time
	globalUntil time.Time
}

func newLimiter() *limiter { return &limiter{sources: map[string]*sourceState{}} }

// sourceOf is the address the connection came from: never a forwarded header, which a caller
// could write to be anyone.
func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// locked answers how long source must still wait, or zero.
func (l *limiter) locked(source string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := l.globalUntil.Sub(now)
	if s := l.sources[source]; s != nil {
		if r := s.lockedUntil.Sub(now); r > remaining {
			remaining = r
		}
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

// fail records one wrong attempt from source. It answers whether this failure locked the address,
// and whether it tripped the global backstop.
func (l *limiter) fail(source string, now time.Time) (locked, backstop bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sources[source]
	if s == nil {
		l.makeRoom(now)
		s = &sourceState{}
		l.sources[source] = s
	}
	if now.Sub(s.last) > failureMemory {
		s.failures = 0
	}
	s.last = now
	s.failures++
	if s.failures >= maxFailures {
		s.failures = 0
		s.lockedUntil = now.Add(lockoutWindow)
		locked = true
	}
	kept := l.recent[:0]
	for _, t := range l.recent {
		if now.Sub(t) <= backstopWindow {
			kept = append(kept, t)
		}
	}
	l.recent = append(kept, now)
	if len(l.recent) >= backstopCount {
		l.recent = nil
		l.globalUntil = now.Add(lockoutWindow)
		backstop = true
	}
	return locked, backstop
}

// succeed forgets source's failures.
func (l *limiter) succeed(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sources, source)
}

// makeRoom keeps the per-address table bounded: stale entries go first, then the oldest.
func (l *limiter) makeRoom(now time.Time) {
	if len(l.sources) < maxTracked {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, s := range l.sources {
		if now.Sub(s.last) > failureMemory && !s.lockedUntil.After(now) {
			delete(l.sources, k)
			continue
		}
		if oldestKey == "" || s.last.Before(oldest) {
			oldestKey, oldest = k, s.last
		}
	}
	if len(l.sources) >= maxTracked && oldestKey != "" {
		delete(l.sources, oldestKey)
	}
}
