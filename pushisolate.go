package main

import (
	"bytes"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/config"
	"github.com/hashicorp/go-hclog"
)

// isolateRejected pushes refSpecs with push. When the server rejects the push
// (a per-ref "command error", for example GitHub's pre-receive hook declining a
// branch with a file over 100 MB), the whole push is refused, so it bisects
// the refspecs to find exactly which ones are rejected and pushes the others.
// It returns the rejected refspecs. Errors that are not per-ref rejections,
// such as network errors, are returned as they are.
func isolateRejected(refSpecs []config.RefSpec, push func([]config.RefSpec) error) ([]config.RefSpec, error) {
	if len(refSpecs) == 0 {
		return nil, nil
	}
	err := push(refSpecs)
	if err == nil {
		return nil, nil
	}
	if !isRefRejection(err) {
		return nil, err
	}
	if len(refSpecs) == 1 {
		return []config.RefSpec{refSpecs[0]}, nil
	}

	mid := len(refSpecs) / 2
	var rejected []config.RefSpec
	for _, half := range [][]config.RefSpec{refSpecs[:mid], refSpecs[mid:]} {
		r, err := isolateRejected(half, push)
		if err != nil {
			return rejected, err
		}
		rejected = append(rejected, r...)
	}
	return rejected, nil
}

// isRefRejection reports whether a go-git push error is the server rejecting
// one or more refs, as opposed to a transport or protocol failure.
func isRefRejection(err error) bool {
	return err != nil && strings.Contains(err.Error(), "command error on ")
}

// remoteMessageLogger receives the sideband messages a git server sends during
// a push or fetch (go-git's Progress option) and logs them, so that reasons
// such as GitHub's "GH001: Large files detected" are visible instead of only
// "pre-receive hook declined". Lines that look like errors are logged at Warn,
// progress chatter at Debug.
type remoteMessageLogger struct {
	logger hclog.Logger
	args   []any

	mu  sync.Mutex
	buf bytes.Buffer
}

func newRemoteMessageLogger(args ...any) *remoteMessageLogger {
	return &remoteMessageLogger{logger: logger, args: args}
}

func (w *remoteMessageLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexAny(w.buf.Bytes(), "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(w.buf.String()[:i])
		w.buf.Next(i + 1)
		w.logLine(line)
	}
	return len(p), nil
}

func (w *remoteMessageLogger) logLine(line string) {
	if line == "" {
		return
	}
	lower := strings.ToLower(line)
	args := append([]any{"message", line}, w.args...)
	if strings.HasPrefix(lower, "error") || strings.HasPrefix(lower, "fatal") || strings.Contains(lower, "declined") || strings.Contains(lower, "rejected") {
		w.logger.Warn("message from git server", args...)
		return
	}
	w.logger.Debug("message from git server", args...)
}
