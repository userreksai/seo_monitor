package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxTrackedLoginIPs   = 10_000
	maxTrackedLoginPairs = 20_000
)

// LoginProtectionConfig controls the in-process failed-login limiter. The IP
// limit stops username spraying, while the pair limit stops repeated guesses
// against one account without globally locking that account for every client.
type LoginProtectionConfig struct {
	IPMaxFailures   int
	PairMaxFailures int
	FailureWindow   time.Duration
	Lockout         time.Duration
	TrustedProxies  []netip.Prefix
}

type loginFailureBucket struct {
	failures     int
	inFlight     int
	windowStart  time.Time
	blockedUntil time.Time
	lastSeen     time.Time
}

type loginLimiter struct {
	mu              sync.Mutex
	ipMaxFailures   int
	pairMaxFailures int
	failureWindow   time.Duration
	lockout         time.Duration
	ipBuckets       map[string]*loginFailureBucket
	pairBuckets     map[string]*loginFailureBucket
	now             func() time.Time
	operations      uint64
}

type loginAttempt struct {
	limiter *loginLimiter
	ipKey   string
	pairKey string
	done    bool
}

func newLoginLimiter(config LoginProtectionConfig) *loginLimiter {
	return &loginLimiter{
		ipMaxFailures:   config.IPMaxFailures,
		pairMaxFailures: config.PairMaxFailures,
		failureWindow:   config.FailureWindow,
		lockout:         config.Lockout,
		ipBuckets:       make(map[string]*loginFailureBucket),
		pairBuckets:     make(map[string]*loginFailureBucket),
		now:             time.Now,
	}
}

// Start reserves capacity before the password hash is checked, so a parallel
// burst cannot run an unbounded number of expensive bcrypt comparisons.
func (l *loginLimiter) Start(ip, username string) (*loginAttempt, time.Duration) {
	now := l.now()
	ipKey := ip
	pairKey := loginPairKey(ip, username)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.operations++
	l.pruneLocked(now)

	ipBucket := l.bucketLocked(l.ipBuckets, ipKey, now)
	pairBucket := l.bucketLocked(l.pairBuckets, pairKey, now)
	if retry := bucketRetryAfter(ipBucket, l.ipMaxFailures, now); retry > 0 {
		return nil, retry
	}
	if retry := bucketRetryAfter(pairBucket, l.pairMaxFailures, now); retry > 0 {
		return nil, retry
	}

	ipBucket.inFlight++
	pairBucket.inFlight++
	return &loginAttempt{limiter: l, ipKey: ipKey, pairKey: pairKey}, 0
}

func (a *loginAttempt) Failure() time.Duration {
	if a == nil || a.done {
		return 0
	}
	a.done = true
	l := a.limiter
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	retry := recordLoginFailure(l.ipBuckets[a.ipKey], l.ipMaxFailures, l.lockout, now)
	if pairRetry := recordLoginFailure(l.pairBuckets[a.pairKey], l.pairMaxFailures, l.lockout, now); pairRetry > retry {
		retry = pairRetry
	}
	return retry
}

func (a *loginAttempt) Success() {
	if a == nil || a.done {
		return
	}
	a.done = true
	l := a.limiter
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	finishLoginAttempt(l.ipBuckets[a.ipKey], now)
	if bucket := l.pairBuckets[a.pairKey]; bucket != nil {
		finishLoginAttempt(bucket, now)
		bucket.failures = 0
		bucket.windowStart = now
		bucket.blockedUntil = time.Time{}
	}
}

// Cancel removes the in-flight reservation without counting infrastructure
// errors (for example a MongoDB outage) as bad credentials.
func (a *loginAttempt) Cancel() {
	if a == nil || a.done {
		return
	}
	a.done = true
	l := a.limiter
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	finishLoginAttempt(l.ipBuckets[a.ipKey], now)
	finishLoginAttempt(l.pairBuckets[a.pairKey], now)
}

