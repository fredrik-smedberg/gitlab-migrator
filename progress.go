package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// progress renders live progress bars on stderr, one per project, in the style
// of Python's Rich. It is nil when disabled (stderr is not a terminal, or
// -no-progress), and every method tolerates a nil receiver so call sites need
// no checks. Log output is routed through the renderer so log lines are
// printed above the bars instead of being overwritten by them.
var progress *progressRenderer

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"

	progressRefresh = 125 * time.Millisecond
)

var (
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ansiRegexp    = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
)

type progressBar struct {
	r *progressRenderer

	label        string
	phase        string
	done, total  int
	started      time.Time
	phaseStarted time.Time
	finished     bool
	failed       bool
	rendered     bool // finished bar has been drawn once as a permanent line
	labelWidth   int  // set by the renderer so labels of live bars align
}

// phaseWidth keeps the phase column fixed so the bar does not move between phases.
const phaseWidth = len("pushing temporary branches")

type progressRenderer struct {
	mu     sync.Mutex
	out    io.Writer
	width  int
	bars   []*progressBar
	lines  int // live lines currently drawn below the cursor origin
	frame  int
	start  time.Time
	stopCh chan struct{}
	done   chan struct{}

	projectsTotal, projectsDone int
}

// newProgressRenderer returns a renderer writing to out. It does not start the
// refresh goroutine; call run for that. width is the terminal width in cells.
func newProgressRenderer(out io.Writer, width int) *progressRenderer {
	if width <= 0 {
		width = 100
	}
	return &progressRenderer{out: out, width: width, start: time.Now()}
}

// enableProgress turns on live progress on stderr when it is a terminal.
func enableProgress() {
	if !isatty.IsTerminal(os.Stderr.Fd()) && !isatty.IsCygwinTerminal(os.Stderr.Fd()) {
		return
	}
	progress = newProgressRenderer(os.Stderr, terminalWidth())
	progress.run()
}

// run starts the periodic refresh and hides the cursor until stop is called.
func (r *progressRenderer) run() {
	if r == nil || r.stopCh != nil {
		return
	}
	r.stopCh = make(chan struct{})
	r.done = make(chan struct{})
	fmt.Fprint(r.out, "\x1b[?25l")
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(progressRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.render()
			case <-r.stopCh:
				return
			}
		}
	}()
}

// stop draws the final state, clears the live area and shows the cursor again.
func (r *progressRenderer) stop() {
	if r == nil {
		return
	}
	if r.stopCh != nil {
		close(r.stopCh)
		<-r.done
		r.stopCh = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bars {
		if !b.finished {
			b.finished = true
		}
	}
	r.renderLocked()
	r.clearLocked()
	fmt.Fprint(r.out, "\x1b[?25h")
}

func (r *progressRenderer) setProjects(n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.projectsTotal = n
	r.mu.Unlock()
}

func (r *progressRenderer) projectDone() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.projectsDone++
	r.mu.Unlock()
}

func (r *progressRenderer) newBar(label string) *progressBar {
	if r == nil {
		return nil
	}
	now := time.Now()
	b := &progressBar{r: r, label: label, phase: "starting", started: now, phaseStarted: now}
	r.mu.Lock()
	r.bars = append(r.bars, b)
	r.mu.Unlock()
	return b
}

// setPhase starts a new phase. total 0 means indeterminate (a spinner is shown).
func (b *progressBar) setPhase(phase string, total int) {
	if b == nil {
		return
	}
	b.lock()
	b.phase = phase
	b.total = total
	b.done = 0
	b.phaseStarted = time.Now()
	b.unlock()
}

func (b *progressBar) advance(n int) {
	if b == nil {
		return
	}
	b.lock()
	b.done += n
	b.unlock()
}

// lock and unlock take the renderer lock; a bar made without a renderer (tests) has none.
func (b *progressBar) lock() {
	if b.r != nil {
		b.r.mu.Lock()
	}
}

func (b *progressBar) unlock() {
	if b.r != nil {
		b.r.mu.Unlock()
	}
}

// finish marks the bar complete. It is drawn once more as a permanent line and
// then leaves the live area.
func (b *progressBar) finish(failed bool) {
	if b == nil {
		return
	}
	b.lock()
	b.finished = true
	b.failed = failed
	b.rendered = false
	b.unlock()
}

// Write implements io.Writer for log output: the live area is cleared, the log
// line is written where the bars were, and the bars are drawn again below it.
func (r *progressRenderer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	n, err := r.out.Write(p)
	if err != nil {
		return n, err
	}
	if len(p) > 0 && p[len(p)-1] != '\n' {
		fmt.Fprint(r.out, "\n")
	}
	r.renderLocked()
	return n, nil
}

func (r *progressRenderer) render() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	r.renderLocked()
}

// clearLocked moves the cursor to the top of the live area and erases it.
func (r *progressRenderer) clearLocked() {
	if r.lines > 0 {
		fmt.Fprintf(r.out, "\r\x1b[%dA\x1b[0J", r.lines)
		r.lines = 0
	}
}

