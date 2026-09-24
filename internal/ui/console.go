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
func (c *Console) Run(ctx context.Context, ag *agent.Agent) {
	// The agent reports what it is doing; this decides what that looks like.
	ag.OnToolCall = c.logToolCall

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

// logToolCall shows a tool running. The arguments are the interesting half —
// they are what the model decided to search for — so they are shown and the
// result is not: a search write-up is several paragraphs, and printing it here
// would show you the answer twice.
func (c *Console) logToolCall(name, args, _ string) {
	fmt.Fprintf(c.out, "  [%s] %s\n", name, truncate(args, 120))
}

// truncate keeps one tool line to one terminal line.
func truncate(s string, max int) string {
	flat := []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(s))
	if len(flat) <= max {
		return string(flat)
	}
	return string(flat[:max]) + "…"
}
