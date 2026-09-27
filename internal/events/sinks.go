package events

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Bus fans one event out to several sinks. It is itself an Emitter, so
// anything that takes one place to report to can be handed many.
type Bus []Emitter

func (b Bus) Emit(e Event) {
	for _, sink := range b {
		if sink != nil {
			sink.Emit(e)
		}
	}
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

	// The three fields the line already shows as columns are zeroed on a copy
	// before marshalling, so they do not appear twice. Every one of them is
	// omitempty, which is what makes this work.
	detail := e
	detail.TS, detail.Type, detail.Workflow = "", "", ""

	raw, err := json.Marshal(detail)
	if err != nil {
		raw = []byte("{}")
	}
	fmt.Fprintf(c.out, "  %s  %-21s %-9s %s\n", mark, e.Type, e.Workflow, raw)
}

// JSONL appends every event to a file, one JSON object per line.
//
// This is the change that makes the stream outlive the process. Part 1 could
// tell you what the agent was doing only while you watched; this can tell you
// what it did last Tuesday, and — because every event carries a timestamp and
// a workflow id — cost and latency reporting become a script over a file
// rather than new instrumentation.
type JSONL struct {
	mu   sync.Mutex
	path string
}

// NewJSONL appends to path, creating it and its directory as needed.
func NewJSONL(path string) *JSONL { return &JSONL{path: path} }

// Emit appends one line. A logging failure is swallowed on purpose: a broken
// log should not take down the run it was only supposed to describe.
//
// The file is opened per write rather than held open. At this volume the cost
// is irrelevant, and it means a log that is deleted or rotated mid-run simply
// starts again on the next event instead of writing into a vanished handle.
func (j *JSONL) Emit(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	// O_CREATE makes the file, not the directory above it. Creating that here
	// rather than at construction keeps the promise the open-per-write makes:
	// a log whose directory is deleted mid-run starts again on the next event.
	if dir := filepath.Dir(j.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}

	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}
