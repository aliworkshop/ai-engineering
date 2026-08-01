package agent

import (
	"context"
	"os"
	"testing"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/session-3/agent/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/session-3/agent/internal/tools"
)

func TestCompactionShrinksHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("live; skipped in -short")
	}
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	ag := New(llm.NewOpenRouter(key), evalModel, tools.Default(approve(false)))
	var compacted bool
	ag.OnCompact = func(string) { compacted = true }

	questions := []string{
		"In one word, capital of France?",
		"In one word, capital of Japan?",
		"In one word, capital of Italy?",
		"In one word, capital of Spain?",
		"In one word, capital of Germany?", // 5th -> triggers compaction
	}
	for i, q := range questions {
		if _, err := ag.Ask(context.Background(), q); err != nil {
			t.Fatalf("ask %d: %v", i+1, err)
		}
		t.Logf("after turn %d: history has %d messages", i+1, len(ag.history))
	}

	if !compacted {
		t.Fatalf("expected compaction to fire on turn 5")
	}
	// Sliding window: system prompt + summary + the last turn kept verbatim
	// (its user question and the assistant's answer) = 4 messages.
	if len(ag.history) != 4 {
		t.Fatalf("expected history compacted to 4 messages (system + summary + last turn), got %d", len(ag.history))
	}
	if ag.history[len(ag.history)-1].ChatAssistantMessage == nil {
		t.Fatalf("expected the last turn's answer to be preserved verbatim")
	}
}
