package main

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// throttlingTransport is the HTTP transport below the GitHub retry client, so every attempt,
// retries included, passes through it. It is shared by all goroutines, so it enforces
// process-wide limits:
//   - content-creating requests (POST, PATCH, PUT, DELETE) are spaced out by a rate limiter, to
//     stay within GitHub's secondary rate limits (80 per minute, 500 per hour);
//   - a rate-limited response (429, or 403 with Retry-After, X-Ratelimit-Remaining: 0, or a
//     "rate limit" message) pauses all later requests, reads included, until Retry-After or the
//     reset time has passed, or for defaultRateLimitPause when GitHub gives no time, since
//     continuing to send requests while rate-limited risks a ban;
//   - the rate-limited request itself is sent again after the pause, as often as needed. A rate
//     limit is a wait, never a failure, so the retry client's attempt budget is kept for real errors;
//   - when a write is rate-limited, the write limiter rate is halved (at most once per
//     rateLowerInterval, down to minWritesPerHour), so a too high setting corrects itself; after
//     rateRecoverInterval without rate limits it is raised again step by step, up to the setting.
//
// The "github write" Debug line is logged per attempt, which is also what GitHub's rate limit counts.
//
// Only REST API calls go through this transport. Git pushes use go-git's own HTTP transport and are
// not throttled here.
type throttlingTransport struct {
	base   http.RoundTripper
	writes *rate.Limiter
	// configured is the write limit from the flag; the limiter never exceeds it.
	configured rate.Limit

	mu          sync.Mutex
	pausedUntil time.Time
	lastLimited time.Time // last time a write was rate-limited (or the limit lowered)
	lastRaised  time.Time
}

const (
	// minWritesPerHour is the floor for lowerWriteRate.
	minWritesPerHour = 250
	// rateLowerInterval is the shortest time between two halvings of the write limit, so a burst
	// of rate-limited retries during one pause does not collapse the limit to the floor.
	rateLowerInterval = 2 * time.Minute
	// rateRecoverInterval is the time without rate limits after which the write limit is raised.
	rateRecoverInterval = 10 * time.Minute
	// rateRecoverFactor is applied at each raise.
	rateRecoverFactor = 1.25
	// rateLimitResetMargin is added to X-Ratelimit-Reset before requests resume.
	rateLimitResetMargin = 30 * time.Second
	// rateLimitBodyLimit is how much of a 403 body is read to look for a rate limit message.
	rateLimitBodyLimit = 64 << 10
)

// defaultRateLimitPause is used when a rate-limited response gives no Retry-After or reset time.
// GitHub's guidance is to wait at least one minute. A variable so tests can shorten it.
var defaultRateLimitPause = time.Minute

// newThrottlingTransport wraps base. writesPerHour <= 0 disables the write limiter.
func newThrottlingTransport(base http.RoundTripper, writesPerHour int) *throttlingTransport {
	t := &throttlingTransport{base: base}
	if writesPerHour > 0 {
		t.configured = rate.Every(time.Hour / time.Duration(writesPerHour))
		t.writes = rate.NewLimiter(t.configured, 1)
	}
	return t
}

