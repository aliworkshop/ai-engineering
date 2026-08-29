package agent

import (
	"strings"
	"testing"
)

func turn(question, answer string) []Msg {
	return []Msg{
		{Role: "user", Text: question},
		{Role: "assistant", Text: answer},
	}
}

// TestContextIsAssembledNotAccumulated is the Part 4 distinction, made
// concrete: the model sees a construction, not the log.
func TestContextIsAssembledNotAccumulated(t *testing.T) {
	mem := NewMemory("you are a helpful agent")
	mem.Commit(turn("what is 2+2?", "4"))
	mem.Summary = "earlier: introduced ourselves"

	ctx := mem.Context([]Msg{{Role: "user", Text: "and 3+3?"}})

	if ctx[0].Role != "system" || ctx[0].Text != "you are a helpful agent" {
		t.Fatalf("standing instructions must come first, got %+v", ctx[0])
	}
	if ctx[1].Role != "system" || !strings.Contains(ctx[1].Text, "introduced ourselves") {
		t.Fatalf("the summary should follow the system prompt, got %+v", ctx[1])
	}
	if last := ctx[len(ctx)-1]; last.Text != "and 3+3?" {
		t.Fatalf("the live question must come last, got %+v", last)
	}

	// Assembling again with different working messages must not have mutated
	// anything — context is built per turn, not appended to.
	again := mem.Context(nil)
	if len(again) != 4 { // system + summary + the turn's two messages
		t.Fatalf("expected system + summary + one 2-message turn, got %d", len(again))
	}
	if again[len(again)-1].Text == "and 3+3?" {
		t.Fatalf("the live question leaked into memory; working messages are not committed")
	}
}

// TestCompactionCutsOnTurnBoundaries guards the invariant that keeps the API
// from rejecting the next request: a tool result may never be separated from
// the assistant message that called for it.
func TestCompactionCutsOnTurnBoundaries(t *testing.T) {
	mem := NewMemory("system")
	mem.MaxContextTokens = 60
	mem.KeepContextTokens = 30

	for i := 0; i < 6; i++ {
		mem.Commit([]Msg{
			{Role: "user", Text: strings.Repeat("question ", 5)},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_x", Name: "get_weather", Args: `{"location":"Oslo"}`}}},
			{Role: "tool", Text: strings.Repeat("result ", 5), ToolCallID: "call_x"},
			{Role: "assistant", Text: "done"},
		})
	}

	if !mem.Overflowing() {
		t.Fatalf("six padded turns should have overflowed a 60-token budget")
	}
	old := mem.Overflow()
	if len(old) == 0 {
		t.Fatalf("expected overflow to hand back the oldest turns")
	}

	// Every turn that came out is whole, and every one left behind is too.
	for _, group := range [][]Turn{old, mem.Turns} {
		for _, tr := range group {
			var pendingCall bool
			for _, m := range tr.Msgs {
				if len(m.ToolCalls) > 0 {
					pendingCall = true
				}
				if m.Role == "tool" {
					if !pendingCall {
						t.Fatalf("a tool result was cut away from its call: %+v", tr.Msgs)
					}
					pendingCall = false
				}
			}
			if pendingCall {
				t.Fatalf("a tool call was cut away from its result: %+v", tr.Msgs)
			}
		}
	}
}

// TestTheMostRecentTurnIsNeverCompacted: a follow-up question refers to the
// last turn, and a summary blurs exactly what it refers to.
func TestTheMostRecentTurnIsNeverCompacted(t *testing.T) {
	mem := NewMemory("system")
	mem.MaxContextTokens = 10
	mem.KeepContextTokens = 5

	for i := 0; i < 4; i++ {
		mem.Commit(turn(strings.Repeat("padding ", 20), "ok"))
	}
	mem.Commit(turn("edit /tmp/report.md line 12", "edited"))

	mem.Overflow()

	if len(mem.Turns) < 1 {
		t.Fatalf("compaction emptied memory; at least the last turn must survive")
	}
	last := mem.Turns[len(mem.Turns)-1]
	if !strings.Contains(last.Msgs[0].Text, "/tmp/report.md") {
		t.Fatalf("the most recent turn was compacted away: %+v", last.Msgs)
	}
}

