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

// A judgment is not a side effect, and it does not repeat for the same reason.
// The same send verified twice is a replay leaking through; the same ITEM
// verified twice is a redraft being checked, which is the gate doing its job.
func TestARedraftIsNotARepeatedJudgment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path,
		Event{Type: WorkflowStarted, Workflow: "wf1"},
		Event{Type: JevRequested, Workflow: "wf1", Name: "triage item-3", Call: "triage-item-3"},
		// The first draft is blocked, so the model writes another and the gate
		// runs again — same purpose, different send, different key.
		Event{Type: JevRequested, Workflow: "wf1", Name: "verify item-3", Call: "verify-call_a"},
		Event{Type: JevGate, Workflow: "wf1", Name: "item-3", Output: "blocked"},
		Event{Type: JevRequested, Workflow: "wf1", Name: "verify item-3", Call: "verify-call_b"},
		Event{Type: JevGate, Workflow: "wf1", Name: "item-3", Output: "pass"},
		Event{Type: ToolRequested, Workflow: "wf1", Name: "sendReply", Call: "call_b"},
		Event{Type: WorkflowCompleted, Workflow: "wf1"},
	)

	report, err := Audit(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean() {
		t.Fatalf("a redraft was counted as a repeat: %+v", report.Rejudged)
	}
	if report.Judgments != 3 {
		t.Errorf("judgments = %d, want 3", report.Judgments)
	}
}

// And the failure this half is there to catch: the same decision paid for
// twice, because a recovery re-ran a step that had already been checkpointed.
func TestAJudgmentBoughtTwiceIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, path,
		Event{Type: JevRequested, Workflow: "wf1", Name: "triage item-1", Call: "triage-item-1"},
		Event{Type: JevRequested, Workflow: "wf1", Name: "triage item-1", Call: "triage-item-1"},
	)

	report, err := Audit(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean() {
		t.Fatal("the same triage ran twice and the audit called it clean")
	}
	if len(report.Rejudged) != 1 || report.Rejudged[0].Count != 2 {
		t.Fatalf("rejudged = %+v, want one entry counted twice", report.Rejudged)
	}
	if len(report.Duplicated) != 0 {
		t.Errorf("a judgment was reported as a repeated side effect: %+v", report.Duplicated)
	}
}