func (t *throttlingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	isWrite := false
	switch req.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		isWrite = true
	}

	// Keep the body replayable so a rate-limited request can be sent again.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		data, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req = req.Clone(ctx)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		req.Body, _ = req.GetBody()
		req.ContentLength = int64(len(data))
	}

	for attempt := 1; ; attempt++ {
		if err := t.waitForPause(ctx); err != nil {
			return nil, err
		}
		if isWrite && t.writes != nil {
			if err := t.writes.Wait(ctx); err != nil {
				return nil, err
			}
			// Another request may have hit a rate limit while we waited for the limiter.
			if err := t.waitForPause(ctx); err != nil {
				return nil, err
			}
		}
		if isWrite {
			logger.Debug("github write", "method", req.Method, "path", req.URL.Path, "attempt", attempt)
		}

		resp, err := t.base.RoundTrip(req)
		if err != nil || resp == nil {
			return resp, err
		}

		limited, until, reason := rateLimitInfo(resp)
		if !limited {
			if !until.IsZero() {
				// The primary rate limit is exhausted although this request succeeded: wait for the reset.
				t.pauseUntil(until)
				logger.Warn("GitHub rate limit exhausted, pausing all GitHub API requests until reset", "until", until)
			} else if isWrite {
				t.maybeRaiseWriteRate()
			}
			return resp, nil
		}

		if until.IsZero() {
			until = time.Now().Add(defaultRateLimitPause)
		}
		t.pauseUntil(until)
		if isWrite {
			t.lowerWriteRate()
		}
		logger.Warn("GitHub rate limit hit, pausing all GitHub API requests and retrying", "status", resp.StatusCode, "reason", reason, "method", req.Method, "path", req.URL.Path, "pause", time.Until(until).Round(time.Second), "attempt", attempt)

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				// Cannot replay the body; hand the response to the retry client instead.
				return resp, nil
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(ctx)
			req.Body = body
		}
	}
}

// rateLimitInfo reports whether resp is a rate-limited response, until when requests should
// pause (zero when GitHub gave no time), and a short reason for the log. For a 403 the body is
// inspected for a rate limit message and then restored. A successful response with
// X-Ratelimit-Remaining: 0 is not limited but returns the reset time.
func rateLimitInfo(resp *http.Response) (limited bool, until time.Time, reason string) {
	retryAfter := resp.Header.Get("Retry-After")
	remainingZero := resp.Header.Get("X-Ratelimit-Remaining") == "0"
	if reset, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil && reset > 0 && remainingZero {
		until = time.Unix(reset, 0).Add(rateLimitResetMargin)
	}
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
		until = time.Now().Add(time.Duration(seconds) * time.Second)
	}

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true, until, "429"
	case http.StatusForbidden:
		switch {
		case retryAfter != "":
			return true, until, "retry-after"
		case remainingZero:
			return true, until, "rate limit exhausted"
		}
		if resp.Body != nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, rateLimitBodyLimit))
			rest, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), bytes.NewReader(rest)))
			if strings.Contains(strings.ToLower(string(data)), "rate limit") {
				msg := strings.Join(strings.Fields(string(data)), " ")
				if len(msg) > 160 {
					msg = msg[:160] + "…"
				}
				return true, until, msg
			}
		}
	}
	return false, until, ""
}

// lowerWriteRate halves the write limiter rate, down to minWritesPerHour, at most once per
// rateLowerInterval. It does nothing when the write limiter is disabled.
func (t *throttlingTransport) lowerWriteRate() {
	if t.writes == nil {
		return
	}

	t.mu.Lock()
	now := time.Now()
	if now.Sub(t.lastLimited) < rateLowerInterval {
		t.lastLimited = now
		t.mu.Unlock()
		return
	}
	t.lastLimited = now
	limit := t.writes.Limit() / 2
	if floor := rate.Every(time.Hour / minWritesPerHour); limit < floor {
		limit = floor
	}
	t.writes.SetLimit(limit)
	t.mu.Unlock()

	logger.Warn("GitHub rate-limited a write, lowering the write limit", "writes_per_hour", writesPerHour(limit))
}

// maybeRaiseWriteRate raises the write limit by rateRecoverFactor, up to the configured limit,
// when no write has been rate-limited for rateRecoverInterval, at most once per interval.
func (t *throttlingTransport) maybeRaiseWriteRate() {
	if t.writes == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	limit := t.writes.Limit()
	if limit >= t.configured {
		return
	}
	now := time.Now()
	if now.Sub(t.lastLimited) < rateRecoverInterval || now.Sub(t.lastRaised) < rateRecoverInterval {
		return
	}
	limit = rate.Limit(math.Min(float64(limit)*rateRecoverFactor, float64(t.configured)))
	t.writes.SetLimit(limit)
	t.lastRaised = now
	logger.Info("no GitHub rate limits for a while, raising the write limit", "writes_per_hour", writesPerHour(limit))
}

func writesPerHour(limit rate.Limit) int {
	return int(math.Round(float64(limit) * 3600))
}

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
