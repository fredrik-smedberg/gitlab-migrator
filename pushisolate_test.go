package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/config"
	"github.com/hashicorp/go-hclog"
)

func TestIsolateRejectedFindsBadRefsWithFewPushes(t *testing.T) {
	var specs []config.RefSpec
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		specs = append(specs, config.RefSpec("refs/heads/"+n+":refs/heads/"+n))
	}
	bad := map[config.RefSpec]bool{specs[3]: true, specs[8]: true}

	pushes := 0
	var pushed []config.RefSpec
	push := func(rs []config.RefSpec) error {
		pushes++
		for _, r := range rs {
			if bad[r] {
				return errors.New("command error on " + r.Src() + ": pre-receive hook declined")
			}
		}
		pushed = append(pushed, rs...)
		return nil
	}

	rejected, err := isolateRejected(specs, push)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 2 || !bad[rejected[0]] || !bad[rejected[1]] {
		t.Errorf("rejected = %v", rejected)
	}
	if len(pushed) != 8 {
		t.Errorf("pushed %d refs, want 8: %v", len(pushed), pushed)
	}
	for _, r := range pushed {
		if bad[r] {
			t.Errorf("bad ref %s was pushed", r)
		}
	}
}

func TestIsolateRejectedBisectsInsteadOfPushingOneByOne(t *testing.T) {
	var specs []config.RefSpec
	for i := range 64 {
		specs = append(specs, config.RefSpec(fmt.Sprintf("refs/heads/b%d:refs/heads/b%d", i, i)))
	}
	bad := specs[41]
	pushes := 0
	push := func(rs []config.RefSpec) error {
		pushes++
		if slices.Contains(rs, bad) {
			return errors.New("command error on " + bad.Src() + ": pre-receive hook declined")
		}
		return nil
	}
	rejected, err := isolateRejected(specs, push)
	if err != nil || len(rejected) != 1 || rejected[0] != bad {
		t.Fatalf("rejected=%v err=%v", rejected, err)
	}
	// 1 full push + 2 per bisection level (log2(64) = 6) = 13
	if pushes > 13 {
		t.Errorf("%d pushes for 64 refs with 1 bad one, want at most 13", pushes)
	}
}

func TestIsolateRejectedPassesThroughOtherErrors(t *testing.T) {
	specs := []config.RefSpec{"refs/heads/a:refs/heads/a", "refs/heads/b:refs/heads/b"}
	netErr := errors.New("dial tcp: connection refused")
	_, err := isolateRejected(specs, func([]config.RefSpec) error { return netErr })
	if !errors.Is(err, netErr) {
		t.Errorf("expected the network error to be returned, got %v", err)
	}
}

func TestIsolateRejectedNoFailure(t *testing.T) {
	specs := []config.RefSpec{"refs/heads/a:refs/heads/a"}
	calls := 0
	rejected, err := isolateRejected(specs, func([]config.RefSpec) error { calls++; return nil })
	if err != nil || len(rejected) != 0 || calls != 1 {
		t.Errorf("rejected=%v err=%v calls=%d", rejected, err, calls)
	}
}

func TestRemoteMessageLoggerSplitsLinesAndFlagsErrors(t *testing.T) {
	var lines []string
	l := hclog.New(&hclog.LoggerOptions{Level: hclog.Debug, Output: &testLogSink{lines: &lines}})
	w := &remoteMessageLogger{logger: l, args: []any{"repo", "x"}}

	w.Write([]byte("Counting objects: 1\rCounting objects: 2\nerror: GH001: Large files detected.\n"))
	w.Write([]byte("partial"))
	w.Write([]byte(" line\n"))

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "[WARN]") || !strings.Contains(joined, "GH001") {
		t.Errorf("GH001 line should be logged at Warn: %q", joined)
	}
	if !strings.Contains(joined, "partial line") {
		t.Errorf("partial line should be joined across writes: %q", joined)
	}
	if strings.Count(joined, "Counting objects") != 2 {
		t.Errorf("carriage-return separated progress lines should be logged separately at Debug: %q", joined)
	}
	if slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "[WARN]") && strings.Contains(s, "Counting") }) {
		t.Errorf("progress lines should not be Warn: %q", joined)
	}
}

type testLogSink struct{ lines *[]string }

func (s *testLogSink) Write(p []byte) (int, error) {
	*s.lines = append(*s.lines, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
