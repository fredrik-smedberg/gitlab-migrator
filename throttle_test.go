package main

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/time/rate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func okResponse(status int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

func newTestRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "https://api.github.com/repos/o/r/pulls", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func ensureTestLogger() {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
}

func TestWritesAreRateLimited(t *testing.T) {
	ensureTestLogger()
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK, nil), nil
	})
	tr := newThrottlingTransport(stub, 3600) // one write per second

	start := time.Now()
	for i := 0; i < 3; i++ {
		resp, err := tr.RoundTrip(newTestRequest(t, http.MethodGet))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("three GETs took %v, want < 1s", elapsed)
	}

	start = time.Now()
	for i := 0; i < 3; i++ {
		resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if elapsed := time.Since(start); elapsed < 1800*time.Millisecond {
		t.Fatalf("three POSTs took %v, want >= 1.8s", elapsed)
	}
}

func TestRetryAfterPausesAllRequestsAndRetries(t *testing.T) {
	ensureTestLogger()
	var calls atomic.Int32
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return okResponse(http.StatusForbidden, http.Header{"Retry-After": []string{"1"}}), nil
		}
		return okResponse(http.StatusOK, nil), nil
	})
	tr := newThrottlingTransport(stub, 0)

	start := time.Now()
	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 from the retried request", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d calls, want 2 (rate-limited attempt plus retry)", calls.Load())
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retry after Retry-After: 1 happened after %v, want >= 900ms", elapsed)
	}

	// Later requests, reads included, are not delayed any more once the pause has passed.
	start = time.Now()
	resp, err = tr.RoundTrip(newTestRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("GET after the pause took %v", elapsed)
	}
}

