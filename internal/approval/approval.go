// Package approval makes a human decision a durable part of the workflow
// instead of a blocked goroutine.
//
// The naive version is one line with three bugs in it:
//
//	approved := approver.Confirm(action)   // 1. holds the process open for the whole wait
//	                                       // 2. dies on restart — the pending question is gone
//	                                       // 3. a human might answer in 30 seconds or 3 days
//
// The fix needs no new machinery, because Part 2 already built it: a human
// decision is just another checkpointed step — one whose value comes from a
// person rather than from a function. If nobody answers, the workflow parks
// itself on disk and the process is free to exit. When the decision lands —
// today or Thursday — replay flies through the cached steps and stops exactly
// on the gate with the answer waiting.
//
// The interactive y/n is kept, because a human who *is* sitting there should
// not have to run a second command. It is now just the fast path into the same
// durable step.
package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// Decider is an approver that can distinguish "the human said no" from "no
// human answered". A plain tools.Approver returns one bool and cannot: a
// closed browser tab and a deliberate refusal both arrive as false, and those
// two deserve opposite handling — one should park the workflow for later, the
// other should tell the model it was refused.
//
// Front-ends that can tell the difference implement this; the Gate falls back
// to the plain interface for the ones that can't (test stubs, mostly).
type Decider interface {
	Decide(action string) (approved, answered bool)
}

// approver is the minimum the Gate needs from an interactive front-end. It is
// declared here rather than imported from tools so this package sits *under*
// the tools it gates and nothing points back the other way.
type approver interface {
	Confirm(action string) bool
}

// Gate is a tools.Approver that checkpoints what the human said.
//
// One Gate serves a whole process and is attached to each workflow as it runs,
// because the tools are built once at startup (they need an Approver then)
// while workflows come and go.
type Gate struct {
	mu   sync.Mutex
	dir  string   // where decisions from the `-approve` CLI land
	live approver // the interactive human, if there is one
	bus  events.Emitter

	wf     *durable.Workflow
	callID string // the tool call the next Confirm belongs to
	parked *durable.Suspended
}

// decision is the on-disk shape written by the approve CLI. It is a struct
// rather than a bare bool so a reviewer reading decisions/<id>.json six months
// later can see what was approved, not just that something was.
//
// CallID is the safety belt. A decision is for ONE tool call, and replay is
// what guarantees the resumed run reaches that same call — but "the replay is
// correct" is an assumption, and this is the assumption that must not fail
// silently, because failing it means a human's yes authorising an action they
// never saw. Binding the verdict to the call id turns a subtle divergence into
// a second approval prompt.
type decision struct {
	Approved bool   `json:"approved"`
	CallID   string `json:"call_id,omitempty"`
	Action   string `json:"action,omitempty"`
	By       string `json:"by,omitempty"`
}

// Pending is what a parked workflow is waiting on, written next to the decision
// so `-list` can show a human what they are being asked to approve — and so
// Write can bind their answer to it.
type Pending struct {
	Workflow string `json:"workflow"`
	CallID   string `json:"call_id"`
	Action   string `json:"action"`
}

// NewGate stores decisions under dir. live may be nil — that is the headless
// case, where every gate parks the workflow and waits for the CLI.
func NewGate(dir string, live approver, bus events.Emitter) (*Gate, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("approval: create decisions dir: %w", err)
	}
	return &Gate{dir: dir, live: live, bus: bus}, nil
}

// Attach binds the gate to the workflow now running and clears any park left
// over from the previous one.
func (g *Gate) Attach(wf *durable.Workflow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.wf, g.parked, g.callID = wf, nil, ""
}

// Scope names the tool call the next Confirm belongs to. The agent calls it
// before dispatching each tool, which is what lets the checkpoint be keyed by
// call id — the same key the model's own tool call carries, and therefore the
// same key on replay.
func (g *Gate) Scope(callID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.callID = callID
}

// Parked reports the suspension, if the last gate could not get an answer. The
// agent checks this after each tool call and unwinds — leaving the workflow on
// disk exactly where it stopped.
func (g *Gate) Parked() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked == nil {
		return nil
	}
	return g.parked
}

