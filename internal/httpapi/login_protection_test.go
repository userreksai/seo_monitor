package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func testLoginLimiter(now *time.Time, ipMax, pairMax int) *loginLimiter {
	limiter := newLoginLimiter(LoginProtectionConfig{
		IPMaxFailures: ipMax, PairMaxFailures: pairMax,
		FailureWindow: time.Minute, Lockout: 10 * time.Minute,
	})
	limiter.now = func() time.Time { return *now }
	return limiter
}

func TestLoginLimiterBlocksPairThenIP(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	limiter := testLoginLimiter(&now, 3, 2)

	first, retry := limiter.Start("203.0.113.10", "admin")
	if first == nil || retry != 0 || first.Failure() != 0 {
		t.Fatal("first failed login should be allowed")
	}
	second, retry := limiter.Start("203.0.113.10", "ADMIN")
	if second == nil || retry != 0 {
		t.Fatal("second login in the normalized IP/account pair should start")
	}
	if retry = second.Failure(); retry != 10*time.Minute {
		t.Fatalf("pair retry = %s, want 10m", retry)
	}
	if attempt, retry := limiter.Start("203.0.113.10", "admin"); attempt != nil || retry != 10*time.Minute {
		t.Fatalf("blocked pair returned attempt=%v retry=%s", attempt, retry)
	}

	third, retry := limiter.Start("203.0.113.10", "another-user")
	if third == nil || retry != 0 {
		t.Fatal("another account should be allowed until the IP threshold is reached")
	}
	if retry = third.Failure(); retry != 10*time.Minute {
		t.Fatalf("IP retry = %s, want 10m", retry)
	}
	if attempt, retry := limiter.Start("203.0.113.10", "third-user"); attempt != nil || retry != 10*time.Minute {
		t.Fatalf("blocked IP returned attempt=%v retry=%s", attempt, retry)
	}
	if attempt, retry := limiter.Start("203.0.113.11", "admin"); attempt == nil || retry != 0 {
		t.Fatal("a different IP should have independent capacity")
	} else {
		attempt.Cancel()
	}

	now = now.Add(10*time.Minute + time.Second)
	if attempt, retry := limiter.Start("203.0.113.10", "admin"); attempt == nil || retry != 0 {
		t.Fatal("expired lockout should release both buckets")
	} else {
		attempt.Cancel()
	}
}

func TestLoginLimiterSuccessClearsPairFailures(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	limiter := testLoginLimiter(&now, 10, 2)

	failed, _ := limiter.Start("203.0.113.20", "admin")
	failed.Failure()
	succeeded, retry := limiter.Start("203.0.113.20", "admin")
	if succeeded == nil || retry != 0 {
		t.Fatal("correct login should be able to use the remaining pair capacity")
	}
	succeeded.Success()

	again, _ := limiter.Start("203.0.113.20", "admin")
	if retry := again.Failure(); retry != 0 {
		t.Fatalf("first failure after success unexpectedly locked pair for %s", retry)
	}
}

func TestLoginLimiterReservesParallelCapacity(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	limiter := testLoginLimiter(&now, 10, 2)
	first, _ := limiter.Start("203.0.113.30", "admin")
	second, _ := limiter.Start("203.0.113.30", "admin")
	if third, retry := limiter.Start("203.0.113.30", "admin"); third != nil || retry != time.Second {
		t.Fatalf("parallel overflow returned attempt=%v retry=%s", third, retry)
	}
	first.Cancel()
	second.Cancel()
}

func TestClientIPIgnoresForwardingHeaderFromUntrustedPeer(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	request.RemoteAddr = "198.51.100.20:4567"
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if got := clientIP(request, trusted); got != "198.51.100.20" {
		t.Fatalf("clientIP = %q, want direct peer", got)
	}
}

func TestClientIPWalksTrustedProxyChainFromRight(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	request.RemoteAddr = "127.0.0.1:4567"
	request.Header.Set("X-Forwarded-For", "192.0.2.44, 10.0.0.8")
	trusted := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}
	if got := clientIP(request, trusted); got != "192.0.2.44" {
		t.Fatalf("clientIP = %q, want original untrusted client", got)
	}
}

func TestClientIPRejectsMalformedTrustedHeader(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	request.RemoteAddr = "127.0.0.1:4567"
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if got := clientIP(request, trusted); got != "127.0.0.1" {
		t.Fatalf("clientIP = %q, want safe peer fallback", got)
	}
}

func TestSecureSessionCookie(t *testing.T) {
	server := &Server{authCookieSecure: true}
	recorder := httptest.NewRecorder()
	expires := time.Now().Add(time.Hour)
	server.setSessionCookie(recorder, "secret-token", expires)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value != "secret-token" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("unexpected session cookie: %#v", cookie)
	}
	if cookie.SameSite != 3 || cookie.Path != "/api/" {
		t.Fatalf("unexpected session cookie scope: %#v", cookie)
	}
}
