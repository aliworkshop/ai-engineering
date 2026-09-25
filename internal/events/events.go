// Package events is the harness's nervous system: everything the agent does is
// an Event, and the terminal is the inspector — one glyphed line each.
//
// The reason to build it this way is not prettier output. It is that progress
// which exists only as a fmt.Println is invisible to everything except a human
// watching the screen. An event is a value: it can be rendered, counted, logged
// to a file, pushed down a websocket, or replayed after the fact. This package
// only renders — but everything later is a second sink, not a rewrite.
//
// It imports nothing of ours, on purpose. Any layer may emit.
package events

import (
	"encoding/json"
	"fmt"
	"io"
)

// The event vocabulary. Only the first five are emitted today; the rest are
// declared because they are where this is going, and a name reserved now is a
// name that cannot be spelled two ways later.
const (
	WorkflowStarted   = "workflow.started"
	WorkflowCompleted = "workflow.completed"
	WorkflowFailed    = "workflow.failed"
	ToolRequested     = "tool.requested"
	ToolCompleted     = "tool.completed"
	ModelCompleted    = "model.completed"
)

// glyph is one character per event type, so a stream is scannable without
// reading it. The map is deliberately wider than what we emit: handoffs,
// approvals, plans and sub-agents are the parts still to come.
var glyph = map[string]string{
	WorkflowStarted:      "▶",
	WorkflowCompleted:    "✔",
	WorkflowFailed:       "✘",
	ModelCompleted:       "🧠",
	ToolRequested:        "⚙",
	ToolCompleted:        "✓",
	"memory.compacted":   "🗜",
	"agent.handoff":      "↪",
	"plan.created":       "🗺",
	"subagent.started":   "├",
	"subagent.completed": "✓",
	"subagent.failed":    "✘",
	"approval.requested": "✋",
	"approval.resolved":  "🖊",
}

// Event is one thing that happened. Every field but Type is optional, and the
// renderer shows only the ones that are set — so a tool event reads as a tool
// event without a schema per event type.
type Event struct {
	Type     string `json:"-"` // rendered in the fixed column, not the detail
	Workflow string `json:"-"` // ditto: it is the thread, not a detail

	Name   string `json:"name,omitempty"`   // tool or step name
	Call   string `json:"call,omitempty"`   // the model's own id for one tool call
	Args   string `json:"args,omitempty"`   // the model's JSON arguments
	Input  string `json:"input,omitempty"`  // what a workflow was asked to do
	Output string `json:"output,omitempty"` // what it answered
	Error  string `json:"error,omitempty"`
}

// Emitter is a place events go. An agent depends on this interface rather than
// on a terminal, which is what lets the same stream feed a log or a browser
// later without the agent knowing.
type Emitter interface {
	Emit(Event)
}

// Emit sends one event, tolerating a nil emitter. Every service in this repo is
// optional, and "run silently" should not mean "guard every call site".
func Emit(to Emitter, e Event) Event {
	if to != nil {
		to.Emit(e)
	}
	return e
}

// Console renders events to a writer, one line each.
type Console struct {
	out io.Writer
}

// NewConsole writes to w — normally os.Stdout.
func NewConsole(w io.Writer) *Console { return &Console{out: w} }

// Emit renders one line: glyph, type, workflow, and whatever detail the event
// carries as JSON — all of it. A truncated stream is easier to scan and worse
// to debug with, and the whole reason to have a stream is the moment you need
// to know exactly what the model asked for.
func (c *Console) Emit(e Event) {
	mark, ok := glyph[e.Type]
	if !ok {
		mark = "·"
	}

	detail, err := json.Marshal(e)
	if err != nil {
		detail = []byte("{}")
	}
	fmt.Fprintf(c.out, "  %s  %-21s %-9s %s\n",
		mark, e.Type, e.Workflow, string(detail))
}
