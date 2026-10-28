package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
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
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("three GETs took %v, want < 200ms", elapsed)
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

func TestRetryAfterPausesAllRequests(t *testing.T) {
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
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("first call status %d, want 403", resp.StatusCode)
	}

	resp, err = tr.RoundTrip(newTestRequest(t, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("GET after Retry-After: 1 started after %v, want >= 900ms", elapsed)
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
