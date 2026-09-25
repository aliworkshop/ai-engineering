package events

import (
	"os"
	"path/filepath"
	"testing"
)

// write builds a log the way a run would: one Emit per line.
func write(t *testing.T, path string, list ...Event) {
	t.Helper()
	sink := NewJSONL(path)
	for _, e := range list {
		Emit(sink, e)
	}
}

// The distinction the audit exists to make: a replayed step is not a repeated
// side effect. A clean recovery serves steps from cache and never re-emits a
// tool request, so the count stays at one per call.
func TestReplayIsNotADuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path,
		Event{Type: WorkflowStarted, Workflow: "wf1"},
		Event{Type: ToolRequested, Workflow: "wf1", Name: "sendReply", Call: "call_a"},
		Event{Type: ToolCompleted, Workflow: "wf1", Name: "sendReply", Call: "call_a"},
		// …crash, then a recovery run that replays the step it already did.
		Event{Type: WorkflowResumed, Workflow: "wf1"},
		Event{Type: StepCached, Workflow: "wf1", Name: "tool-call_a"},
		Event{Type: WorkflowCompleted, Workflow: "wf1"},
	)

	report, err := Audit(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean() {
		t.Fatalf("a replayed step was counted as a repeated side effect: %+v", report.Duplicated)
	}
	if report.Calls != 1 || report.Replayed != 1 {
		t.Fatalf("calls=%d replayed=%d, want 1 and 1", report.Calls, report.Replayed)
	}
}

// And the failure it is there to catch: the same call actually running twice,
// which is what a re-run without checkpoints does.
func TestRepeatedSideEffectIsCaught(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path,
		Event{Type: ToolRequested, Workflow: "wf1", Name: "sendReply", Call: "call_a"},
		Event{Type: ToolRequested, Workflow: "wf1", Name: "sendReply", Call: "call_a"},
		// A different call to the same tool is normal: three items, three sends.
		Event{Type: ToolRequested, Workflow: "wf1", Name: "sendReply", Call: "call_b"},
	)

	report, err := Audit(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Duplicated) != 1 {
		t.Fatalf("duplicated = %+v, want exactly the one repeated call", report.Duplicated)
	}
	if d := report.Duplicated[0]; d.Call != "call_a" || d.Count != 2 || d.Name != "sendReply" {
		t.Fatalf("wrong duplicate reported: %+v", d)
	}
	if report.Calls != 2 {
		t.Fatalf("calls = %d, want 2 distinct", report.Calls)
	}
}

// A torn final line — the process died mid-write — must not stop the audit.
// That is precisely when it is needed.
func TestTornLineIsCountedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path, Event{Type: ToolRequested, Workflow: "wf1", Call: "call_a"})

	f, err := openAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"tool.req`)
	f.Close()

	report, err := Audit(path)
	if err != nil {
		t.Fatalf("a torn log should still audit: %v", err)
	}
	if report.Total != 2 || report.Calls != 1 {
		t.Fatalf("total=%d calls=%d, want 2 and 1", report.Total, report.Calls)
	}
}

// openAppend is the test's own opener; the sink keeps its file private.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