// Confirm implements tools.Approver. Four cases, in order of cost:
//
//  1. Replay — the decision is already checkpointed. Return it. No human is
//     bothered twice for the same call, which is what makes resuming safe.
//  2. A decision file is waiting (the CLI answered while we were away).
//     Consume it, checkpoint it, proceed.
//  3. A human is here. Ask them, checkpoint the answer.
//  4. Nobody answered. Emit approval.requested, park the workflow, and let the
//     process die. This costs nothing to wait on.
func (g *Gate) Confirm(action string) bool {
	g.mu.Lock()
	wf, callID := g.wf, g.callID
	g.mu.Unlock()

	// With no workflow attached the durable half is simply unavailable — a unit
	// test, or a caller that hasn't opted in. Degrade to the plain blocking ask
	// rather than refusing to work.
	if wf == nil {
		if g.live == nil {
			return false
		}
		return g.live.Confirm(action)
	}

	step := stepName(callID, action)

	// 1. replay
	if d, cached, err := durable.Lookup[decision](wf, step); cached && err == nil {
		g.emitResolved(wf, action, d.Approved, "replay")
		return d.Approved
	}

	// 2. a decision left by the CLI
	if d, ok := g.takeDecision(wf.ID(), callID); ok {
		d.Action = action
		if err := durable.Record(wf, step, d); err != nil {
			return false
		}
		g.emitResolved(wf, action, d.Approved, d.By)
		return d.Approved
	}

	// 3. a human who is actually here
	if approved, answered := g.ask(action); answered {
		d := decision{Approved: approved, CallID: callID, Action: action, By: "interactive"}
		if err := durable.Record(wf, step, d); err != nil {
			return false
		}
		// An earlier park on this workflow is now answered; leaving its request
		// file behind would show up in -list as still waiting on someone.
		_ = os.Remove(pendingPath(g.dir, wf.ID()))
		g.emitResolved(wf, action, approved, "interactive")
		return approved
	}

	// 4. nobody home — park it. Days are fine.
	events.Emit(g.bus, events.Event{
		Type: events.ApprovalRequested, Workflow: wf.ID(), Name: callID, Text: action,
	})
	_ = g.writePending(Pending{Workflow: wf.ID(), CallID: callID, Action: action})
	_ = wf.Finish(durable.StatusSuspended)

	g.mu.Lock()
	g.parked = &durable.Suspended{ID: wf.ID(), Reason: "awaiting approval: " + action}
	g.mu.Unlock()

	events.Emit(g.bus, events.Event{
		Type: events.WorkflowSuspended, Workflow: wf.ID(), Text: action,
	})
	return false
}

// ask puts the question to the interactive front-end. A Decider tells us
// whether a human actually answered; a plain Approver can only say yes or no,
// so a false from one has to be taken at face value as a real refusal.
func (g *Gate) ask(action string) (approved, answered bool) {
	switch h := g.live.(type) {
	case nil:
		return false, false
	case Decider:
		return h.Decide(action)
	default:
		return h.Confirm(action), true
	}
}

func (g *Gate) emitResolved(wf *durable.Workflow, action string, approved bool, by string) {
	events.Emit(g.bus, events.Event{
		Type: events.ApprovalResolved, Workflow: wf.ID(),
		Text: action, Approved: &approved, Agent: by,
	})
}

// stepName keys the checkpoint by the model's own tool call id, so a replay
// looks up the same decision even though the action text was rebuilt from
// scratch. The action hash is folded in as a guard: if a replay somehow reaches
// this call id asking to do something *different*, the names won't match and we
// ask again rather than silently reusing a yes that was given for something
// else.
func stepName(callID, action string) string {
	sum := sha256.Sum256([]byte(action))
	id := callID
	if id == "" {
		id = "anon"
	}
	return "approval-" + id + "-" + hex.EncodeToString(sum[:4])
}

// takeDecision consumes the CLI's answer for this call, if one is waiting.
//
// It refuses a decision recorded against a DIFFERENT tool call, and leaves that
// decision on disk rather than eating it — the run that it does belong to may
// still be coming. That check is what stops a divergent replay from spending a
// human's yes on the wrong action.
//
// A matching decision is deleted after reading: it authorises one gate, and
// leaving it behind would silently approve the next thing the agent asked for.
func (g *Gate) takeDecision(workflowID, callID string) (decision, bool) {
	path := decisionPath(g.dir, workflowID)
	raw, err := os.ReadFile(path)
	if err != nil {
		return decision{}, false
	}

	var d decision
	if err := json.Unmarshal(raw, &d); err != nil {
		_ = os.Remove(path) // undecodable: nothing can ever use it
		return decision{}, false
	}
	if d.CallID != "" && callID != "" && d.CallID != callID {
		return decision{}, false
	}

	_ = os.Remove(path)
	_ = os.Remove(pendingPath(g.dir, workflowID))
	if d.By == "" {
		d.By = "cli"
	}
	return d, true
}

func (g *Gate) writePending(p Pending) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pendingPath(g.dir, p.Workflow), raw, 0o644)
}

func decisionPath(dir, workflowID string) string {
	return filepath.Join(dir, strings.TrimSpace(workflowID)+".json")
}

func pendingPath(dir, workflowID string) string {
	return filepath.Join(dir, strings.TrimSpace(workflowID)+".pending.json")
}

// Waiting reports what a parked workflow is asking a human to decide, so the
// CLI can show the action rather than just an id. Missing is not an error: a
// workflow can be suspended for other reasons, and "nothing pending" is a
// legitimate answer.
func Waiting(dir, workflowID string) (Pending, bool) {
	raw, err := os.ReadFile(pendingPath(dir, workflowID))
	if err != nil {
		return Pending{}, false
	}
	var p Pending
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pending{}, false
	}
	return p, true
}

// Write records a human's verdict for a parked workflow. This is the other side
// of the loop — what the `-approve` CLI calls — and it deliberately does no
// agent work: it drops a file, and the next run's replay does the rest.
//
// The verdict is bound to whichever call the workflow parked on, so it can only
// ever authorise the action the human was actually shown.
func Write(dir, workflowID string, approved bool, by string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("approval: create decisions dir: %w", err)
	}

	d := decision{Approved: approved, By: by}
	if pending, ok := Waiting(dir, workflowID); ok {
		d.CallID, d.Action = pending.CallID, pending.Action
	}

	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(decisionPath(dir, workflowID), raw, 0o644); err != nil {
		return fmt.Errorf("approval: record decision: %w", err)
	}
	return nil
}
