package auth

import (
	"sync"
	"time"
)

// SendLimiter caps how many sign-in emails go out, so the login form can't
// be used to flood someone's inbox or burn the mail server's reputation.
// The limits are bserver's: per address, per client IP, and overall.
//
// The counts live in memory on the node serving the form. That's enough
// while one VPS serves everything (M0); with several web nodes each keeps
// its own counts, which loosens the limits by that factor and is still a
// cap.
type SendLimiter struct {
	PerAddress int // per email address per hour (5)
	PerIP      int // per client IP per hour (20)
	Global     int // overall per hour (60)

	mu     sync.Mutex
	window time.Time // start of the current hour's window
	byAddr map[string]int
	byIP   map[string]int
	total  int
}

// NewSendLimiter returns a limiter with bserver's limits.
func NewSendLimiter() *SendLimiter {
	return &SendLimiter{PerAddress: 5, PerIP: 20, Global: 60}
}

// Allow counts one send and reports whether it's within the limits. A
// refused send isn't counted.
//
// It uses fixed one-hour windows rather than a sliding window: simpler, and
// the worst case (a burst either side of the boundary) is twice the limit
// for a moment, which is still a hard cap.
func (l *SendLimiter) Allow(email, ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) >= time.Hour || l.byAddr == nil {
		l.window = now
		l.byAddr = map[string]int{}
		l.byIP = map[string]int{}
		l.total = 0
	}
	if l.byAddr[email] >= l.PerAddress || l.byIP[ip] >= l.PerIP || l.total >= l.Global {
		return false
	}
	l.byAddr[email]++
	l.byIP[ip]++
	l.total++
	return true
}
