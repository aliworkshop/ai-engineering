package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// syncBuf is a goroutine-safe buffer so the -race detector stays quiet while the
// spinner goroutine writes and the test reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The spinner writes labelled carriage-return frames while running, then erases
// the line on stop. We force enabled=true because the test buffer isn't a TTY.
func TestSpinnerAnimatesAndClears(t *testing.T) {
	buf := &syncBuf{}
	s := &spinner{out: buf, label: "thinking…", enabled: true}

	s.Start()
	time.Sleep(220 * time.Millisecond) // immediate frame + a couple of ticks
	s.Stop()

	out := buf.String()
	if !strings.Contains(out, "thinking…") {
		t.Fatalf("expected the label in the output, got %q", out)
	}
	if strings.Count(out, "thinking…") < 2 {
		t.Fatalf("expected multiple animation frames, got %d", strings.Count(out, "thinking…"))
	}
	if !strings.HasSuffix(out, blankLine("thinking…")) {
		t.Fatalf("expected the spinner line to be blanked on stop, got %q", out)
	}
	// No ANSI escapes: IDE consoles that honour \r still drop \033[K, and the
	// leftover frame is exactly what that would strand on screen.
	if strings.Contains(out, "\033[") {
		t.Fatalf("spinner must not depend on ANSI escapes, got %q", out)
	}
}

// blankLine is what Stop writes to wipe a frame: return, spaces wide enough to
// cover "<rune><space><label>", return again.
func blankLine(label string) string {
	return "\r" + strings.Repeat(" ", utf8.RuneCountInString(label)+2) + "\r"
}

// On a non-terminal writer the spinner must stay silent, so piped output and
// captured test logs never fill with carriage-return noise.
func TestSpinnerNoopOnNonTerminal(t *testing.T) {
	t.Setenv("AGENT_SPINNER", "") // ignore whatever the developer has set
	buf := &syncBuf{}
	s := newSpinner(buf, "thinking…") // bytes-backed writer → not a terminal
	s.Start()
	s.Stop()
	if got := buf.String(); got != "" {
		t.Fatalf("expected no output on a non-terminal, got %q", got)
	}
}

// AGENT_SPINNER overrides the terminal detection both ways — the escape hatch
// for IDE run windows, which are pipes with a human watching.
func TestSpinnerEnvOverride(t *testing.T) {
	buf := &syncBuf{}

	for _, on := range []string{"1", "true", "on", "always", "yes", "YES"} {
		t.Setenv("AGENT_SPINNER", on)
		if !spinnerEnabled(buf) {
			t.Errorf("AGENT_SPINNER=%q should force the spinner on", on)
		}
	}
	for _, off := range []string{"0", "false", "off", "never", "no"} {
		t.Setenv("AGENT_SPINNER", off)
		if spinnerEnabled(buf) {
			t.Errorf("AGENT_SPINNER=%q should force the spinner off", off)
		}
	}
	// Unset or unrecognized falls back to detecting a terminal, and a buffer
	// isn't one.
	for _, fallback := range []string{"", "maybe"} {
		t.Setenv("AGENT_SPINNER", fallback)
		if spinnerEnabled(buf) {
			t.Errorf("AGENT_SPINNER=%q should fall back to terminal detection", fallback)
		}
	}

	// Forced on, it really does animate to a plain buffer.
	t.Setenv("AGENT_SPINNER", "1")
	s := newSpinner(buf, "thinking…")
	s.Start()
	time.Sleep(120 * time.Millisecond)
	s.Stop()
	if !strings.Contains(buf.String(), "thinking…") {
		t.Fatalf("expected frames when forced on, got %q", buf.String())
	}
}

// Stop must be safe when the spinner was never started, and Start must not
// launch a second goroutine when one is already running — the console wires
// both to mid-turn callbacks that can fire in any order.
func TestSpinnerStopWithoutStartAndDoubleStart(t *testing.T) {
	buf := &syncBuf{}
	s := &spinner{out: buf, label: "thinking…", enabled: true}

	s.Stop() // never started
	if got := buf.String(); got != "" {
		t.Fatalf("expected no output from a stray Stop, got %q", got)
	}

	s.Start()
	s.Start() // must be a no-op, not a second goroutine
	s.Stop()
	if s.stop != nil || s.done != nil {
		t.Fatal("expected Stop to clear the spinner's channels")
	}
}
