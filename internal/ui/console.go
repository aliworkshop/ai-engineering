// Package ui is the terminal front-end. A single Console owns stdin, so the
// chat prompt and the approval prompt never fight over buffered input. The same
// Console is also the human-in-the-loop Approver.
package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/durable"
)

// Console reads from one input stream and writes to one output stream.
type Console struct {
	in   *bufio.Reader
	out  io.Writer
	spin *spinner
}

// New wraps the given streams (normally os.Stdin / os.Stdout).
func New(in io.Reader, out io.Writer) *Console {
	return &Console{
		in:   bufio.NewReader(in),
		out:  out,
		spin: newSpinner(out, "thinking…"),
	}
}

// Confirm implements tools.Approver: it shows the pending action and waits for
// a yes/no on the terminal.
func (c *Console) Confirm(action string) bool {
	approved, _ := c.Decide(action)
	return approved
}

// Decide is the richer answer the durable approval gate wants: it reports
// whether a human answered at all, separately from what they said.
//
// The distinction matters because the two have opposite consequences. A "no" is
// a decision — it gets checkpointed, and the model is told it was refused. No
// answer at all (the terminal hit EOF, someone pressed Ctrl-D, the session is
// being piped from a script) is not a decision, and the right response is to
// park the workflow on disk for a human to answer later rather than to invent a
// refusal.
//
// The spinner is paused for the duration — it's the agent that's waiting on the
// human now, not the other way round, and a live spinner would overwrite the
// prompt the human is meant to read.
func (c *Console) Decide(action string) (approved, answered bool) {
	c.spin.Stop()
	defer c.spin.Start()

	fmt.Fprintf(c.out, "\n⚠️  Approve this action?\n    %s\n    [y/N]: ", action)
	line, err := c.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(c.out, "\n(no answer — parking this for later)")
		return false, false
	}
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true, true
	default:
		return false, true
	}
}

// EventWriter is a stdout the harness's event stream can safely write to
// mid-turn.
//
// The spinner and the event stream both want the terminal at the same moment,
// and they are not equals: the spinner is decoration that redraws itself, the
// event is information. Anything that prints during a turn therefore has to
// pause the spinner first, or the next animation frame lands on top of the line
// just written and the output becomes unreadable exactly when you are watching
// it because something went wrong.
//
// Handing out a Writer rather than exporting the spinner keeps that rule in one
// place: the events package renders to an io.Writer and stays ignorant of
// terminals entirely.
func (c *Console) EventWriter() io.Writer { return spinnerSafe{c} }

type spinnerSafe struct{ c *Console }

func (s spinnerSafe) Write(p []byte) (int, error) {
	s.c.spin.Stop()
	defer s.c.spin.Start()
	return s.c.out.Write(p)
}

// Run is the read-eval-print loop: read a line, let the agent answer, repeat
// until the user types "exit" or sends EOF (Ctrl-D).
func (c *Console) Run(ctx context.Context, ag *agent.Agent) {
	ag.OnToolCall = c.logToolCall
	ag.OnCompact = c.logCompact

	fmt.Fprintln(c.out, "English teacher ready. Paste text to correct, or ask about a rule. Type 'exit' to quit.")
	for {
		fmt.Fprint(c.out, "\nyou> ")
		line, err := c.in.ReadString('\n')
		if err != nil { // EOF
			fmt.Fprintln(c.out)
			return
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			return
		}

		// A turn can take several model round trips, so show the user that
		// something is happening instead of a dead terminal. Everything that
		// prints mid-turn pauses the spinner first, so it only ever animates
		// while we're genuinely blocked on the model.
		c.spin.Start()
		answer, workflow, err := ag.AskDurable(ctx, input)
		c.spin.Stop()

		if parked, ok := durable.IsSuspended(err); ok {
			// Not an error: the workflow is alive on disk with a question
			// outstanding, and nothing is holding the process open. Answer it
			// now or next week; either way it picks up where it stopped.
			fmt.Fprintf(c.out, "\n⏸  %s\n", parked.Reason)
			fmt.Fprintf(c.out, "   go run . -approve %s      (or -deny %s)\n", workflow, workflow)
			continue
		}
		if err != nil {
			fmt.Fprintln(c.out, "\nerror:", err)
			continue
		}
		fmt.Fprintln(c.out, "\nagent>", answer)
	}
}

// logToolCall and logCompact both interrupt a running turn to print a line, so
// each clears the spinner first and restarts it afterwards — otherwise the next
// animation frame would land on top of the line just written.
func (c *Console) logToolCall(name, args, result string) {
	c.spin.Stop()
	defer c.spin.Start()

	fmt.Fprintf(c.out, "  [tool] %s(%s) -> %s\n", name, args, oneLine(result, 120))
}

func (c *Console) logCompact(summary string) {
	c.spin.Stop()
	defer c.spin.Start()

	fmt.Fprintf(c.out, "  [history compacted] %s\n", oneLine(summary, 120))
}

func oneLine(s string, max int) string {
	flat := strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	if len(flat) > max {
		return flat[:max] + "…"
	}
	return flat
}
