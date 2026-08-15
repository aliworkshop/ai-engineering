package approval

import (
	"path/filepath"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// human is a front-end that can be told what to do, and counts how often it was
// asked — the number that matters, since the point of checkpointing a decision
// is that nobody is asked twice.
type human struct {
	approve  bool
	answers  bool // false = walked away, the case that parks the workflow
	asked    int
	lastSeen string
}

func (h *human) Confirm(action string) bool {
	approved, _ := h.Decide(action)
	return approved
}

func (h *human) Decide(action string) (bool, bool) {
	h.asked++
	h.lastSeen = action
	return h.approve, h.answers
}

type recorder struct{ seen []events.Event }

func (r *recorder) Emit(e events.Event) { r.seen = append(r.seen, e) }

func (r *recorder) has(t events.Type) bool {
	for _, e := range r.seen {
		if e.Type == t {
			return true
		}
	}
	return false
}

func harness(t *testing.T, live approver) (*Gate, *durable.Store, string, *recorder) {
	t.Helper()
	root := t.TempDir()
	rec := &recorder{}

	store, err := durable.NewStore(filepath.Join(root, "wf"), rec)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	decisions := filepath.Join(root, "decisions")
	gate, err := NewGate(decisions, live, rec)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	return gate, store, decisions, rec
}

// TestDecisionIsCheckpointed is the core of Part 7: a human is asked once, and
// every replay after that reads the answer off disk.
func TestDecisionIsCheckpointed(t *testing.T) {
	person := &human{approve: true, answers: true}
	gate, store, _, rec := harness(t, person)

	wf, _ := store.Open("run-1", "delete the file")
	gate.Attach(wf)
	gate.Scope("call_abc")

	if !gate.Confirm("DELETE file \"notes.md\"") {
		t.Fatalf("expected the human's yes to come through")
	}
	if person.asked != 1 {
		t.Fatalf("expected one question, got %d", person.asked)
	}
	if gate.Parked() != nil {
		t.Fatalf("an answered gate must not park the workflow")
	}

	// Replay: same workflow, same call id. The human must not be asked again —
	// that is what makes resuming after a crash safe rather than annoying.
	replayed, _ := store.Open("run-1", "")
	gate.Attach(replayed)
	gate.Scope("call_abc")

	if !gate.Confirm("DELETE file \"notes.md\"") {
		t.Fatalf("replay lost the recorded approval")
	}
	if person.asked != 1 {
		t.Fatalf("replay asked the human again (%d times total)", person.asked)
	}
	if !rec.has(events.ApprovalResolved) {
		t.Fatalf("expected an approval.resolved event")
	}
}

// TestNoAnswerParksTheWorkflow is the behaviour change the durable gate exists
// for: walking away is not a "no", it is "later".
func TestNoAnswerParksTheWorkflow(t *testing.T) {
	person := &human{answers: false} // asked, but never clicks
	gate, store, _, rec := harness(t, person)

	wf, _ := store.Open("run-2", "delete the file")
	gate.Attach(wf)
	gate.Scope("call_xyz")

	if gate.Confirm("DELETE file \"notes.md\"") {
		t.Fatalf("an unanswered gate must not approve")
	}

	parked := gate.Parked()
	if parked == nil {
		t.Fatalf("expected the workflow to be parked")
	}
	if _, ok := durable.IsSuspended(parked); !ok {
		t.Fatalf("expected a Suspended, got %T", parked)
	}
	if wf.Status() != durable.StatusSuspended {
		t.Fatalf("workflow status is %q, want suspended", wf.Status())
	}
	if !rec.has(events.ApprovalRequested) || !rec.has(events.WorkflowSuspended) {
		t.Fatalf("expected approval.requested and workflow.suspended events")
	}

	// Crucially: nothing was checkpointed. The tool never ran, so the replay
	// after approval has to reach this exact call again.
	if rec.has(events.ApprovalResolved) {
		t.Fatalf("a parked gate must not record a decision")
	}
}

// TestParkedRunResumesFromTheCLIDecision walks the whole Part 7 loop: park,
// process exits, a human answers days later, replay lands on the gate.
func TestParkedRunResumesFromTheCLIDecision(t *testing.T) {
	absent := &human{answers: false}
	gate, store, decisions, _ := harness(t, absent)

	wf, _ := store.Open("run-3", "refund the duplicate")
	gate.Attach(wf)
	gate.Scope("call_1")
	gate.Confirm("REFUND $49.00")

	if wf.Status() != durable.StatusSuspended {
		t.Fatalf("expected the run to park, status is %q", wf.Status())
	}

	// ... the process exits. Some time later, a human decides.
	if err := Write(decisions, "run-3", true, "ali"); err != nil {
		t.Fatalf("write decision: %v", err)
	}

	// A fresh gate with NO interactive human at all — the resume path.
	headless, err := NewGate(decisions, nil, nil)
	if err != nil {
		t.Fatalf("headless gate: %v", err)
	}
	resumed, _ := store.Open("run-3", "")
	headless.Attach(resumed)
	headless.Scope("call_1")

	if !headless.Confirm("REFUND $49.00") {
		t.Fatalf("expected the recorded decision to approve the resumed run")
	}
	if headless.Parked() != nil {
		t.Fatalf("a run with its decision waiting must not park again")
	}

	// The decision file is consumed, so it cannot silently approve the NEXT
	// thing the agent asks for.
	headless.Scope("call_2")
	if headless.Confirm("DELETE everything") {
		t.Fatalf("a spent decision approved a second, different action")
	}
}

// TestDeniedActionIsRecordedAsADecision checks the other branch: "no" is an
// answer, so it is checkpointed and the run continues rather than parking.
func TestDeniedActionIsRecordedAsADecision(t *testing.T) {
	person := &human{approve: false, answers: true}
	gate, store, _, _ := harness(t, person)

	wf, _ := store.Open("run-4", "delete the file")
	gate.Attach(wf)
	gate.Scope("call_no")

	if gate.Confirm("DELETE file \"prod.db\"") {
		t.Fatalf("expected a refusal")
	}
	if gate.Parked() != nil {
		t.Fatalf("a refusal is a decision, not a reason to park")
	}
	if wf.Status() == durable.StatusSuspended {
		t.Fatalf("a refused run must not be suspended")
	}

	replayed, _ := store.Open("run-4", "")
	gate.Attach(replayed)
	gate.Scope("call_no")
	if gate.Confirm("DELETE file \"prod.db\"") {
		t.Fatalf("replay flipped a recorded refusal into an approval")
	}
	if person.asked != 1 {
		t.Fatalf("replay re-asked a question that was already answered")
	}
}

// TestDifferentActionUnderTheSameCallIDIsNotReused guards the hash in the step
// name: a checkpoint may only satisfy the action it was given for.
func TestDifferentActionUnderTheSameCallIDIsNotReused(t *testing.T) {
	person := &human{approve: true, answers: true}
	gate, store, _, _ := harness(t, person)

	wf, _ := store.Open("run-5", "task")
	gate.Attach(wf)
	gate.Scope("call_same")

	gate.Confirm("DELETE file \"scratch.txt\"")
	gate.Confirm("DELETE file \"/etc/passwd\"") // same call id, different action

	if person.asked != 2 {
		t.Fatalf("a different action reused an earlier approval (asked %d times)", person.asked)
	}
}

// TestWithoutAWorkflowItIsJustAnApprover keeps the degraded path honest: a unit
// test or a caller that never opted into durability still gets a working gate.
func TestWithoutAWorkflowItIsJustAnApprover(t *testing.T) {
	person := &human{approve: true, answers: true}
	gate, _, _, _ := harness(t, person)

	if !gate.Confirm("WRITE file \"x\"") {
		t.Fatalf("expected the plain blocking ask to work with no workflow attached")
	}
	if gate.Parked() != nil {
		t.Fatalf("there is no workflow to park")
	}
}
