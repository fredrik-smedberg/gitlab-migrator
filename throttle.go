package main

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// throttlingTransport is the outermost RoundTripper for the GitHub REST client. It is shared by
// all goroutines, so it enforces process-wide limits:
//   - content-creating requests (POST, PATCH, PUT, DELETE) are spaced out by a rate limiter, to
//     stay within GitHub's secondary rate limits (80 per minute, 500 per hour);
//   - when GitHub answers 403 or 429 with a Retry-After header, all later requests, reads included,
//     wait until that time has passed, since continuing to send requests while rate-limited risks a ban.
//
// Only REST API calls go through this transport. Git pushes use go-git's own HTTP transport and are
// not throttled here.
type throttlingTransport struct {
	base        http.RoundTripper
	writes      *rate.Limiter
	mu          sync.Mutex
	pausedUntil time.Time
}

// newThrottlingTransport wraps base. writesPerHour <= 0 disables the write limiter.
func newThrottlingTransport(base http.RoundTripper, writesPerHour int) *throttlingTransport {
	t := &throttlingTransport{base: base}
	if writesPerHour > 0 {
		t.writes = rate.NewLimiter(rate.Every(time.Hour/time.Duration(writesPerHour)), 1)
	}
	return t
}

func (t *throttlingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	// Wait out any pause set by an earlier rate-limited response. Loop in case the pause was
	// extended while we slept.
	for {
		t.mu.Lock()
		wait := time.Until(t.pausedUntil)
		t.mu.Unlock()
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	switch req.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		if t.writes != nil {
			if err := t.writes.Wait(ctx); err != nil {
				return nil, err
			}
		}
		logger.Debug("github write", "method", req.Method, "path", req.URL.Path)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if seconds, perr := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); perr == nil && seconds > 0 {
			pause := time.Duration(seconds) * time.Second
			until := time.Now().Add(pause)
			t.mu.Lock()
			if until.After(t.pausedUntil) {
				t.pausedUntil = until
			}
			t.mu.Unlock()
			logger.Warn("GitHub rate limit hit, pausing all GitHub API requests", "status", resp.StatusCode, "pause", pause)
		}
	}

	return resp, nil
}
