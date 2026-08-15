// Package durable turns the agent loop from a script into a runtime.
//
// The problem it solves is not "remember the conversation". It is this: the
// agent asks to delete a file, the tool deletes it, and the process dies before
// anything is written down. Re-run the task and it deletes again — except now
// the file is gone and the second attempt does something else entirely. State
// you can rebuild is cheap; side effects you already caused are not.
//
// The mechanism is small enough to hold in your head. A workflow is a JSON file
// on disk. A step is a named unit of work whose result is checkpointed the
// instant it finishes. To recover from a crash you simply re-run the workflow
// body: completed steps return their cached result without executing — no model
// call, no side effect, no cost — and execution races forward to exactly where
// it died. That replay trick is the core of every durable-execution engine;
// swapping these JSON files for Postgres changes the storage, not the idea.
package durable

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// Status is where a workflow stands. Suspended is the interesting one: it is
// not an error and not a completion, it is a workflow that is *waiting for a
// human* (Part 7) with no process holding it open.
type Status string

const (
	StatusRunning   Status = "running"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusSuspended Status = "suspended"
)

// Suspended parks a workflow. It travels up as an ordinary error so every
// caller in between gets to unwind normally — but callers that understand it
// (the CLI) report "waiting for a human" instead of "crashed".
type Suspended struct {
	ID     string
	Reason string
}

func (s *Suspended) Error() string {
	return fmt.Sprintf("workflow %s suspended: %s", s.ID, s.Reason)
}

// IsSuspended reports whether an error is a park rather than a failure. Use
// this instead of comparing strings; the distinction decides whether a human
// sees "something broke" or "your approval is waiting".
func IsSuspended(err error) (*Suspended, bool) {
	var s *Suspended
	ok := errors.As(err, &s)
	return s, ok
}

// Store is a directory of workflow files. One file per workflow, named by id.
type Store struct {
	dir    string
	events events.Emitter
}

// NewStore opens (and creates) a directory of workflow state. The emitter is
// where lifecycle and replay events go; nil is legal and means "run silently",
// which keeps tests from having to wire a sink.
func NewStore(dir string, emitter events.Emitter) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("durable: create store dir: %w", err)
	}
	return &Store{dir: dir, events: emitter}, nil
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// state is the on-disk shape. It is deliberately boring and self-describing:
// you should be able to cat a workflow file mid-incident and understand what
// the agent had done, without a tool.
type state struct {
	ID      string                     `json:"id"`
	Input   string                     `json:"input"`
	Agent   string                     `json:"agent,omitempty"` // current specialist (Part 5)
	Status  Status                     `json:"status"`
	Started string                     `json:"started"`
	Steps   map[string]json.RawMessage `json:"steps"`
	Order   []string                   `json:"order"` // step names, in completion order
}

// Workflow is one durable run. Every mutation writes the whole file — at this
// size that is both fastest and safest, since a single write means there is no
// window where the file describes half a checkpoint.
type Workflow struct {
	mu    sync.Mutex // Part 6 runs steps from several goroutines at once
	store *Store
	st    state

	// resumed records that this workflow was loaded from disk with work already
	// checkpointed, which is what makes a replay distinguishable from a fresh
	// run in the event stream.
	resumed bool
}

// Open loads the workflow with this id, or starts a new one with the given
// input. That "or" is the entire recovery API: the caller does not branch on
// crashed-vs-fresh, it just opens the id and re-runs the body.
func (s *Store) Open(id, input string) (*Workflow, error) {
	w := &Workflow{store: s}

	raw, err := os.ReadFile(s.path(id))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &w.st); err != nil {
			return nil, fmt.Errorf("durable: workflow %s is corrupt: %w", id, err)
		}
		w.resumed = len(w.st.Steps) > 0
		// A resumed workflow is runnable again by definition — the caller only
		// re-opens one it intends to drive.
		w.st.Status = StatusRunning
		if input != "" {
			w.st.Input = input
		}

	case errors.Is(err, os.ErrNotExist):
		w.st = state{
			ID:      id,
			Input:   input,
			Status:  StatusRunning,
			Started: time.Now().Format(time.RFC3339),
			Steps:   map[string]json.RawMessage{},
		}

	default:
		return nil, fmt.Errorf("durable: read workflow %s: %w", id, err)
	}

	if w.st.Steps == nil {
		w.st.Steps = map[string]json.RawMessage{}
	}
	if err := w.save(); err != nil {
		return nil, err
	}

	kind := events.WorkflowStarted
	if w.resumed {
		kind = events.WorkflowResumed
	}
	events.Emit(s.events, events.Event{
		Type:     kind,
		Workflow: id,
		Agent:    w.st.Agent,
		Text:     truncate(w.st.Input, 80),
	})
	return w, nil
}

// ID, Input, Status, Agent and Resumed expose the state a caller legitimately
// needs without handing out the struct it would then be tempted to mutate.
func (w *Workflow) ID() string { return w.st.ID }
func (w *Workflow) Resumed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resumed
}

func (w *Workflow) Input() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.Input
}

func (w *Workflow) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.Status
}