// renderLocked draws finished bars as permanent lines, then the live bars.
func (r *progressRenderer) renderLocked() {
	r.frame++
	now := time.Now()

	live := r.bars[:0]
	for _, b := range r.bars {
		if b.finished {
			if !b.rendered {
				fmt.Fprintln(r.out, renderBarLine(b, r.width, now))
				b.rendered = true
			}
			continue
		}
		live = append(live, b)
	}
	r.bars = live

	var sb strings.Builder
	lines := 0
	if r.projectsTotal > 1 {
		fmt.Fprintf(&sb, "%s%sprojects%s %d/%d  %selapsed%s %s\n", ansiBold, ansiCyan, ansiReset, r.projectsDone, r.projectsTotal, ansiDim, ansiReset, formatDuration(now.Sub(r.start)))
		lines++
	}
	labelWidth := 0
	for _, b := range r.bars {
		labelWidth = max(labelWidth, len([]rune(b.label)))
	}
	for _, b := range r.bars {
		b.labelWidth = labelWidth
		sb.WriteString(renderBarLineFrame(b, r.width, now, r.frame))
		sb.WriteString("\n")
		lines++
	}
	if lines > 0 {
		fmt.Fprint(r.out, sb.String())
	}
	r.lines = lines
}

// renderBarLine renders one bar without ANSI-free width overflow. Call with the
// renderer lock held or on a bar not shared with the renderer.
func renderBarLine(b *progressBar, width int, now time.Time) string {
	return renderBarLineFrame(b, width, now, 0)
}

func renderBarLineFrame(b *progressBar, width int, now time.Time, frame int) string {
	if width <= 0 {
		width = 100
	}
	elapsed := formatDuration(now.Sub(b.started))

	// Fixed text, measured without ANSI so the bar can take the remaining width.
	label := b.label
	if max := width / 4; len([]rune(label)) > max && max > 4 {
		label = string([]rune(label)[:max-1]) + "…"
	}
	if b.labelWidth > len([]rune(label)) && b.labelWidth <= width/4 {
		label += strings.Repeat(" ", b.labelWidth-len([]rune(label)))
	}
	phase := b.phase
	if !b.finished && len(phase) < phaseWidth {
		phase += strings.Repeat(" ", phaseWidth-len(phase))
	}

	var status, bar string
	switch {
	case b.finished && b.failed:
		phase = ""
		status = ansiRed + "✘ failed" + ansiReset
	case b.finished:
		phase = ""
		status = ansiGreen + "✔ done" + ansiReset
	case b.total <= 0:
		status = ansiDim + "…" + ansiReset
	default:
		pct := b.done * 100 / b.total
		if pct > 100 {
			pct = 100
		}
		rate := ""
		eta := "ETA --"
		if d := now.Sub(b.phaseStarted); b.done > 0 && d >= time.Second {
			perSec := float64(b.done) / d.Seconds()
			rate = fmt.Sprintf("%.2f/s", perSec)
			if b.done < b.total {
				eta = "ETA " + formatDuration(time.Duration(float64(b.total-b.done)/perSec*float64(time.Second)))
			} else {
				eta = "ETA 0s"
			}
		}
		digits := len(strconv.Itoa(b.total))
		status = fmt.Sprintf("%*d/%d %s%3d%%%s %s%8s %-10s%s", digits, b.done, b.total, ansiBold, pct, ansiReset, ansiDim, rate, eta, ansiReset)
	}

	plainLen := func(s string) int { return len([]rune(stripANSI(s))) }
	fixed := plainLen(label) + 2 + plainLen(phase) + 2 + plainLen(status) + 2 + len(elapsed)
	barWidth := width - fixed - 2
	if !b.finished && b.total > 0 {
		if barWidth < 8 {
			barWidth = 8
		}
		if barWidth > 40 {
			barWidth = 40
		}
		filled := b.done * barWidth / b.total
		if filled > barWidth {
			filled = barWidth
		}
		bar = ansiMagenta + strings.Repeat("━", filled) + ansiDim + strings.Repeat("━", barWidth-filled) + ansiReset + "  "
	} else if !b.finished {
		bar = ansiMagenta + spinnerFrames[frame%len(spinnerFrames)] + ansiReset + "  "
	}

	line := fmt.Sprintf("%s%s%s  %s%s%s  %s%s  %s%s%s", ansiBold+ansiCyan, label, ansiReset, ansiMagenta, phase, ansiReset, bar, status, ansiDim, elapsed, ansiReset)

	// Trim from the right if the terminal is narrower than the fixed text.
	if plainLen(line) > width {
		runes := []rune(stripANSI(line))
		line = string(runes[:width-1]) + "…"
	}
	return line
}

// formatDuration renders a duration compactly: 45s, 12m03s, 5h12m.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return strconv.Itoa(s) + "s"
	}
}

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

// terminalWidth returns the stderr terminal width, or 0 if unknown.
func terminalWidth() int {
	if w := terminalWidthFd(int(os.Stderr.Fd())); w > 0 {
		return w
	}
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 {
		return v
	}
	return 0
}

// switchableWriter lets the logger's output be redirected after the logger has
// been created, for example to the progress renderer once it is enabled.
type switchableWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchableWriter) set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

func (s *switchableWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
