package main

import (
	"io"
	"net/http"
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