// Agent and SetAgent track which specialist currently holds the conversation
// (Part 5). It lives in the workflow file so a handoff survives a crash — a
// resumed run picks up as billing, not back at triage.
func (w *Workflow) Agent() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.Agent
}

func (w *Workflow) SetAgent(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Agent = name
	return w.save()
}

// Finish records a terminal (or parked) status. It is separate from the step
// machinery because the *outcome* of a workflow is not itself a unit of work.
func (w *Workflow) Finish(status Status) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Status = status
	return w.save()
}

// save writes the whole state file. Callers hold w.mu.
//
// It writes to a temp file and renames, because the failure this package exists
// to prevent is a crash at the worst possible moment — and a torn state file
// would turn one lost step into an unrecoverable workflow.
func (w *Workflow) save() error {
	raw, err := json.MarshalIndent(w.st, "", "  ")
	if err != nil {
		return fmt.Errorf("durable: encode workflow %s: %w", w.st.ID, err)
	}
	final := w.store.path(w.st.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("durable: write workflow %s: %w", w.st.ID, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("durable: commit workflow %s: %w", w.st.ID, err)
	}
	return nil
}

// Cached reports whether a step already has a checkpointed result, without
// running anything. The approval gate uses it to tell "the human already
// answered this in an earlier run" from "nobody has been asked yet".
func (w *Workflow) Cached(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.st.Steps[name]
	return ok
}

// Lookup reads a checkpointed step without running anything. Step covers the
// common case; this exists for the caller that needs to *inspect* a past
// decision — the approval gate asking "did a human already answer this?" —
// where there is no function to fall back to.
func Lookup[T any](w *Workflow, name string) (T, bool, error) {
	var zero T

	w.mu.Lock()
	raw, ok := w.st.Steps[name]
	w.mu.Unlock()
	if !ok {
		return zero, false, nil
	}

	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, true, fmt.Errorf("durable: decode cached step %q: %w", name, err)
	}
	return out, true, nil
}

// Step is the whole point of the package: run fn once, ever, for this workflow.
//
// On the first pass it executes, checkpoints the result, and returns it. On
// every replay after a crash it returns the stored value and fn is never
// called — so a step that emailed a customer, moved money, or deleted a file
// does not do it twice, and a step that cost a model call does not cost a
// second one.
//
// It is a free function rather than a method because Go does not allow type
// parameters on methods, and a typed result is worth the slightly odd spelling:
// the caller gets its own struct back, not a map to re-assert.
func Step[T any](w *Workflow, name string, fn func() (T, error)) (T, error) {
	var zero T

	w.mu.Lock()
	raw, cached := w.st.Steps[name]
	w.mu.Unlock()

	if cached {
		var out T
		if err := json.Unmarshal(raw, &out); err != nil {
			return zero, fmt.Errorf("durable: decode cached step %q: %w", name, err)
		}
		events.Emit(w.store.events, events.Event{
			Type: events.StepCached, Workflow: w.st.ID, Name: name,
		})
		return out, nil
	}

	start := time.Now()
	out, err := fn()
	if err != nil {
		// Deliberately NOT checkpointed. A failed step has no result to cache,
		// and pinning the failure would make the crash permanent — retrying the
		// workflow has to mean retrying the thing that broke.
		return zero, err
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return zero, fmt.Errorf("durable: encode step %q: %w", name, err)
	}

	w.mu.Lock()
	w.st.Steps[name] = encoded
	w.st.Order = append(w.st.Order, name)
	saveErr := w.save()
	w.mu.Unlock()
	if saveErr != nil {
		return zero, saveErr
	}

	// step.completed, not tool.completed: this package checkpoints model turns,
	// human decisions, and whole investigations as well as tools, and calling
	// all of them "tool" makes the stream lie about what happened.
	events.Emit(w.store.events, events.Event{
		Type: events.StepCompleted, Workflow: w.st.ID, Name: name,
		Millis: time.Since(start).Milliseconds(),
	})
	return out, nil
}

// Record checkpoints a value that was produced somewhere else — most often a
// human decision (Part 7), where the "work" happened in a different process on
// a different day. It is Step with the execution half removed.
func Record[T any](w *Workflow, name string, value T) error {
	_, err := Step(w, name, func() (T, error) { return value, nil })
	return err
}

// Summary is the listable shape of a workflow, for the CLI's "what is waiting
// on me?" view.
type Summary struct {
	ID      string
	Status  Status
	Agent   string
	Input   string
	Steps   int
	Started string
}

// List reports every workflow in the store, newest first. This is how a human
// finds the id of the run that parked itself yesterday.
func (s *Store) List() ([]Summary, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("durable: list workflows: %w", err)
	}

	var out []Summary
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue // a workflow we can't read shouldn't hide the ones we can
		}
		var st state
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		out = append(out, Summary{
			ID: st.ID, Status: st.Status, Agent: st.Agent,
			Input: truncate(st.Input, 60), Steps: len(st.Steps), Started: st.Started,
		})
	}

	// Started is RFC3339, which sorts lexically, so newest-first is a reverse
	// string compare — no time parsing and no error path.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Started > out[i].Started {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func truncate(s string, max int) string {
	r := []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
