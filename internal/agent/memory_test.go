package agent

import (
	"strings"
	"testing"
)

// turnOf builds one pass of the loop: a call, and its result of a given size.
func turnOf(id string, resultChars int) Turn {
	return Turn{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "runCode", Args: `{}`}}},
		{Role: "tool", ToolCallID: id, Text: strings.Repeat("x", resultChars)},
	}
}

// Four characters a token, rounded up, and a message that is only tool calls
// still costs something.
func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens([]Msg{{Role: "user", Text: "12345678"}}); got != 2 {
		t.Errorf("8 chars = %d tokens, want 2", got)
	}
	if got := estimateTokens([]Msg{{Role: "user", Text: "123456789"}}); got != 3 {
		t.Errorf("9 chars = %d tokens, want 3", got)
	}
	if got := estimateTokens(Turn{turnOf("call_a", 0)[0]}); got == 0 {
		t.Error("a message that is only a tool call was counted as free")
	}
}

// The goal is pinned: whatever has been compacted, the task is still there,
// verbatim, right after the standing prompt and the conversation before it.
func TestBuildContextPinsTheTask(t *testing.T) {
	seed := []Msg{{Role: "user", Text: "earlier"}, {Role: "assistant", Text: "ok"}}
	turns := []Turn{turnOf("call_a", 4)}

	got := buildContext(seed, "find the duplicate", "classified cus_88121 as billing", turns)

	roles := make([]string, len(got))
	for i, m := range got {
		roles[i] = m.Role
	}
	want := "system user assistant user system assistant tool"
	if strings.Join(roles, " ") != want {
		t.Fatalf("roles = %v, want %s", roles, want)
	}
	if got[0].Text != SystemPrompt {
		t.Error("the standing prompt is not first")
	}
	if got[3].Text != "find the duplicate" {
		t.Errorf("task = %q, want it verbatim", got[3].Text)
	}
	if !strings.Contains(got[4].Text, "classified cus_88121 as billing") {
		t.Errorf("summary message = %q", got[4].Text)
	}
}

// No summary yet, no summary message: an empty "Summary of earlier work" is a
// line the model would try to make sense of.
func TestBuildContextLeavesOutAnEmptySummary(t *testing.T) {
	got := buildContext(nil, "task", "", nil)
	if len(got) != 2 {
		t.Errorf("got %d messages, want the prompt and the task: %+v", len(got), got)
	}
}

// Old turns go whole and oldest first, until what is left fits the keep
// budget — and every tool result stays behind the call that asked for it.
func TestPeelCutsWholeTurnsOldestFirst(t *testing.T) {
	turns := []Turn{turnOf("call_a", 400), turnOf("call_b", 400), turnOf("call_c", 400)}

	old, kept := peel(turns)

	if len(old)+len(kept) != len(turns) {
		t.Fatalf("peel lost turns: %d old + %d kept, want %d", len(old), len(kept), len(turns))
	}
	if len(old) == 0 || old[0][0].ToolCalls[0].ID != "call_a" {
		t.Errorf("the oldest turn was not the first to go: %+v", old)
	}
	if estimateTokens(flatten(kept)) > keepContextTokens {
		t.Errorf("kept %d tokens, want at most %d", estimateTokens(flatten(kept)), keepContextTokens)
	}
	for _, turn := range kept {
		if turn[0].ToolCalls[0].ID != turn[1].ToolCallID {
			t.Errorf("a turn was split: %+v", turn)
		}
	}
}

// The newest turn holds results the model has not read yet, so it stays even
// when it alone is over budget.
func TestPeelKeepsTheNewestTurn(t *testing.T) {
	turns := []Turn{turnOf("call_a", 10), turnOf("call_b", 5000)}

	old, kept := peel(turns)

	if len(kept) != 1 || kept[0][0].ToolCalls[0].ID != "call_b" {
		t.Errorf("kept %+v, want only the newest turn", kept)
	}
	if len(old) != 1 {
		t.Errorf("summarized %d turns, want 1", len(old))
	}
}
