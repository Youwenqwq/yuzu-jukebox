package auth

import (
	"net"
	"sync"
	"time"
)

const (
	guestPasswordProbeLimit  = 10
	globalPasswordProbeLimit = 100
	passwordlessGuestLimit   = 20
	guestPasswordProbeWindow = 10 * time.Minute
)

type passwordProbeBucket struct {
	count       int
	windowStart time.Time
}

// passwordProbeLimiter enforces independent per-IP limits for incorrect admin
// password probes and passwordless guest logins, plus a server-wide incorrect
// password ceiling. State intentionally remains in memory: a restart clears
// these short-lived bans without introducing stale persistent lockouts.
type passwordProbeLimiter struct {
	mu sync.Mutex

	buckets      map[string]*passwordProbeBucket
	guestBuckets map[string]*passwordProbeBucket

	globalCount       int
	globalWindowStart time.Time

	now         func() time.Time
	lastCleanup time.Time
}

func newPasswordProbeLimiter() *passwordProbeLimiter {
	now := time.Now
	return &passwordProbeLimiter{
		buckets:      make(map[string]*passwordProbeBucket),
		guestBuckets: make(map[string]*passwordProbeBucket),
		now:          now,
		lastCleanup:  now(),
	}
}

// allow records an authentication result and reports whether the request may
// continue. Buckets are fail-closed: once full, even a matching password is
// rejected until its window expires. The global bucket counts wrong probes
// even when their source IP is already blocked.
func (l *passwordProbeLimiter) allow(remoteAddr string, submitted, matched bool) bool {
	now := l.now()
	ip := remoteIP(remoteAddr)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupExpired(now)
	if !submitted {
		bucket := l.activeBucket(l.guestBuckets, ip, now)
		if bucket != nil && bucket.count >= passwordlessGuestLimit {
			return false
		}
		if bucket == nil {
			bucket = &passwordProbeBucket{windowStart: now}
			l.guestBuckets[ip] = bucket
		}
		bucket.count++
		return true
	}

	if !l.globalWindowStart.IsZero() &&
		!now.Before(l.globalWindowStart.Add(guestPasswordProbeWindow)) {
		l.globalCount = 0
		l.globalWindowStart = time.Time{}
	}
	if l.globalCount >= globalPasswordProbeLimit {
		return false
	}
	if !matched {
		if l.globalWindowStart.IsZero() {
			l.globalWindowStart = now
		}
		l.globalCount++
	}

	bucket := l.activeBucket(l.buckets, ip, now)
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

func (l *passwordProbeLimiter) activeBucket(buckets map[string]*passwordProbeBucket, ip string, now time.Time) *passwordProbeBucket {
	bucket := buckets[ip]
	if bucket != nil && !now.Before(bucket.windowStart.Add(guestPasswordProbeWindow)) {
		delete(buckets, ip)
		return nil
	}
	return bucket
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
	for ip, bucket := range l.guestBuckets {
		if !now.Before(bucket.windowStart.Add(guestPasswordProbeWindow)) {
			delete(l.guestBuckets, ip)
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
