package durable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// recorder collects events so a test can assert on what the harness reported,
// not just on what it returned.
type recorder struct{ seen []events.Event }

func (r *recorder) Emit(e events.Event) { r.seen = append(r.seen, e) }

func (r *recorder) count(t events.Type) int {
	n := 0
	for _, e := range r.seen {
		if e.Type == t {
			n++
		}
	}
	return n
}

func newStore(t *testing.T) (*Store, *recorder) {
	t.Helper()
	rec := &recorder{}
	store, err := NewStore(filepath.Join(t.TempDir(), "wf"), rec)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, rec
}

// TestReplaySkipsCompletedSteps is the claim the whole package exists to make:
// after a crash, work that already happened does not happen again.
func TestReplaySkipsCompletedSteps(t *testing.T) {
	store, rec := newStore(t)

	// Pass one: two steps run, and the second "crashes" the process after it.
	sent := 0
	wf, err := store.Open("abc123", "refund the duplicate charge")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, name := range []string{"lookup", "send-email"} {
		if _, err := Step(wf, name, func() (string, error) {
			sent++
			return name + "-done", nil
		}); err != nil {
			t.Fatalf("step %s: %v", name, err)
		}
	}
	if sent != 2 {
		t.Fatalf("first pass should have executed 2 steps, executed %d", sent)
	}

	// Pass two: same id, same body. Nothing may execute a second time — this is
	// the difference between resuming and re-sending the customer a second
	// email.
	replayed, err := store.Open("abc123", "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !replayed.Resumed() {
		t.Fatalf("expected the reopened workflow to report itself as resumed")
	}
	if got := replayed.Input(); got != "refund the duplicate charge" {
		t.Fatalf("expected the task to come back off disk, got %q", got)
	}

	for _, name := range []string{"lookup", "send-email"} {
		out, err := Step(replayed, name, func() (string, error) {
			sent++
			return "SHOULD NOT RUN", nil
		})
		if err != nil {
			t.Fatalf("replay step %s: %v", name, err)
		}
		if out != name+"-done" {
			t.Fatalf("replay of %s returned %q, want the cached result", name, out)
		}
	}
	if sent != 2 {
		t.Fatalf("replay executed %d extra steps; it must execute none", sent-2)
	}

	// And a NEW step after the cached ones does run — replay races forward to
	// exactly where it died and carries on.
	out, err := Step(replayed, "confirm", func() (string, error) {
		sent++
		return "confirmed", nil
	})
	if err != nil || out != "confirmed" {
		t.Fatalf("new step after replay: got %q, %v", out, err)
	}
	if sent != 3 {
		t.Fatalf("expected exactly one new step to execute, total %d", sent)
	}

	if rec.count(events.StepCached) != 2 {
		t.Fatalf("expected 2 step.cached events, got %d", rec.count(events.StepCached))
	}
	if rec.count(events.WorkflowResumed) != 1 {
		t.Fatalf("expected 1 workflow.resumed event, got %d", rec.count(events.WorkflowResumed))
	}
}

// TestFailedStepIsNotCheckpointed guards the subtlest rule in the package: a
// step that failed has no result, and pinning the failure would make the crash
// permanent.
func TestFailedStepIsNotCheckpointed(t *testing.T) {
	store, _ := newStore(t)
	wf, err := store.Open("retry-me", "task")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	boom := errors.New("network is down")
	if _, err := Step(wf, "flaky", func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("expected the step's error to surface, got %v", err)
	}
	if wf.Cached("flaky") {
		t.Fatalf("a failed step must not be checkpointed — retrying it would be impossible")
	}

	out, err := Step(wf, "flaky", func() (string, error) { return "worked this time", nil })
	if err != nil || out != "worked this time" {
		t.Fatalf("retry after failure: got %q, %v", out, err)
	}
}

// TestStructuredResultsRoundTrip covers the checkpoint carrying something
// richer than a string, which is what the approval gate stores.
func TestStructuredResultsRoundTrip(t *testing.T) {
	type verdict struct {
		Approved bool   `json:"approved"`
		By       string `json:"by"`
	}

	store, _ := newStore(t)
	wf, _ := store.Open("shapes", "task")

	if err := Record(wf, "approval-1", verdict{Approved: true, By: "ali"}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, cached, err := Lookup[verdict](wf, "approval-1")
	if err != nil || !cached {
		t.Fatalf("lookup: cached=%v err=%v", cached, err)
	}
	if !got.Approved || got.By != "ali" {
		t.Fatalf("round trip lost data: %+v", got)
	}

	if _, cached, _ := Lookup[verdict](wf, "never-happened"); cached {
		t.Fatalf("a step that was never run must not report as cached")
	}
}

// TestSuspensionIsAStatusNotAFailure checks that parking a workflow leaves it
// alive on disk and recognizable as waiting rather than broken.
func TestSuspensionIsAStatusNotAFailure(t *testing.T) {
	store, _ := newStore(t)
	wf, _ := store.Open("parked", "delete the thing")

	if err := wf.Finish(StatusSuspended); err != nil {
		t.Fatalf("finish: %v", err)
	}

	rows, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != StatusSuspended {
		t.Fatalf("expected one suspended workflow, got %+v", rows)
	}

	err = &Suspended{ID: "parked", Reason: "awaiting approval"}
	parked, ok := IsSuspended(err)
	if !ok || parked.ID != "parked" {
		t.Fatalf("IsSuspended failed to recognize a park: %v", err)
	}
	if _, ok := IsSuspended(errors.New("real failure")); ok {
		t.Fatalf("IsSuspended must not claim an ordinary error is a park")
	}
}

// TestStateSurvivesOnDiskAlone reopens through a brand new Store, the way a
// second process would — the in-memory Workflow is gone, and only the file is
// left.
func TestStateSurvivesOnDiskAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wf")

	first, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	wf, _ := first.Open("cross-process", "write the file")
	if _, err := Step(wf, "tool-1", func() (string, error) { return "wrote it", nil }); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := wf.SetAgent("operator"); err != nil {
		t.Fatalf("set agent: %v", err)
	}

	second, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("second store: %v", err)
	}
	reopened, err := second.Open("cross-process", "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Agent() != "operator" {
		t.Fatalf("a handoff must survive the process: agent is %q", reopened.Agent())
	}
	out, err := Step(reopened, "tool-1", func() (string, error) {
		t.Fatal("cached step re-executed in a fresh process")
		return "", nil
	})
	if err != nil || out != "wrote it" {
		t.Fatalf("cross-process replay: %q, %v", out, err)
	}
}

// TestCorruptWorkflowIsReported prefers a clear error over silently starting
// over, which would re-run every side effect the file was there to prevent.
func TestCorruptWorkflowIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wf")
	store, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := store.Open("broken", "task"); err == nil {
		t.Fatalf("expected opening a corrupt workflow to fail loudly")
	}
}
