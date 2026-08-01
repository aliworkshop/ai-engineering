package ui

import (
	"bufio"
	"strings"
	"testing"
)

// clearLine is what the spinner emits to wipe its own line before anything else
// prints. It uses spaces, not ANSI, so it survives consoles that drop \033[K.
var clearLine = blankLine("thinking…")

// newTestConsole builds a Console whose spinner is force-enabled (the test
// buffer isn't a TTY) and already running, i.e. exactly the state the console
// is in partway through a turn.
func newTestConsole(input string) (*Console, *syncBuf) {
	buf := &syncBuf{}
	c := &Console{
		in:   bufio.NewReader(strings.NewReader(input)),
		out:  buf,
		spin: &spinner{out: buf, label: "thinking…", enabled: true},
	}
	c.spin.Start()
	return c, buf
}

// Anything printed mid-turn has to wipe the spinner line first, or the next
// animation frame lands on top of the text.
func TestToolLogClearsSpinnerFirst(t *testing.T) {
	c, buf := newTestConsole("")
	c.logToolCall("read_file", `{"path":"x"}`, "contents")
	c.spin.Stop()

	out := buf.String()
	tool := strings.Index(out, "[tool]")
	if tool < 0 {
		t.Fatalf("tool line missing from output: %q", out)
	}
	if cleared := strings.LastIndex(out[:tool], clearLine); cleared < 0 {
		t.Fatalf("expected the spinner line to be cleared before the tool line, got %q", out)
	}
}

// The approval prompt is the one moment the human is being waited on, so the
// spinner must be off — not just cleared once, but not animating over the
// question while the human reads it.
func TestConfirmPausesSpinner(t *testing.T) {
	c, buf := newTestConsole("y\n")

	var runningDuringPrompt bool
	c.spin.Stop()
	c.spin.Start()
	approved := c.Confirm("rm -rf /tmp/x")
	runningDuringPrompt = c.spin.stop != nil // restarted by Confirm's defer
	c.spin.Stop()

	if !approved {
		t.Fatal("expected 'y' to approve")
	}
	out := buf.String()
	prompt := strings.Index(out, "Approve this action?")
	if prompt < 0 {
		t.Fatalf("approval prompt missing from output: %q", out)
	}
	if cleared := strings.LastIndex(out[:prompt], clearLine); cleared < 0 {
		t.Fatalf("expected the spinner line to be cleared before the prompt, got %q", out)
	}
	// Stop() joins the animation goroutine before Confirm writes, so nothing can
	// animate over the question while the human reads it. The single frame that
	// may follow is the deferred restart, drawn only after the answer came in.
	if frames := strings.Count(out[prompt:], "thinking…"); frames > 1 {
		t.Fatalf("spinner animated over the approval prompt: %d frames in %q", frames, out[prompt:])
	}
	if !runningDuringPrompt {
		t.Fatal("expected Confirm to restart the spinner once the human answered")
	}
}
