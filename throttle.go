package main

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// throttlingTransport is the HTTP transport below the GitHub retry client, so every attempt,
// retries included, passes through it. It is shared by all goroutines, so it enforces
// process-wide limits:
//   - content-creating requests (POST, PATCH, PUT, DELETE) are spaced out by a rate limiter, to
//     stay within GitHub's secondary rate limits (80 per minute, 500 per hour);
//   - when GitHub answers 403 or 429 with a Retry-After header, all later requests, reads included,
//     wait until that time has passed, since continuing to send requests while rate-limited risks a ban;
//   - when a response says the primary rate limit is exhausted (X-Ratelimit-Remaining: 0), all later
//     requests wait until X-Ratelimit-Reset plus a margin.
//
// The "github write" Debug line is logged per attempt, which is also what GitHub's rate limit counts.
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

	if err := t.waitForPause(ctx); err != nil {
		return nil, err
	}

	switch req.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		if t.writes != nil {
			if err := t.writes.Wait(ctx); err != nil {
				return nil, err
			}
			// Another request may have hit a rate limit while we waited for the limiter.
			if err := t.waitForPause(ctx); err != nil {
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
			t.pauseUntil(time.Now().Add(pause))
			logger.Warn("GitHub rate limit hit, pausing all GitHub API requests", "status", resp.StatusCode, "pause", pause)
		}
	}

	// The primary rate limit is exhausted: wait until it resets, with a margin for clock skew.
	if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		if reset, perr := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); perr == nil && reset > 0 {
			until := time.Unix(reset, 0).Add(rateLimitResetMargin)
			t.pauseUntil(until)
			logger.Warn("GitHub rate limit exhausted, pausing all GitHub API requests until reset", "status", resp.StatusCode, "until", until)
		}
	}

	return resp, nil
}

// rateLimitResetMargin is added to X-Ratelimit-Reset before requests resume.
const rateLimitResetMargin = 30 * time.Second

// pauseUntil pauses all requests until the given time. An existing longer pause is kept.
func (t *throttlingTransport) pauseUntil(until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until.After(t.pausedUntil) {
		t.pausedUntil = until
	}
}

// waitForPause waits out any pause set by an earlier rate-limited response. It loops in case
// the pause was extended while it slept.
func (t *throttlingTransport) waitForPause(ctx context.Context) error {
	for {
		t.mu.Lock()
		wait := time.Until(t.pausedUntil)
		t.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
