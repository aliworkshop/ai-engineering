package agent

// End-to-end tests for the harness, run against a fake model.
//
// The eval suite in this package measures whether the MODEL behaves; these
// measure whether the RUNTIME does — which is a different question and, unlike
// the first, has exact answers. A scripted model server makes the loop fully
// deterministic, so "a crash never re-sends" stops being a claim in a comment
// and becomes an assertion: the tool ran once across two runs, and the count is
// the test.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/toolspec"
)

// --- a scripted model ---------------------------------------------------------

// scriptedModel answers each request with the next canned reply. It counts its
// own calls, which is how a replay proves it made no new ones.
type scriptedModel struct {
	mu      sync.Mutex
	replies []string // raw JSON message objects
	calls   int
}

func (m *scriptedModel) next() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	if i >= len(m.replies) {
		return `{"role":"assistant","content":"(script exhausted)"}`
	}
	return m.replies[i]
}

func (m *scriptedModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// toolCall builds the assistant message JSON for one tool request.
func toolCall(id, name, args string) string {
	encoded, _ := json.Marshal(args)
	return fmt.Sprintf(
		`{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}`,
		id, name, encoded)
}

func finalAnswer(text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"role":"assistant","content":%s}`, encoded)
}

// fakeClient points the real SDK at a local server, so everything under test —
// request building, the union types, the loop — is the production path.
func fakeClient(t *testing.T, model *scriptedModel) *openrouter.OpenRouter {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"gen-1","model":"fake","object":"chat.completion","created":0,
			"choices":[{"index":0,"finish_reason":"stop","message":%s}]}`, model.next())
	}))
	t.Cleanup(server.Close)

	return openrouter.New(
		openrouter.WithSecurity("test-key"),
		openrouter.WithServerURL(server.URL),
	)
}

// --- a counting toolbox -------------------------------------------------------

// countingTools records every dispatch. The counts are the assertions: a tool
// that ran twice across a crash is the exact failure durable execution exists
// to prevent.
type countingTools struct {
	mu    sync.Mutex
	calls []string
	gate  interface{ Confirm(string) bool } // nil unless a tool is gated
	risky map[string]bool

	// afterCall is the crash injector: it fires once the tool has done its
	// work, which is the only interesting moment to die — the side effect has
	// happened and nothing has been written down yet.
	afterCall func(name string)
}

