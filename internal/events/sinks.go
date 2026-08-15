package events

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// glyph gives each event type a one-character face. A stream of these is
// scannable at a glance in a way a stream of words is not: you see the shape of
// a run — think, call, call, park — before you read a single field.
var glyph = map[Type]string{
	WorkflowStarted:   "▶",
	WorkflowResumed:   "⟲",
	WorkflowCompleted: "✔",
	WorkflowFailed:    "✘",
	WorkflowSuspended: "⏸",
	ModelCompleted:    "🧠",
	ToolRequested:     "⚙",
	ToolCompleted:     "✓",
	StepCompleted:     "▪",
	StepCached:        "⏩",
	MemoryCompacted:   "🗜",
	AgentHandoff:      "↪",
	PlanCreated:       "🗺",
	SubagentStarted:   "├",
	SubagentCompleted: "✓",
	SubagentFailed:    "✘",
	ApprovalRequested: "✋",
	ApprovalResolved:  "🖊",
}

// Console renders events as one glyphed line each on a writer. It is the
// harness's inspector: the same stream the durable log stores, made readable.
//
// It holds a mutex because Part 6 fans investigations out across goroutines,
// and interleaved half-lines would make exactly the concurrency you are trying
// to observe impossible to read.
type Console struct {
	mu  sync.Mutex
	out io.Writer

	// Quiet drops the routine per-tool chatter, keeping only the events a
	// human watching a normal session cares about. The terminal UI already
	// prints its own tool lines, so without this the two would double up.
	Quiet bool
}

// NewConsole renders to the given writer (normally os.Stdout).
func NewConsole(out io.Writer) *Console { return &Console{out: out} }

func (c *Console) Emit(e Event) {
	if c.Quiet && routine(e.Type) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	mark := glyph[e.Type]
	if mark == "" {
		mark = "·"
	}
	fmt.Fprintf(c.out, "  %s %-19s %s\n", mark, e.Type, truncate(detail(e), 90))
}

// routine reports whether an event is normal per-step noise rather than a
// state change worth interrupting a human for.
func routine(t Type) bool {
	switch t {
	case ToolRequested, ToolCompleted, ModelCompleted, StepCompleted, StepCached:
		return true
	}
	return false
}

// detail renders the populated fields of an event compactly. It deliberately
// does not marshal the struct: the JSONL log wants every field, but a human
// scanning the terminal wants the two that changed.
func detail(e Event) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("wf", e.Workflow)
	add("agent", e.Agent)
	add("name", e.Name)
	add("from", e.From)
	add("to", e.To)
	add("args", oneLine(e.Args))
	add("result", oneLine(e.Result))
	add("text", oneLine(e.Text))
	add("error", e.Error)
	if e.Approved != nil {
		add("approved", fmt.Sprintf("%t", *e.Approved))
	}
	if e.Tokens > 0 {
		add("tokens", fmt.Sprintf("%d", e.Tokens))
	}
	if e.Millis > 0 {
		add("ms", fmt.Sprintf("%d", e.Millis))
	}
	return strings.Join(parts, " ")
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// JSONL appends every event to a file, one JSON object per line. This is the
// durable half of Part 2: the workflow file holds *state* (what is true now),
// while this holds *history* (everything that happened, in order). You can
// delete the workflow file and still reconstruct the run from here.
//
// Being append-only is what makes it safe under a crash — there is no record
// to half-rewrite — and what makes the Part 4 distinction possible: history
// lives here and is never sent to the model wholesale.
type JSONL struct {
	mu   sync.Mutex
	path string
}

// NewJSONL logs to the given path, creating its directory if needed.
func NewJSONL(path string) *JSONL {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return &JSONL{path: path}
}

func (l *JSONL) Emit(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Opened per write rather than held open: an event stream is low-volume,
	// and a handle that survives a crash mid-buffer is worth less than a line
	// that is on disk the moment it is emitted.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, e.JSON())
}

// Path is where this log writes — surfaced so the CLI can point a human at it.
func (l *JSONL) Path() string { return l.path }
