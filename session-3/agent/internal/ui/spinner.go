package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
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
	return &spinner{out: out, label: label, enabled: spinnerEnabled(out)}
}

// spinnerEnabled decides whether to animate at all.
//
// The default is "only on a real terminal", which keeps carriage-return frames
// out of pipes and test buffers. But that check also reports false in an IDE
// run window (GoLand's Run tool window, VS Code's debug console), which hands
// the process a pipe even though a human is sitting there watching — the one
// case where the detection is right about the plumbing and wrong about the
// audience. AGENT_SPINNER overrides it in either direction.
func spinnerEnabled(w io.Writer) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_SPINNER"))) {
	case "1", "true", "on", "always", "yes":
		return true
	case "0", "false", "off", "never", "no":
		return false
	}
	return isTerminal(w)
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

// frameRate is how long each frame is held. At 60ms the braille wheel reads as
// continuous motion rather than a stutter, without spending a write every few
// milliseconds on an animation nobody studies frame by frame.
const frameRate = 60 * time.Millisecond

func (s *spinner) run() {
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	// Draw the first frame immediately so even a very short model call shows the
	// spinner at least once, instead of waiting a full tick that may never come.
	fmt.Fprintf(s.out, "\r%c %s", frames[0], s.label)
	ticker := time.NewTicker(frameRate)
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
//
// The line is blanked by overwriting it with spaces rather than with the ANSI
// erase-to-end-of-line sequence (\033[K). IDE consoles commonly honour a bare
// carriage return but drop \033[K, which would leave "⠦ thinking…" stranded on
// screen underneath the answer. Spaces work anywhere a \r does.
func (s *spinner) Stop() {
	if !s.enabled || s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done // wait for the goroutine to stop writing
	fmt.Fprintf(s.out, "\r%s\r", strings.Repeat(" ", s.width()))
	s.stop, s.done = nil, nil
}

// width is how many columns one frame occupies: a spinner rune, a space, then
// the label. Counted in runes — the label is UTF-8 and every glyph the spinner
// draws is single-width.
func (s *spinner) width() int {
	return utf8.RuneCountInString(s.label) + 2
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