func (l *loginLimiter) bucketLocked(buckets map[string]*loginFailureBucket, key string, now time.Time) *loginFailureBucket {
	bucket := buckets[key]
	if bucket == nil {
		bucket = &loginFailureBucket{windowStart: now, lastSeen: now}
		buckets[key] = bucket
		return bucket
	}
	if !bucket.blockedUntil.IsZero() && !now.Before(bucket.blockedUntil) {
		bucket.failures = 0
		bucket.blockedUntil = time.Time{}
		bucket.windowStart = now
	} else if bucket.blockedUntil.IsZero() && now.Sub(bucket.windowStart) >= l.failureWindow {
		bucket.failures = 0
		bucket.windowStart = now
	}
	bucket.lastSeen = now
	return bucket
}

func bucketRetryAfter(bucket *loginFailureBucket, maxFailures int, now time.Time) time.Duration {
	if !bucket.blockedUntil.IsZero() && now.Before(bucket.blockedUntil) {
		return bucket.blockedUntil.Sub(now)
	}
	if bucket.failures+bucket.inFlight >= maxFailures {
		// Existing bcrypt checks may succeed and release capacity shortly.
		return time.Second
	}
	return 0
}

func recordLoginFailure(bucket *loginFailureBucket, maxFailures int, lockout time.Duration, now time.Time) time.Duration {
	if bucket == nil {
		return 0
	}
	finishLoginAttempt(bucket, now)
	bucket.failures++
	if bucket.failures < maxFailures {
		return 0
	}
	bucket.blockedUntil = now.Add(lockout)
	return lockout
}

func finishLoginAttempt(bucket *loginFailureBucket, now time.Time) {
	if bucket == nil {
		return
	}
	if bucket.inFlight > 0 {
		bucket.inFlight--
	}
	bucket.lastSeen = now
}

func (l *loginLimiter) pruneLocked(now time.Time) {
	if l.operations%256 != 0 && len(l.ipBuckets) < maxTrackedLoginIPs && len(l.pairBuckets) < maxTrackedLoginPairs {
		return
	}
	pruneLoginBuckets(l.ipBuckets, maxTrackedLoginIPs, l.failureWindow, now)
	pruneLoginBuckets(l.pairBuckets, maxTrackedLoginPairs, l.failureWindow, now)
}

func pruneLoginBuckets(buckets map[string]*loginFailureBucket, maxEntries int, failureWindow time.Duration, now time.Time) {
	for key, bucket := range buckets {
		if bucket.inFlight == 0 && !now.Before(bucket.blockedUntil) && now.Sub(bucket.lastSeen) >= failureWindow {
			delete(buckets, key)
		}
	}
	for len(buckets) >= maxEntries {
		var oldestKey string
		var oldestTime time.Time
		for key, bucket := range buckets {
			if bucket.inFlight != 0 {
				continue
			}
			if oldestKey == "" || bucket.lastSeen.Before(oldestTime) {
				oldestKey, oldestTime = key, bucket.lastSeen
			}
		}
		if oldestKey == "" {
			return
		}
		delete(buckets, oldestKey)
	}
}

func loginPairKey(ip, username string) string {
	normalized := strings.ToLower(strings.TrimSpace(username))
	digest := sha256.Sum256([]byte(normalized))
	return ip + ":" + hex.EncodeToString(digest[:16])
}

func clientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	peer, ok := parseIP(r.RemoteAddr)
	if !ok {
		return "unknown"
	}
	if !ipIsTrusted(peer, trustedProxies) {
		return peer.String()
	}

	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer.String()
	}
	parts := strings.Split(strings.Join(forwarded, ","), ",")
	for index := len(parts) - 1; index >= 0; index-- {
		address, err := netip.ParseAddr(strings.TrimSpace(parts[index]))
		if err != nil {
			return peer.String()
		}
		address = address.Unmap()
		if !ipIsTrusted(address, trustedProxies) {
			return address.String()
		}
		peer = address
	}
	return peer.String()
}

func parseIP(remoteAddress string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil {
		remoteAddress = host
	}
	address, err := netip.ParseAddr(strings.TrimSpace(remoteAddress))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func ipIsTrusted(address netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func writeLoginRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeError(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后重试")
}
