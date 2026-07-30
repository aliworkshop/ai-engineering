package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
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
	if !strings.HasSuffix(out, "\r\033[K") {
		t.Fatalf("expected the spinner line to be cleared on stop, got %q", out)
	}
}

// On a non-terminal writer the spinner must stay silent, so piped output and
// captured test logs never fill with carriage-return noise.
func TestSpinnerNoopOnNonTerminal(t *testing.T) {
	buf := &syncBuf{}
	s := newSpinner(buf, "thinking…") // bytes-backed writer → not a terminal
	s.Start()
	s.Stop()
	if got := buf.String(); got != "" {
		t.Fatalf("expected no output on a non-terminal, got %q", got)
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
