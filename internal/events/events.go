// Package events is the harness's nervous system. Nothing in the runtime
// print s its feelings: every move it makes — a model turn, a tool call, a
// handoff, a human decision — becomes one typed Event handed to an Emitter.
//
// That indirection is the whole point. The terminal renders an event as a
// glyphed line, the browser pushes it down an SSE stream, and the durable log
// appends it to events.jsonl — three very different consumers, none of which
// the code doing the work has to know about. Invisible infrastructure is
// undebuggable infrastructure, and observability you have to thread through
// call sites never gets added.
package events

import (
	"encoding/json"
	"time"
)

// Type names one thing the harness did. They read as noun.verb so the stream
// sorts and greps well, and they are the same names a production orchestrator
// would use, so the vocabulary transfers.
type Type string

const (
	// The workflow lifecycle (Part 2). Suspended is not a failure: it means a
	// human decision is outstanding and the process is free to exit (Part 7).
	WorkflowStarted   Type = "workflow.started"
	WorkflowResumed   Type = "workflow.resumed"
	WorkflowCompleted Type = "workflow.completed"
	WorkflowFailed    Type = "workflow.failed"
	WorkflowSuspended Type = "workflow.suspended"

	// One model round trip finished.
	ModelCompleted Type = "model.completed"

	// A tool the model asked for, and the result it produced.
	ToolRequested Type = "tool.requested"
	ToolCompleted Type = "tool.completed"

	// A durable step finished and was checkpointed; StepCached is its replay
	// twin — the work was already done in an earlier run of this workflow, so
	// nothing executed and nothing was billed. Both are about the CHECKPOINT,
	// which is why a model turn reports here and not as a tool.
	StepCompleted Type = "step.completed"
	StepCached    Type = "step.cached"

	// Working memory was folded into a summary (Part 4).
	MemoryCompacted Type = "memory.compacted"

	// Control moved sideways to a specialist (Part 5).
	AgentHandoff Type = "agent.handoff"

	// The supervisor's four phases (Part 6). The plan is emitted because it is
	// a first-class artifact, not ephemeral reasoning.
	PlanCreated       Type = "plan.created"
	SubagentStarted   Type = "subagent.started"
	SubagentCompleted Type = "subagent.completed"
	SubagentFailed    Type = "subagent.failed"

	// A human gate (Part 7).
	ApprovalRequested Type = "approval.requested"
	ApprovalResolved  Type = "approval.resolved"
)

// Event is one thing that happened. Every field past Type is optional, so a
// single struct covers the whole vocabulary without a type switch at every
// call site — and omitempty keeps the JSONL log readable rather than a wall of
// empty keys.
//
// Approved is a *bool rather than a bool because "the human said no" and "no
// human was asked" are different facts, and a plain false cannot tell them
// apart.
type Event struct {
	TS       string `json:"ts"`
	Type     Type   `json:"type"`
	Workflow string `json:"workflow,omitempty"`

	Agent  string `json:"agent,omitempty"`  // which agent/specialist acted
	Name   string `json:"name,omitempty"`   // tool, step, or investigator name
	Args   string `json:"args,omitempty"`   // the model's JSON arguments
	Result string `json:"result,omitempty"` // what came back
	Text   string `json:"text,omitempty"`   // free-form: a summary, a plan, an objective

	From string `json:"from,omitempty"` // handoff source
	To   string `json:"to,omitempty"`   // handoff target

	Approved *bool  `json:"approved,omitempty"`
	Error    string `json:"error,omitempty"`
	Tokens   int    `json:"tokens,omitempty"`
	Millis   int64  `json:"millis,omitempty"` // how long the step took
}

// Emitter is the one seam every producer depends on. Keeping it a single
// method means a test sink is three lines and the UI packages never have to
// import the runtime to listen to it.
type Emitter interface {
	Emit(Event)
}

// EmitterFunc adapts a plain function to Emitter, for the many places that
// only want to close over one channel or one writer.
type EmitterFunc func(Event)

func (f EmitterFunc) Emit(e Event) { f(e) }

// Emit stamps the event and hands it on. Producers call this rather than
// e.Emit directly so the timestamp is set in exactly one place — and so a nil
// Emitter is legal. Events being optional matters: a unit test constructing an
// agent shouldn't have to wire a whole observability stack to get past line
// one.
func Emit(to Emitter, e Event) Event {
	if e.TS == "" {
		e.TS = time.Now().Format("15:04:05")
	}
	if to != nil {
		to.Emit(e)
	}
	return e
}

// Bus fans one event out to several sinks — typically the terminal renderer
// and the durable JSONL log at the same time. A sink that fails is not allowed
// to take the others down with it: the log is diagnostics, and diagnostics
// must never be the reason a workflow dies.
type Bus []Emitter

func (b Bus) Emit(e Event) {
	for _, sink := range b {
		if sink == nil {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			sink.Emit(e)
		}()
	}
}

// JSON renders the event as one log line. Used by the JSONL sink and handy in
// tests, where asserting on the serialized form catches a field that silently
// stopped being populated.
func (e Event) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"type":"event.unmarshalable"}`
	}
	return string(b)
}
