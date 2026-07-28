package wsapi

import (
	"net"
	"sync"
	"time"
)

const (
	accessProbeLimit  = 10
	accessProbeWindow = 10 * time.Minute
)

type accessProbeKey struct {
	roomID string
	ip     string
}

type accessProbeBucket struct {
	count       int
	windowStart time.Time
}

type accessProbeLimiter struct {
	mu          sync.Mutex
	buckets     map[accessProbeKey]*accessProbeBucket
	now         func() time.Time
	lastCleanup time.Time
}

func newAccessProbeLimiter() *accessProbeLimiter {
	now := time.Now()
	return &accessProbeLimiter{
		buckets:     make(map[accessProbeKey]*accessProbeBucket),
		now:         time.Now,
		lastCleanup: now,
	}
}

// allow records a failed credential probe, resets a successful one, and rejects
// attempts after the failure threshold for the same Room and source IP.
func (l *accessProbeLimiter) allow(roomID, remoteAddr string, matched bool) bool {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr
	}
	key := accessProbeKey{roomID: roomID, ip: ip}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.lastCleanup.Add(accessProbeWindow)) {
		for bucketKey, candidate := range l.buckets {
			if !now.Before(candidate.windowStart.Add(accessProbeWindow)) {
				delete(l.buckets, bucketKey)
			}
		}
		l.lastCleanup = now
	}
	bucket := l.buckets[key]
	if bucket != nil && !now.Before(bucket.windowStart.Add(accessProbeWindow)) {
		delete(l.buckets, key)
		bucket = nil
	}
	if matched {
		delete(l.buckets, key)
		return true
	}
	if bucket != nil && bucket.count >= accessProbeLimit {
		return false
	}
	if bucket == nil {
		bucket = &accessProbeBucket{windowStart: now}
		l.buckets[key] = bucket
	}
	bucket.count++
	return true
}