func TestRateLimitMessageWithoutRetryAfterPausesDefault(t *testing.T) {
	ensureTestLogger()
	defer func(d time.Duration) { defaultRateLimitPause = d }(defaultRateLimitPause)
	defaultRateLimitPause = 300 * time.Millisecond

	var calls atomic.Int32
	var bodies []string
	stub := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) == 1 {
			resp := okResponse(http.StatusForbidden, nil)
			resp.Body = io.NopCloser(strings.NewReader(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`))
			return resp, nil
		}
		return okResponse(http.StatusCreated, nil), nil
	})
	tr := newThrottlingTransport(stub, 0)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/repos/o/r/issues/1/comments", strings.NewReader(`{"body":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("retry happened after %v, want >= 250ms (default pause)", elapsed)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"body":"hi"}` {
		t.Fatalf("request body must be replayed on retry, got %q", bodies)
	}
}

func TestNonRateLimit403IsReturned(t *testing.T) {
	ensureTestLogger()
	var calls atomic.Int32
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := okResponse(http.StatusForbidden, nil)
		resp.Body = io.NopCloser(strings.NewReader(`{"message":"Resource not accessible by personal access token"}`))
		return resp, nil
	})
	tr := newThrottlingTransport(stub, 0)
	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("status %d calls %d, want the 403 returned once", resp.StatusCode, calls.Load())
	}
	if !strings.Contains(string(body), "Resource not accessible") {
		t.Fatalf("body must be restored after inspection, got %q", body)
	}
	if !tr.pausedUntil.IsZero() {
		t.Fatalf("a permission 403 must not pause requests")
	}
}

func TestRateLimitResetPausesAllRequests(t *testing.T) {
	ensureTestLogger()
	reset := time.Now().Add(time.Minute).Unix()
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK, http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{strconv.FormatInt(reset, 10)},
		}), nil
	})
	tr := newThrottlingTransport(stub, 0)

	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	tr.mu.Lock()
	pausedUntil := tr.pausedUntil
	tr.mu.Unlock()
	if want := time.Unix(reset, 0).Add(30 * time.Second); !pausedUntil.Equal(want) {
		t.Fatalf("paused until %v, want %v", pausedUntil, want)
	}

	// A shorter pause must not shorten the existing one.
	tr.pauseUntil(time.Now().Add(time.Second))
	tr.mu.Lock()
	if !tr.pausedUntil.Equal(pausedUntil) {
		t.Fatalf("pause was shortened to %v", tr.pausedUntil)
	}
	tr.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := newTestRequest(t, http.MethodGet).WithContext(ctx)
	if _, err := tr.RoundTrip(req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request during pause returned %v, want context.DeadlineExceeded", err)
	}
}

func TestRateLimitResetIgnoredWhenRemaining(t *testing.T) {
	ensureTestLogger()
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK, http.Header{
			"X-Ratelimit-Remaining": []string{"10"},
			"X-Ratelimit-Reset":     []string{strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
		}), nil
	})
	tr := newThrottlingTransport(stub, 0)

	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !tr.pausedUntil.IsZero() {
		t.Fatalf("paused until %v, want no pause", tr.pausedUntil)
	}
}

func TestPauseSetDuringWriteWaitIsHonoured(t *testing.T) {
	ensureTestLogger()
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK, nil), nil
	})
	tr := newThrottlingTransport(stub, 3600) // one write per second

	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	// Let the second write pass the first pause check and block in the limiter, then pause.
	time.Sleep(200 * time.Millisecond)
	tr.pauseUntil(start.Add(1500 * time.Millisecond))

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 1400*time.Millisecond {
		t.Fatalf("second write finished after %v, want >= 1.4s (pause set during limiter wait)", elapsed)
	}
}

func TestRateLimitedWriteLowersWriteLimitOncePerInterval(t *testing.T) {
	ensureTestLogger()
	var calls atomic.Int32
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if n := calls.Add(1); n == 1 || n == 2 {
			return okResponse(http.StatusForbidden, http.Header{"Retry-After": []string{"1"}}), nil
		}
		return okResponse(http.StatusOK, nil), nil
	})
	tr := newThrottlingTransport(stub, 3600)
	before := tr.writes.Limit()

	resp, err := tr.RoundTrip(newTestRequest(t, http.MethodPost))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("%d calls, want 3 (two rate-limited attempts, then success)", calls.Load())
	}
	// Two rate-limited attempts within rateLowerInterval halve the limit once, not twice.
	if got, want := tr.writes.Limit(), before/2; math.Abs(float64(got-want)) > 1e-9 {
		t.Fatalf("write limit after rate-limited POST = %v, want %v", got, want)
	}

	// A rate-limited read leaves the write limit alone.
	calls.Store(0)
	tr.mu.Lock()
	tr.pausedUntil = time.Time{}
	tr.mu.Unlock()
	stub2 := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return okResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"1"}}), nil
		}
		return okResponse(http.StatusOK, nil), nil
	})
	tr.base = stub2
	resp, err = tr.RoundTrip(newTestRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got, want := tr.writes.Limit(), before/2; math.Abs(float64(got-want)) > 1e-9 {
		t.Fatalf("write limit after rate-limited GET = %v, want %v", got, want)
	}
}

func TestWriteRateRecoversAfterQuietPeriod(t *testing.T) {
	ensureTestLogger()
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK, nil), nil
	})
	tr := newThrottlingTransport(stub, 36000) // ten per second, so the test is quick
	tr.writes.SetLimit(tr.configured / 4)
	lowered := tr.writes.Limit()

	// Recently limited: no raise.
	tr.lastLimited = time.Now()
	resp, _ := tr.RoundTrip(newTestRequest(t, http.MethodPost))
	_ = resp.Body.Close()
	if tr.writes.Limit() != lowered {
		t.Fatalf("limit raised too early")
	}

	// Quiet for longer than rateRecoverInterval: raised by rateRecoverFactor, once per interval.
	tr.lastLimited = time.Now().Add(-2 * rateRecoverInterval)
	tr.lastRaised = time.Time{}
	resp, _ = tr.RoundTrip(newTestRequest(t, http.MethodPost))
	_ = resp.Body.Close()
	if got, want := tr.writes.Limit(), lowered*rateRecoverFactor; math.Abs(float64(got-want)) > 1e-9 {
		t.Fatalf("limit after quiet period = %v, want %v", got, want)
	}
	resp, _ = tr.RoundTrip(newTestRequest(t, http.MethodPost))
	_ = resp.Body.Close()
	if got, want := tr.writes.Limit(), lowered*rateRecoverFactor; math.Abs(float64(got-want)) > 1e-9 {
		t.Fatalf("limit raised twice within one interval: %v", got)
	}

	// Never above the configured limit.
	tr.writes.SetLimit(tr.configured / 1.1)
	tr.lastRaised = time.Time{}
	resp, _ = tr.RoundTrip(newTestRequest(t, http.MethodPost))
	_ = resp.Body.Close()
	if tr.writes.Limit() > tr.configured {
		t.Fatalf("limit %v exceeds configured %v", tr.writes.Limit(), tr.configured)
	}
}

func TestWriteLimitFloor(t *testing.T) {
	ensureTestLogger()
	tr := newThrottlingTransport(roundTripFunc(nil), 150)
	tr.lowerWriteRate()
	tr.lastLimited = time.Time{}
	tr.lowerWriteRate()
	if got, want := tr.writes.Limit(), rate.Every(time.Hour/minWritesPerHour); math.Abs(float64(got-want)) > 1e-12 {
		t.Fatalf("write limit = %v, want floor %v", got, want)
	}
}
