package auth

import (
	"net"
	"sync"
	"time"
)

const (
	guestPasswordProbeLimit  = 10
	guestPasswordProbeWindow = 10 * time.Minute
)

type passwordProbeBucket struct {
	count       int
	windowStart time.Time
}

// passwordProbeLimiter limits only non-empty, incorrect admin-password probes.
// Its buckets intentionally remain in memory: a restart clears short-lived bans,
// and persisting them would add stale lockouts and database work to authentication.
type passwordProbeLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*passwordProbeBucket
	now         func() time.Time
	lastCleanup time.Time
}

func newPasswordProbeLimiter() *passwordProbeLimiter {
	now := time.Now
	return &passwordProbeLimiter{
		buckets:     make(map[string]*passwordProbeBucket),
		now:         now,
		lastCleanup: now(),
	}
}

// allow records an authentication result and reports whether the request may
// continue. The tenth probe is accepted as an ordinary guest login; subsequent
// password-bearing requests from that IP are rejected until the window expires.
// Passwordless guest authentication bypasses the mutex and never affects a bucket.
func (l *passwordProbeLimiter) allow(remoteAddr string, submitted, matched bool) bool {
	if !submitted {
		return true
	}

	now := l.now()
	ip := remoteIP(remoteAddr)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupExpired(now)
	bucket := l.buckets[ip]
	if bucket != nil && !now.Before(bucket.windowStart.Add(guestPasswordProbeWindow)) {
		delete(l.buckets, ip)
		bucket = nil
	}
	if bucket != nil && bucket.count >= guestPasswordProbeLimit {
		return false
	}
	if matched {
		delete(l.buckets, ip)
		return true
	}
	if bucket == nil {
		bucket = &passwordProbeBucket{windowStart: now}
		l.buckets[ip] = bucket
	}
	bucket.count++
	return true
}

func (l *passwordProbeLimiter) cleanupExpired(now time.Time) {
	if now.Before(l.lastCleanup.Add(guestPasswordProbeWindow)) {
		return
	}
	for ip, bucket := range l.buckets {
		if !now.Before(bucket.windowStart.Add(guestPasswordProbeWindow)) {
			delete(l.buckets, ip)
		}
	}
	l.lastCleanup = now
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}
