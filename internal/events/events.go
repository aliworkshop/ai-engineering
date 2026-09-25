// Package events is the harness's nervous system: everything the agent does is
// an Event, and a sink decides what becomes of it.
//
// The reason to build it this way is not prettier output. Progress that exists
// only as a fmt.Println is invisible to everything except a human watching the
// screen. An event is a value: it can be rendered, counted, written to a file,
// pushed down a socket, or read back afterwards. Part 1 had one sink, a
// terminal. Part 2 adds a second — a JSONL log — and that is an addition
// rather than a rewrite, which was the whole point of the first one.
//
// It imports nothing of ours, on purpose. Any layer may emit.
package events

import "time"

// The event vocabulary. Only some of these are emitted today; the rest are
// declared because they are where this is going, and a name reserved now is a
// name that cannot be spelled two ways later.
const (
	WorkflowStarted   = "workflow.started"
	WorkflowResumed   = "workflow.resumed"
	WorkflowCompleted = "workflow.completed"
	WorkflowFailed    = "workflow.failed"

	// StepCompleted is about the CHECKPOINT — a named unit of work whose
	// result is now on disk. StepCached is its replay: the step was already
	// done in an earlier run, so nothing executed this time.
	//
	// These are deliberately different from the tool events below. A step can
	// be a model turn or a human decision as easily as a tool call, and
	// calling all of them "tool" would make the stream lie about what
	// happened.
	StepCompleted = "step.completed"
	StepCached    = "step.cached"

	ToolRequested  = "tool.requested"
	ToolCompleted  = "tool.completed"
	ModelCompleted = "model.completed"
)

// glyph is one character per event type, so a stream is scannable without
// reading it. The map is deliberately wider than what we emit: handoffs,
// approvals, plans and sub-agents are the parts still to come.
var glyph = map[string]string{
	WorkflowStarted:      "▶",
	WorkflowResumed:      "⟲",
	WorkflowCompleted:    "✔",
	WorkflowFailed:       "✘",
	StepCompleted:        "▪",
	StepCached:           "⏩",
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
// console shows only the ones that are set — so a tool event reads as a tool
// event without a schema per event type.
//
// The tags matter now that events outlive the process. A logged line has to be
// complete on its own: the type it was, the workflow it belonged to, and when.
type Event struct {
	TS       string `json:"ts,omitempty"`
	Type     string `json:"type,omitempty"`
	Workflow string `json:"workflow,omitempty"`

	Name   string `json:"name,omitempty"`   // tool or step name
	Call   string `json:"call,omitempty"`   // the model's own id for one tool call
	Args   string `json:"args,omitempty"`   // the model's JSON arguments
	Input  string `json:"input,omitempty"`  // what a workflow was asked to do
	Output string `json:"output,omitempty"` // what it answered
	Error  string `json:"error,omitempty"`
	Millis int64  `json:"ms,omitempty"` // how long a step took
}

// Emitter is a place events go. An agent depends on this interface rather than
// on a terminal, which is what lets the same stream feed a log or a browser
// without the agent knowing.
type Emitter interface {
	Emit(Event)
}

// Emit sends one event, tolerating a nil emitter. Every service in this repo is
// optional, and "run silently" should not mean "guard every call site".
//
// The timestamp is stamped here, once, before the event reaches any sink — so
// the log and the terminal agree about when something happened rather than
// each asking the clock separately. Full RFC3339 rather than the wall time: a
// log that survives a crash survives a day, and latency reporting wants the
// sub-second part.
func Emit(to Emitter, e Event) Event {
	if e.TS == "" {
		e.TS = time.Now().Format(time.RFC3339Nano)
	}
	if to != nil {
		to.Emit(e)
	}
	return e
}