// TestSingleTurnIsNeverCompacted covers the degenerate case: one enormous turn
// has nothing older to fold, and compacting it would be self-erasure.
func TestSingleTurnIsNeverCompacted(t *testing.T) {
	mem := NewMemory("system")
	mem.MaxContextTokens = 1
	mem.KeepContextTokens = 1
	mem.Commit(turn(strings.Repeat("huge ", 500), "ok"))

	if got := mem.Overflow(); got != nil {
		t.Fatalf("expected no turns to be handed off, got %d", len(got))
	}
	if len(mem.Turns) != 1 {
		t.Fatalf("the only turn was dropped")
	}
}

// TestTranscriptKeepsToolWork checks that what goes to the summarizer includes
// the tool calls and results — a summary that drops them is how an agent
// forgets the number it just computed.
func TestTranscriptKeepsToolWork(t *testing.T) {
	text := Transcript([]Turn{{Msgs: []Msg{
		{Role: "user", Text: "sum the primes below 1000"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "run_code", Args: `{"language":"python"}`}}},
		{Role: "tool", Text: "76127"},
		{Role: "assistant", Text: "done"},
	}}})

	for _, want := range []string{"sum the primes", "run_code", "python", "76127", "done"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript dropped %q:\n%s", want, text)
		}
	}
}

// TestSDKRoundTripPreservesToolCallIDs matters beyond bookkeeping: the call id
// is the key a human's approval is checkpointed under, so losing it in
// translation would break replay.
func TestSDKRoundTripPreservesToolCallIDs(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Text: "sys"},
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "one moment", ToolCalls: []ToolCall{
			{ID: "call_9", Name: "get_weather", Args: `{"location":"Tokyo"}`},
		}},
		{Role: "tool", Text: "18.2°C", ToolCallID: "call_9"},
	}

	sdk := toSDK(msgs)
	if len(sdk) != 4 {
		t.Fatalf("expected 4 SDK messages, got %d", len(sdk))
	}

	assistant := sdk[2].ChatAssistantMessage
	if assistant == nil || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls did not survive: %+v", sdk[2])
	}
	if assistant.ToolCalls[0].ID != "call_9" || assistant.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool call mangled: %+v", assistant.ToolCalls[0])
	}
	if assistantText(*assistant) != "one moment" {
		t.Fatalf("assistant text lost: %q", assistantText(*assistant))
	}
	if sdk[3].ChatToolMessage == nil || sdk[3].ChatToolMessage.ToolCallID != "call_9" {
		t.Fatalf("tool result lost its call id: %+v", sdk[3])
	}
}

// TestDecodeHandoff makes sure the loop only reacts to a real transfer marker.
func TestDecodeHandoff(t *testing.T) {
	if h, ok := decodeHandoff(`{"handoff":true,"to":"operator","reason":"needs a file write"}`); !ok || h.To != "operator" {
		t.Fatalf("a valid handoff was not recognized: %+v ok=%v", h, ok)
	}
	for _, notAHandoff := range []string{
		`{"ok":true}`,
		`{"handoff":false,"to":"operator"}`,
		`{"handoff":true}`,
		`Wrote cfg.json`,
		``,
	} {
		if _, ok := decodeHandoff(notAHandoff); ok {
			t.Fatalf("%q was mistaken for a handoff", notAHandoff)
		}
	}
}

// TestEstimateTokensGrowsWithContent is a sanity check on the budget's only
// input — an estimator that ignored tool arguments would never see a prompt
// balloon from a big run_code call.
func TestEstimateTokensGrowsWithContent(t *testing.T) {
	small := EstimateTokens([]Msg{{Role: "user", Text: "hi"}})
	big := EstimateTokens([]Msg{{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "c", Name: "run_code", Args: strings.Repeat("x", 4000)},
	}}})
	if big <= small {
		t.Fatalf("tool arguments must count toward the budget: small=%d big=%d", small, big)
	}
}
