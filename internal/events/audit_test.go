package events

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAuditCountsWorkNotRequests is the distinction the audit exists to make.
// A resumed run re-REQUESTS the tool it already ran — the agent emits
// tool.requested, then the step comes back from the checkpoint — so counting
// requests would report a duplicate every time recovery worked perfectly.
func TestAuditCountsWorkNotRequests(t *testing.T) {
	log := write(t,
		Event{Type: WorkflowStarted, Workflow: "wf1"},
		Event{Type: ToolRequested, Workflow: "wf1", Name: "send_email", Call: "call_a"},
		Event{Type: StepCompleted, Workflow: "wf1", Name: "tool-call_a"},
		Event{Type: WorkflowFailed, Workflow: "wf1"},

		// The replay: same call requested again, but the step is cached.
		Event{Type: WorkflowResumed, Workflow: "wf1"},
		Event{Type: ToolRequested, Workflow: "wf1", Name: "send_email", Call: "call_a"},
		Event{Type: StepCached, Workflow: "wf1", Name: "tool-call_a"},
		Event{Type: WorkflowCompleted, Workflow: "wf1"},
	)

	report, err := Audit(log)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !report.Clean() {
		t.Fatalf("a replayed request is not a repeated side effect: %+v", report.Duplicated)
	}
	if report.Replayed != 1 {
		t.Fatalf("replayed=%d, want the one cached step", report.Replayed)
	}
	if report.Requested["wf1/call_a"] != 2 {
		t.Fatalf("the two requests should still be visible: %v", report.Requested)
	}
}

// The real failure: the same step ran twice, so whatever it did happened twice.
func TestAuditCatchesRepeatedWork(t *testing.T) {
	log := write(t,
		Event{Type: StepCompleted, Workflow: "wf1", Name: "tool-call_a"},
		Event{Type: WorkflowFailed, Workflow: "wf1"},
		Event{Type: StepCompleted, Workflow: "wf1", Name: "tool-call_a"},
	)

	report, err := Audit(log)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.Clean() {
		t.Fatal("the same step ran twice and the audit did not notice")
	}
	if len(report.Duplicated) != 1 || report.Duplicated[0].Count != 2 ||
		report.Duplicated[0].Step != "tool-call_a" || report.Duplicated[0].Workflow != "wf1" {
		t.Fatalf("wrong duplicate reported: %+v", report.Duplicated)
	}
}

// The same step name in two different workflows is two runs doing their own
// work — the workflow is part of the key for that reason.
func TestAuditKeepsWorkflowsApart(t *testing.T) {
	log := write(t,
		Event{Type: StepCompleted, Workflow: "wf1", Name: "model-00"},
		Event{Type: StepCompleted, Workflow: "wf2", Name: "model-00"},
	)

	report, err := Audit(log)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !report.Clean() || len(report.Workflows) != 2 {
		t.Fatalf("workflows were conflated: %+v", report)
	}
}

// Garbage lines are counted, not fatal: the log is diagnostics, and an audit
// that refuses to run because one line is half-written is useless exactly when
// it is needed — after a crash.
func TestAuditSurvivesABrokenLine(t *testing.T) {
	log := write(t, Event{Type: StepCompleted, Workflow: "wf1", Name: "model-00"})
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = f.WriteString("{\"type\":\"step.comp\n")
	f.Close()

	report, err := Audit(log)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if report.Skipped != 1 || report.Total != 1 {
		t.Fatalf("skipped=%d total=%d, want 1 and 1", report.Skipped, report.Total)
	}
}

func write(t *testing.T, list ...Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log := NewJSONL(path)
	for _, e := range list {
		log.Emit(Emit(nil, e))
	}
	return path
}
