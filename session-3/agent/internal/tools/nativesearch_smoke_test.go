package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	openrouter "github.com/OpenRouterTeam/go-sdk"
)

// TestNativeWebSearchLive hits the real OpenRouter web plugin. It costs credits,
// so it skips under -short and without a key — same deal as the agent evals.
func TestNativeWebSearchLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live; skipped in -short")
	}
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	tool := NativeWebSearch{
		Client: openrouter.New(openrouter.WithSecurity(key)),
		Model:  "openai/gpt-4o-mini",
	}
	out, err := tool.Run(context.Background(), `{"query":"who won the most recent FIFA World Cup"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("result:\n%s", out)

	if strings.TrimSpace(out) == "" || out == "No results." {
		t.Fatalf("expected a summarized answer, got %q", out)
	}
	// The plugin's whole point is grounding, so an answer with no link back to a
	// source means the search half of the round trip didn't happen.
	if !strings.Contains(out, "http") {
		t.Fatalf("expected at least one source URL in the answer, got:\n%s", out)
	}
}

func TestNativeWebSearchRejectsEmptyQuery(t *testing.T) {
	tool := NativeWebSearch{Client: openrouter.New(), Model: "openai/gpt-4o-mini"}
	if _, err := tool.Run(context.Background(), `{"query":"   "}`); err == nil {
		t.Fatal("expected an error for a blank query")
	}
}
