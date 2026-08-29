package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// approve is a stub human: it says yes or no to every approval request.
type approve bool

func (a approve) Confirm(string) bool { return bool(a) }

// refuseToBeAsked fails the test if any tool requests approval — used to prove
// read-only tools never reach the human-in-the-loop gate.
type refuseToBeAsked struct{ t *testing.T }

func (r refuseToBeAsked) Confirm(action string) bool {
	r.t.Fatalf("a read-only tool asked for approval: %s", action)
	return false
}

// gated stands in for a tool that changes the world. The toolset ships without
// one at the moment, and what these tests are about is the gate rather than any
// particular tool behind it: it declares itself Sensitive, it asks the Approver
// first, and a "no" means nothing happened.
type gated struct {
	Approver Approver
	ran      *bool
}

func (gated) Sensitive() bool { return true }

func (gated) Spec() components.ChatFunctionTool {
	return defineTool("change_thing", "Change something outside the agent. Requires human approval.",
		`{"type":"object","properties":{"what":{"type":"string"}},"required":["what"]}`)
}

func (t gated) Run(_ context.Context, args string) (string, error) {
	var a struct {
		What string `json:"what"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if !t.Approver.Confirm("Change " + a.What + "?") {
		return "Denied by the human.", nil
	}
	*t.ran = true
	return "Changed " + a.What + ".", nil
}

// harmless is a read-only tool that needs neither network nor approval, so a
// test can dispatch something and watch the gate stay out of the way.
type harmless struct{}

func (harmless) Spec() components.ChatFunctionTool {
	return defineTool("say_hello", "Return a greeting.", `{"type":"object","properties":{}}`)
}

func (harmless) Run(context.Context, string) (string, error) { return "hello", nil }

// jsonArgs builds a tool's JSON argument string the way the model would.
func jsonArgs(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

// A gated tool acts only after the human says yes.
func TestGatedToolActsOnlyOnApproval(t *testing.T) {
	var ran bool
	reg := Default(approve(true), WithExtra(gated{Approver: approve(true), ran: &ran}))

	if got := reg.Dispatch(context.Background(), "change_thing", jsonArgs(t, map[string]any{
		"what": "the config",
	})); !strings.Contains(got, "Changed") {
		t.Fatalf("change_thing: %q", got)
	}
	if !ran {
		t.Fatal("the tool reported success without doing anything")
	}
}

// Requirement 5: a "no" at the human-in-the-loop prompt blocks the action.
func TestHumanInLoopDenies(t *testing.T) {
	var ran bool
	reg := Default(approve(false), WithExtra(gated{Approver: approve(false), ran: &ran}))

	if got := reg.Dispatch(context.Background(), "change_thing", jsonArgs(t, map[string]any{
		"what": "the config",
	})); !strings.Contains(got, "Denied") {
		t.Fatalf("expected denial, got %q", got)
	}
	if ran {
		t.Fatal("the action happened despite the denial")
	}
}

// Dispatch adds no gate of its own: a tool that isn't sensitive never reaches
// the human, however dangerous the surrounding toolset is.
func TestReadOnlyToolsNeedNoApproval(t *testing.T) {
	reg := Default(refuseToBeAsked{t}, WithExtra(harmless{}))

	if got := reg.Dispatch(context.Background(), "say_hello", "{}"); got != "hello" {
		t.Fatalf("say_hello got %q", got)
	}
}

func TestUnknownToolIsHandled(t *testing.T) {
	reg := Default(approve(true))
	if got := reg.Dispatch(context.Background(), "no_such_tool", "{}"); !strings.Contains(got, "unknown tool") {
		t.Fatalf("got %q", got)
	}
}
