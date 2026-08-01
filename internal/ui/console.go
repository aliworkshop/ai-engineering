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
// a yes/no on the terminal. The spinner is paused for the duration — it's the
// agent that's waiting on the human now, not the other way round, and a live
// spinner would overwrite the prompt the human is meant to read.
func (c *Console) Confirm(action string) bool {
	c.spin.Stop()
	defer c.spin.Start()

	fmt.Fprintf(c.out, "\n⚠️  Approve this action?\n    %s\n    [y/N]: ", action)
	line, _ := c.in.ReadString('\n')
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// Run is the read-eval-print loop: read a line, let the agent answer, repeat
// until the user types "exit" or sends EOF (Ctrl-D).
func (c *Console) Run(ctx context.Context, ag *agent.Agent) {
	ag.OnToolCall = c.logToolCall
	ag.OnCompact = c.logCompact

	fmt.Fprintln(c.out, "AI agent ready. Ask me anything. Type 'exit' to quit.")
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
		answer, err := ag.Ask(ctx, input)
		c.spin.Stop()

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
