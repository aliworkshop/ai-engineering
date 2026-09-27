package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
)

// stored builds a step the way the store would have written it.
func stored(t *testing.T, name string, output any) durable.StepInfo {
	t.Helper()
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return durable.StepInfo{Name: name, Output: raw}
}

// A recovered conversation has to come back in the order it happened, with
// every tool result sitting behind the turn that asked for it.
func TestTranscriptPairsToolCallsWithTheirResults(t *testing.T) {
	asked := Msg{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "call_a", Name: "draftReply", Args: `{"item_id":"item-1"}`},
		{ID: "call_b", Name: "sendReply", Args: `{"item_id":"item-1"}`},
	}}
	answered := Msg{Role: "assistant", Text: "done"}

	got := transcript([]durable.StepInfo{
		stored(t, "seed", []Msg(nil)),
		stored(t, "model-00", asked),
		stored(t, "tool-call_a", `{"ok":true}`),
		stored(t, "tool-call_b", `{"sent":true}`),
		stored(t, "model-01", answered),
	})

	want := []Msg{
		asked,
		{Role: "tool", Text: `{"ok":true}`, ToolCallID: "call_a"},
		{Role: "tool", Text: `{"sent":true}`, ToolCallID: "call_b"},
		answered,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transcript() =\n%+v\nwant\n%+v", got, want)
	}
}

// The rule that stops a recovered conversation from being unusable: a turn
// that asked for a tool nothing answered is dropped whole. Half a pair is a
// conversation the next model call refuses outright — so losing a turn beats
// keeping a broken one.
func TestTranscriptDropsAnUnansweredTurn(t *testing.T) {
	crashed := Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_gone", Name: "sendReply"}}}
	good := Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_a", Name: "draftReply"}}}

	got := transcript([]durable.StepInfo{
		stored(t, "model-00", good),
		stored(t, "tool-call_a", `{"ok":true}`),
		// The process died between asking and recording the result.
		stored(t, "model-01", crashed),
	})

	if len(got) != 2 {
		t.Fatalf("transcript() kept %d messages, want the answered pair only: %+v", len(got), got)
	}
	for _, msg := range got {
		if msg.ToolCallID == "call_gone" || len(msg.ToolCalls) == 1 && msg.ToolCalls[0].ID == "call_gone" {
			t.Errorf("the unanswered turn survived: %+v", msg)
		}
	}
}

// A run that was itself a follow-up carries its own seed, so continuing it
// gives you the whole thread rather than its last exchange.
func TestStoredSeedIsTheConversationTheRunStartedFrom(t *testing.T) {
	seed := []Msg{
		{Role: "user", Text: "handle item-1"},
		{Role: "assistant", Text: "sent"},
	}

	got := storedSeed([]durable.StepInfo{
		stored(t, "seed", seed),
		stored(t, "model-00", Msg{Role: "assistant", Text: "hello"}),
	})
	if !reflect.DeepEqual(got, seed) {
		t.Errorf("storedSeed() = %+v, want %+v", got, seed)
	}

	if got := storedSeed([]durable.StepInfo{stored(t, "model-00", Msg{})}); got != nil {
		t.Errorf("storedSeed() = %+v for a first turn, want nil", got)
	}
}
