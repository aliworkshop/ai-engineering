// Package durable turns the agent loop from a script into a runtime.
//
// The problem it solves is not "remember the conversation". It is this: the
// agent asks to email a customer, the tool sends it, and the process dies
// before anything is written down. Re-run the task and it sends again — and
// nobody can tell the second mail from the first, because there is no record
// that either happened.
//
// The mechanism is small enough to hold in your head. A workflow is a JSON
// file on disk. A STEP is a named unit of work whose result is checkpointed the
// instant it finishes. To recover from a crash you simply re-run the workflow
// body: completed steps return their cached result without executing — no
// model call, no side effect, no cost — and execution races forward to exactly
// where it died.
//
// There is no separate recovery path. Resuming IS running. That replay trick
// is the core of every durable-execution engine; swapping these JSON files for
// Postgres changes the storage, not the idea.
package durable

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// Status is where a workflow stands. Running is the interesting one on disk:
// a workflow still marked running when no process is alive is a workflow that
// crashed, and that is exactly what Pending looks for.
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Store is a directory of workflow files, one JSON file per workflow.
type Store struct {
	dir string
	bus events.Emitter
}

// NewStore opens (and creates) a directory of workflow state. The emitter is
// where replay events go; nil is legal and means "run silently", which keeps a
// test from having to wire a sink.
func NewStore(dir string, bus events.Emitter) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("durable: create store %s: %w", dir, err)
	}
	return &Store{dir: dir, bus: bus}, nil
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// state is the on-disk shape. It is deliberately boring and self-describing:
// you should be able to cat a workflow file mid-incident and understand what
// the agent had done, without a tool.
type state struct {
	ID     string                     `json:"id"`
	Input  string                     `json:"input"`
	Status Status                     `json:"status"`
	Order  []string                   `json:"order"` // the steps, in the order they finished
	Steps  map[string]json.RawMessage `json:"steps"`
}

// Workflow is one durable run.
type Workflow struct {
	mu      sync.Mutex
	store   *Store
	st      state
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
		// A corrupt file fails loudly. Starting over silently would repeat
		// every side effect the file was written to prevent.
		if err := json.Unmarshal(raw, &w.st); err != nil {
			return nil, fmt.Errorf("durable: workflow %s is corrupt: %w", id, err)
		}
		w.resumed = true
		w.st.Status = StatusRunning

	case os.IsNotExist(err):
		w.st = state{ID: id, Input: input, Status: StatusRunning}

	default:
		return nil, fmt.Errorf("durable: read workflow %s: %w", id, err)
	}

	if w.st.Steps == nil {
		w.st.Steps = map[string]json.RawMessage{}
	}
	return w, w.save()
}

// ID, Input, Status and Resumed expose the state a caller legitimately needs
// without handing out the struct it would then be tempted to mutate.
func (w *Workflow) ID() string { return w.st.ID }

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

// Resumed reports whether this run is a replay of one that stopped early.
func (w *Workflow) Resumed() bool { return w.resumed }

// Cached reports whether a step already has a checkpointed result, without
// running anything.
func (w *Workflow) Cached(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.st.Steps[name]
	return ok
}

// Finish records a terminal status. It is separate from the step machinery
// because the outcome of a workflow is not itself a unit of work.
func (w *Workflow) Finish(status Status) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st.Status = status
	return w.save()
}

// save writes the whole state file. Callers hold w.mu.
//
// It writes to a temp file and renames, because the failure this package
// exists to prevent is a crash at the worst possible moment — and a torn state
// file would turn one lost step into an unrecoverable workflow.
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

// Step is the whole point of the package: run fn once, ever, for this workflow.
//
// On the first pass it executes, checkpoints the result, and returns it. On
// every replay after a crash it returns the stored value and fn is NEVER
// called — so a step that emailed a customer does not email them twice, and a
// step that cost a model call does not cost a second one.
//
// It is a free function rather than a method because Go does not allow type
// parameters on methods, and a typed result is worth the slightly odd
// spelling: the caller gets its own struct back, not a map to re-assert.
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
		events.Emit(w.store.bus, events.Event{
			Type: events.StepCached, Workflow: w.st.ID, Name: name,
		})
		return out, nil
	}

	start := time.Now()
	out, err := fn()
	if err != nil {
		// Deliberately NOT checkpointed. A failed step has no result to cache,
		// and pinning the failure would make the crash permanent — retrying
		// the workflow has to mean retrying the thing that broke.
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

	events.Emit(w.store.bus, events.Event{
		Type: events.StepCompleted, Workflow: w.st.ID, Name: name,
		Millis: time.Since(start).Milliseconds(),
	})
	return out, nil
}

// Row is one workflow, summarized for a listing.
type Row struct {
	ID     string
	Input  string
	Status Status
	Steps  int
}

// List summarizes every workflow in the store, oldest id first.
func (s *Store) List() ([]Row, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var rows []Row
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var st state
		if err := json.Unmarshal(raw, &st); err != nil {
			// One unreadable file should not hide the rest of the runtime.
			continue
		}
		rows = append(rows, Row{ID: st.ID, Input: st.Input, Status: st.Status, Steps: len(st.Steps)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// Pending is every workflow that was mid-flight when the process last died —
// still marked running with nobody running it. This is what a durable engine
// looks for on launch, and replaying them is what recovery means.
func (s *Store) Pending() ([]Row, error) {
	rows, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Row
	for _, row := range rows {
		if row.Status == StatusRunning {
			out = append(out, row)
		}
	}
	return out, nil
}
