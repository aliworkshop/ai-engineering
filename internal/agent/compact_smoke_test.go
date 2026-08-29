package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// TestCompactionCapsContext is the Part 4 claim, measured: tokens sent per turn
// stop growing.
//
// Note what it no longer asserts. The old version counted messages after a
// fixed number of turns, because compaction used to fire every N questions and
// rewrite the history in place. Both of those are gone: history is now kept
// whole, compaction is triggered by the assembled context outgrowing a token
// budget, and what shrinks is the CONTEXT — which is the only one of the three
// the model ever sees, and so the only one whose size is a cost.
//
// The budgets are set absurdly low so a handful of one-word answers is enough
// to trip them; production leaves them near the model's real context window.
func TestCompactionCapsContext(t *testing.T) {
	if testing.Short() {
		t.Skip("live; skipped in -short")
	}
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	ag := New(llm.NewOpenRouter(key), evalModel, tools.Default(approve(false)))
	ag.Memory().MaxContextTokens = 230
	ag.Memory().KeepContextTokens = 215

	var compacted bool
	ag.OnCompact = func(string) { compacted = true }

	questions := []string{
		"In one word, capital of France?",
		"In one word, capital of Japan?",
		"In one word, capital of Italy?",
		"In one word, capital of Spain?",
		"In one word, capital of Germany?",
	}

	peak := 0
	for i, q := range questions {
		if _, err := ag.Ask(context.Background(), q); err != nil {
			t.Fatalf("ask %d: %v", i+1, err)
		}
		size := EstimateTokens(ag.Memory().Context(nil))
		if size > peak {
			peak = size
		}
		t.Logf("after turn %d: context ≈%d tokens, %d turns verbatim, summary %d chars",
			i+1, size, len(ag.Memory().Turns), len(ag.Memory().Summary))
	}

	if !compacted {
		t.Fatalf("expected compaction to fire once the context passed its budget")
	}
	if ag.Memory().Summary == "" {
		t.Fatalf("expected older turns to survive as a summary, not to be dropped")
	}
	if peak > ag.Memory().MaxContextTokens*2 {
		t.Fatalf("context peaked at ≈%d tokens against a %d budget — it is not being capped",
			peak, ag.Memory().MaxContextTokens)
	}

	// The most recent turn is never summarized: a follow-up question nearly
	// always refers to it, and a summary blurs exactly what it refers to.
	last := ag.Memory().Turns[len(ag.Memory().Turns)-1]
	if len(last.Msgs) == 0 || last.Msgs[0].Role != "user" ||
		!strings.Contains(last.Msgs[0].Text, "Germany") {
		t.Fatalf("expected the last question to be kept verbatim, got %+v", last.Msgs)
	}
}
