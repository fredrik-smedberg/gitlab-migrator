package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                              "0s",
		45 * time.Second:               "45s",
		12*time.Minute + 3*time.Second: "12m03s",
		5*time.Hour + 12*time.Minute:   "5h12m",
		26*time.Hour + 1*time.Minute:   "26h01m",
		-3 * time.Second:               "0s",
		1500 * time.Millisecond:        "1s",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRenderBarLineDeterminate(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	b := &progressBar{
		label:        "schedulinglayer",
		phase:        "writing",
		done:         1234,
		total:        2792,
		started:      now.Add(-62 * time.Minute),
		phaseStarted: now.Add(-20 * time.Minute),
	}

	line := stripANSI(renderBarLine(b, 100, now))

	for _, want := range []string{"schedulinglayer", "writing", "1234/2792", "44%", "1.03/s", "ETA 25m", "1h02m"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("line contains newline: %q", line)
	}
	if n := len([]rune(line)); n > 100 {
		t.Errorf("line is %d cells, wider than 100: %q", n, line)
	}
	if !strings.Contains(line, "━") {
		t.Errorf("line has no bar glyphs: %q", line)
	}
}

func TestRenderBarLineIndeterminateAndFinished(t *testing.T) {
	now := time.Now()
	b := &progressBar{label: "svc", phase: "cloning", started: now.Add(-3 * time.Second), phaseStarted: now}

	line := stripANSI(renderBarLine(b, 80, now))
	if strings.Contains(line, "ETA") || strings.Contains(line, "/s") {
		t.Errorf("indeterminate line should have no rate or ETA: %q", line)
	}
	if !strings.Contains(line, "cloning") {
		t.Errorf("indeterminate line lacks phase: %q", line)
	}

	b.finish(false)
	done := stripANSI(renderBarLine(b, 80, now))
	if !strings.Contains(done, "done") {
		t.Errorf("finished line should say done: %q", done)
	}

	b.finish(true)
	failed := stripANSI(renderBarLine(b, 80, now))
	if !strings.Contains(failed, "failed") {
		t.Errorf("failed line should say failed: %q", failed)
	}
}

func TestRenderBarLineFitsNarrowWidth(t *testing.T) {
	now := time.Now()
	b := &progressBar{label: "a-rather-long-project-name", phase: "preparing", done: 5, total: 10, started: now, phaseStarted: now.Add(-time.Second)}
	line := stripANSI(renderBarLine(b, 40, now))
	if n := len([]rune(line)); n > 40 {
		t.Errorf("line is %d cells, wider than 40: %q", n, line)
	}
}

func TestRendererInterleavesLogLinesAboveBars(t *testing.T) {
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, 80)

	bar := r.newBar("svc")
	bar.setPhase("preparing", 4)
	bar.advance(1)
	r.render()

	first := buf.String()
	if !strings.Contains(stripANSI(first), "preparing") {
		t.Fatalf("first render lacks bar: %q", first)
	}

	buf.Reset()
	if _, err := r.Write([]byte("hello log\n")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	plain := stripANSI(out)

	logAt := strings.Index(plain, "hello log")
	barAt := strings.LastIndex(plain, "preparing")
	if logAt < 0 || barAt < 0 || logAt > barAt {
		t.Errorf("log line must be written before the redrawn bar: %q", plain)
	}
	if !strings.Contains(out, "\x1b[1A") {
		t.Errorf("expected the previous bar line to be cleared with a cursor-up sequence: %q", out)
	}
}

func TestRendererDropsFinishedBarsAfterOneRender(t *testing.T) {
	var buf bytes.Buffer
	r := newProgressRenderer(&buf, 80)

	bar := r.newBar("svc")
	bar.setPhase("writing", 2)
	bar.advance(2)
	bar.finish(false)
	r.render()
	if !strings.Contains(stripANSI(buf.String()), "done") {
		t.Fatalf("finished bar should be rendered once: %q", buf.String())
	}

	buf.Reset()
	r.render()
	if strings.Contains(stripANSI(buf.String()), "svc") {
		t.Errorf("finished bar should not be in the live area any more: %q", buf.String())
	}
}

func TestNilProgressIsSafe(t *testing.T) {
	var r *progressRenderer
	bar := r.newBar("svc")
	bar.setPhase("x", 1)
	bar.advance(1)
	bar.finish(false)
	r.setProjects(3)
	r.projectDone()
	r.stop()
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[1m\x1b[36mabc\x1b[0m"); got != "abc" {
		t.Errorf("stripANSI = %q", got)
	}
}
