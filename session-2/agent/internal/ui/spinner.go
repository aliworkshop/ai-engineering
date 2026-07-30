package ui

import (
	"fmt"
	"io"
	"os"
	"time"
)

// spinner animates a "thinking" indicator on a single terminal line while the
// agent waits for the model, then erases that line when stopped so the real
// output starts clean. On a non-terminal writer (a pipe, a test buffer) it does
// nothing, keeping captured output free of carriage-return noise.
type spinner struct {
	out     io.Writer
	label   string
	enabled bool
	stop    chan struct{}
	done    chan struct{}
}

func newSpinner(out io.Writer, label string) *spinner {
	return &spinner{out: out, label: label, enabled: isTerminal(out)}
}

// Start begins the animation in a background goroutine. A second call before
// Stop is a no-op, so it's safe to wire directly to a per-turn callback.
func (s *spinner) Start() {
	if !s.enabled || s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run()
}

func (s *spinner) run() {
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	// Draw the first frame immediately so even a sub-100ms model call shows the
	// spinner at least once, instead of waiting a full tick that may never come.
	fmt.Fprintf(s.out, "\r%c %s", frames[0], s.label)
	ticker := time.NewTicker(90 * time.Millisecond)
	defer ticker.Stop()
	for i := 1; ; i++ {
		select {
		case <-s.stop:
			close(s.done)
			return
		case <-ticker.C:
			fmt.Fprintf(s.out, "\r%c %s", frames[i%len(frames)], s.label)
		}
	}
}

// Stop ends the animation and erases the spinner line. Safe to call when not
// running.
func (s *spinner) Stop() {
	if !s.enabled || s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done                      // wait for the goroutine to stop writing
	fmt.Fprint(s.out, "\r\033[K") // carriage return + clear to end of line
	s.stop, s.done = nil, nil
}

// isTerminal reports whether w is a character device, i.e. an interactive
// terminal — so we never spray spinner frames into a pipe or a test buffer.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