func (c *countingTools) Specs() []components.ChatFunctionTool {
	return []components.ChatFunctionTool{
		toolspec.Define("send_email", "Send an email. Irreversible.",
			`{"type":"object","properties":{"to":{"type":"string"}},"required":["to"]}`),
		toolspec.Define("look_up", "Look something up.",
			`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
}

func (c *countingTools) Dispatch(_ context.Context, name, args string) string {
	if c.risky[name] && c.gate != nil {
		if !c.gate.Confirm(name + " " + args) {
			return "Denied by a human; not done."
		}
	}
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()

	if c.afterCall != nil {
		c.afterCall(name)
	}
	return `{"ok":true,"tool":"` + name + `"}`
}

func (c *countingTools) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, called := range c.calls {
		if called == name {
			n++
		}
	}
	return n
}

func store(t *testing.T, bus events.Emitter) *durable.Store {
	t.Helper()
	s, err := durable.NewStore(filepath.Join(t.TempDir(), "wf"), bus)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return s
}

// --- Part 2 -------------------------------------------------------------------

// TestCrashDoesNotResendTheEmail is the emotional centre of the week, as a
// test: the process dies after an irreversible side effect, and resuming does
// not repeat it.
func TestCrashDoesNotResendTheEmail(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_1", "send_email", `{"to":"customer@example.com"}`),
		finalAnswer("emailed the customer"),
	}}
	toolbox := &countingTools{}
	st := store(t, nil)
	client := fakeClient(t, model)

	// The crash: the moment the email is "sent", the process goes away. The
	// side effect has happened; nothing about it is on disk yet.
	crashed, crash := context.WithCancel(context.Background())
	toolbox.afterCall = func(string) { crash() }

	first := New(client, "fake", toolbox).WithStore(st)
	_, workflowID, err := first.ask(crashed, "email the customer", "")
	if err == nil {
		t.Fatalf("expected the crashed run to stop without an answer")
	}
	if toolbox.count("send_email") != 1 {
		t.Fatalf("the email should have been sent once, was sent %d times", toolbox.count("send_email"))
	}
	callsBeforeCrash := model.count()

	// Restart: a brand new agent — no memory, no shared state — resumes the id.
	toolbox.afterCall = nil
	second := New(client, "fake", toolbox).WithStore(st)
	answer, err := second.Resume(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if got := toolbox.count("send_email"); got != 1 {
		t.Fatalf("the customer was emailed %d times; replay must never repeat a side effect", got)
	}
	if answer != "emailed the customer" {
		t.Fatalf("resumed run returned %q", answer)
	}
	// Exactly one NEW model call: the first turn was served from disk, not
	// re-billed.
	if got := model.count() - callsBeforeCrash; got != 1 {
		t.Fatalf("resume made %d model calls; the replayed turn should have been free", got)
	}
}

// TestWithoutAStoreItIsBrittle documents the other half honestly: the same loop
// with no store repeats everything, which is exactly the Part 1 agent.
func TestWithoutAStoreItIsBrittle(t *testing.T) {
	toolbox := &countingTools{}
	model := &scriptedModel{replies: []string{
		toolCall("call_1", "send_email", `{"to":"customer@example.com"}`),
		finalAnswer("done"),
		toolCall("call_1", "send_email", `{"to":"customer@example.com"}`),
		finalAnswer("done"),
	}}
	client := fakeClient(t, model)

	ag := New(client, "fake", toolbox)
	if _, err := ag.Ask(context.Background(), "email the customer"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if _, err := ag.Ask(context.Background(), "email the customer"); err != nil {
		t.Fatalf("ask again: %v", err)
	}
	if got := toolbox.count("send_email"); got != 2 {
		t.Fatalf("expected the undurable loop to send twice, got %d", got)
	}
}

// --- Part 7 -------------------------------------------------------------------

// absentHuman never answers, which is the case the durable gate exists for.
type absentHuman struct{ asked int }

func (h *absentHuman) Confirm(string) bool        { return false }
func (h *absentHuman) Decide(string) (bool, bool) { h.asked++; return false, false }

// TestParkedApprovalResumesWithoutRepeatingWork walks the full Part 7 loop
// through the real agent: a gate with nobody to answer parks the run, the tool
// never fires, and a later approval resumes onto the same call.
func TestParkedApprovalResumesWithoutRepeatingWork(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_look", "look_up", `{"q":"the charge"}`),
		toolCall("call_send", "send_email", `{"to":"customer@example.com"}`),
		finalAnswer("refunded and emailed"),
	}}
	client := fakeClient(t, model)
	st := store(t, nil)

	human := &absentHuman{}
	gate := &testGate{live: human, decisions: map[string]bool{}}
	toolbox := &countingTools{gate: gate, risky: map[string]bool{"send_email": true}}

	ag := New(client, "fake", toolbox).WithStore(st).WithGate(gate)
	_, workflowID, err := ag.ask(context.Background(), "refund and tell them", "")

	parked, ok := durable.IsSuspended(err)
	if !ok {
		t.Fatalf("expected the run to park, got %v", err)
	}
	if parked.ID != workflowID {
		t.Fatalf("park names workflow %q, run was %q", parked.ID, workflowID)
	}
	if toolbox.count("send_email") != 0 {
		t.Fatalf("the gated tool ran before anyone approved it")
	}
	if toolbox.count("look_up") != 1 {
		t.Fatalf("the ungated work should have completed once, got %d", toolbox.count("look_up"))
	}
	modelCallsBefore := model.count()

	// Days pass. A human approves. A fresh agent resumes.
	gate.decisions[workflowID] = true
	resumed := New(client, "fake", toolbox).WithStore(st).WithGate(gate)

	answer, err := resumed.Resume(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if answer != "refunded and emailed" {
		t.Fatalf("resumed run answered %q", answer)
	}
	if got := toolbox.count("send_email"); got != 1 {
		t.Fatalf("the gated tool ran %d times after one approval", got)
	}
	if got := toolbox.count("look_up"); got != 1 {
		t.Fatalf("replay re-ran the earlier tool %d times", got)
	}
	// Only the final answer needed a new model call; both earlier turns replayed.
	if got := model.count() - modelCallsBefore; got != 1 {
		t.Fatalf("resume made %d model calls, expected 1", got)
	}
}

// testGate is the Gate the agent expects, backed by an in-memory decision map
// so this test does not depend on the approval package's file layout.
type testGate struct {
	live      interface{ Decide(string) (bool, bool) }
	decisions map[string]bool
	wf        *durable.Workflow
	callID    string
	parked    error
}

func (g *testGate) Attach(wf *durable.Workflow) { g.wf, g.parked, g.callID = wf, nil, "" }
func (g *testGate) Scope(callID string)         { g.callID = callID }
func (g *testGate) Parked() error               { return g.parked }

func (g *testGate) Confirm(action string) bool {
	step := "approval-" + g.callID
	if d, cached, _ := durable.Lookup[bool](g.wf, step); cached {
		return d
	}
	if approved, ok := g.decisions[g.wf.ID()]; ok {
		_ = durable.Record(g.wf, step, approved)
		return approved
	}
	if approved, answered := g.live.Decide(action); answered {
		_ = durable.Record(g.wf, step, approved)
		return approved
	}
	_ = g.wf.Finish(durable.StatusSuspended)
	g.parked = &durable.Suspended{ID: g.wf.ID(), Reason: "awaiting approval: " + action}
	return false
}

// --- Part 5 -------------------------------------------------------------------

// TestHandoffSwitchesToolsetAndSurvivesACrash checks both halves of a transfer:
// the specialist's tools take over, and the switch is on disk rather than only
// in memory.
func TestHandoffSwitchesToolsetAndSurvivesACrash(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_h", "handoff", `{"to":"operator","reason":"needs a write"}`),
		finalAnswer("handed over and done"),
	}}
	client := fakeClient(t, model)
	st := store(t, nil)

	assistantTools := &handoffTools{}
	operatorTools := &countingTools{}
	roster := Roster{
		AssistantName: {Name: AssistantName, Prompt: "generalist", Tools: assistantTools},
		OperatorName:  {Name: OperatorName, Prompt: "specialist", Tools: operatorTools},
	}

	ag := New(client, "fake", assistantTools).WithRoster(roster, AssistantName).WithStore(st)
	_, workflowID, err := ag.ask(context.Background(), "write the file", "")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	if ag.Current() != OperatorName {
		t.Fatalf("after a handoff the current agent is %q", ag.Current())
	}
	if ag.mem.System != "specialist" {
		t.Fatalf("the specialist's prompt did not take effect: %q", ag.mem.System)
	}

	// A fresh agent reopening the workflow must come back as the operator, not
	// bounce to the generalist and start the argument again.
	reopened := New(client, "fake", assistantTools).WithRoster(roster, AssistantName).WithStore(st)
	if _, err := reopened.Resume(context.Background(), workflowID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if reopened.Current() != OperatorName {
		t.Fatalf("the handoff did not survive the crash: current is %q", reopened.Current())
	}
}

// handoffTools advertises the one tool the generalist needs for this test.
type handoffTools struct{}

func (handoffTools) Specs() []components.ChatFunctionTool {
	return []components.ChatFunctionTool{
		toolspec.Define("handoff", "Hand over to a specialist.",
			`{"type":"object","properties":{"to":{"type":"string"},"reason":{"type":"string"}},"required":["to","reason"]}`),
	}
}

func (handoffTools) Dispatch(_ context.Context, name, args string) string {
	if name != "handoff" {
		return "error: unknown tool " + name
	}
	var a struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	out, _ := json.Marshal(handoffMarker{Handoff: true, To: a.To, Reason: a.Reason})
	return string(out)
}

// TestHandoffToAnUnknownAgentIsIgnored: the roster is the authority, not the
// model. A hallucinated agent name must not silently swap the toolset away.
func TestHandoffToAnUnknownAgentIsIgnored(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_h", "handoff", `{"to":"finance","reason":"not a real agent"}`),
		finalAnswer("did it myself"),
	}}
	client := fakeClient(t, model)

	assistantTools := &handoffTools{}
	roster := Roster{
		AssistantName: {Name: AssistantName, Prompt: "generalist", Tools: assistantTools},
		OperatorName:  {Name: OperatorName, Prompt: "specialist", Tools: &countingTools{}},
	}

	ag := New(client, "fake", assistantTools).WithRoster(roster, AssistantName)
	if _, err := ag.Ask(context.Background(), "do a thing"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if ag.Current() != AssistantName {
		t.Fatalf("an unknown handoff target changed the agent to %q", ag.Current())
	}
}

// --- Part 1 -------------------------------------------------------------------

// TestTheRunIsFullyDescribedByItsEvents is the events-first payoff: a reader
// with only the stream can reconstruct what happened.
func TestTheRunIsFullyDescribedByItsEvents(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_1", "look_up", `{"q":"x"}`),
		finalAnswer("found it"),
	}}
	client := fakeClient(t, model)

	var seen []events.Event
	var mu sync.Mutex
	bus := events.EmitterFunc(func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, e)
	})

	ag := New(client, "fake", &countingTools{}).WithStore(store(t, bus)).WithEvents(bus)
	if _, err := ag.Ask(context.Background(), "look it up"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	var types []string
	for _, e := range seen {
		types = append(types, string(e.Type))
	}
	stream := strings.Join(types, " ")

	for _, want := range []string{
		string(events.WorkflowStarted),
		string(events.ToolRequested),
		string(events.ToolCompleted),
		string(events.ModelCompleted),
		string(events.WorkflowCompleted),
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("the stream is missing %s:\n%s", want, stream)
		}
	}
}

// TestHandoffThenParkResumesOntoTheSameCall is a regression test for a bug that
// only appears when Part 5 and Part 7 meet.
//
// The run hands off, then the specialist's first action needs approval and
// parks. On resume, the replay MUST land on that same tool call.
//
// It originally did not, and the cause took two individually reasonable
// decisions to produce: model-turn checkpoints were keyed by agent name as well
// as position, AND resume restored the recorded agent before replaying. Either
// alone is harmless. Together, the replay started as the operator, so every
// cached turn — recorded when the assistant was speaking — missed. It made a
// fresh model call, got a fresh tool call id, spent the human's approval on it,
// performed the action, then hit the ORIGINAL cached call and asked to be
// approved all over again. One yes, the action done once and asked twice.
//
// That is why this test drives both parts together: the combination is the bug,
// and a test of either half in isolation would have passed.
func TestHandoffThenParkResumesOntoTheSameCall(t *testing.T) {
	model := &scriptedModel{replies: []string{
		toolCall("call_handoff", "handoff", `{"to":"operator","reason":"needs a write"}`),
		toolCall("call_write", "send_email", `{"to":"customer@example.com"}`),
		finalAnswer("done"),
	}}
	client := fakeClient(t, model)
	st := store(t, nil)

	human := &absentHuman{}
	gate := &testGate{live: human, decisions: map[string]bool{}}
	operatorTools := &countingTools{gate: gate, risky: map[string]bool{"send_email": true}}
	assistantTools := &handoffTools{}

	roster := Roster{
		AssistantName: {Name: AssistantName, Prompt: "generalist", Tools: assistantTools},
		OperatorName:  {Name: OperatorName, Prompt: "specialist", Tools: operatorTools},
	}

	ag := New(client, "fake", assistantTools).WithRoster(roster, AssistantName).
		WithStore(st).WithGate(gate)

	_, workflowID, err := ag.ask(context.Background(), "email the customer", "")
	if _, ok := durable.IsSuspended(err); !ok {
		t.Fatalf("expected the specialist's action to park, got %v", err)
	}
	if operatorTools.count("send_email") != 0 {
		t.Fatalf("the gated action ran before approval")
	}
	modelCallsBeforeApproval := model.count()

	// The human approves. A fresh process resumes.
	gate.decisions[workflowID] = true
	resumed := New(client, "fake", assistantTools).WithRoster(roster, AssistantName).
		WithStore(st).WithGate(gate)

	answer, err := resumed.Resume(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("resume after approval: %v", err)
	}
	if answer != "done" {
		t.Fatalf("resumed run answered %q", answer)
	}
	if got := operatorTools.count("send_email"); got != 1 {
		t.Fatalf("the approved action ran %d times, want exactly 1", got)
	}
	if resumed.Current() != OperatorName {
		t.Fatalf("replay did not re-derive the handoff: current is %q", resumed.Current())
	}
	// Both earlier turns replayed from cache; only the final answer is new.
	if got := model.count() - modelCallsBeforeApproval; got != 1 {
		t.Fatalf("resume made %d model calls, expected 1 — the replay is diverging", got)
	}
	if human.asked > 1 {
		t.Fatalf("the human was asked %d times for one action", human.asked)
	}
}
