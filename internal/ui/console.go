// Package ui is the terminal front-end: it owns stdin and stdout, and nothing
// else in the program prints. That is what lets the agent be driven by a test,
// a script, or a browser later without touching a line of it.
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
	in  *bufio.Reader
	out io.Writer
}

// New wraps the given streams (normally os.Stdin / os.Stdout).
func New(in io.Reader, out io.Writer) *Console {
	return &Console{in: bufio.NewReader(in), out: out}
}

// Run is the read-eval-print loop: read a line, let the agent answer, repeat
// until the user types "exit" or sends EOF (Ctrl-D).
//
// It no longer prints what the agent is doing mid-turn. The event stream does
// that, and one channel for progress beats two that can disagree.
func (c *Console) Run(ctx context.Context, ag *agent.Agent) {
	fmt.Fprintln(c.out, "Type a message, or 'exit' to quit.")
	for {
		fmt.Fprint(c.out, "\nyou> ")
		line, err := c.in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" { // EOF
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

		answer, err := ag.Ask(ctx, input)
		if err != nil {
			fmt.Fprintln(c.out, "error:", err)
			continue
		}
		fmt.Fprintln(c.out, "\nagent>", answer)
	}
}
