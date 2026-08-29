package agent

// Behavioral eval harness.
//
// Where the tools package unit-tests each tool deterministically, this runs
// whole tasks through the REAL agent loop and grades the outcome: did it answer
// without a tool when it should, reach for openrouter_web_search when it needed
// facts, call get_weather rather than guessing, and actually compute a number
// in the sandbox instead of inventing one?
//
// Run:  go test ./internal/agent -run Eval -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

const evalModel = "openai/gpt-4o-mini"

// approve is a stub human answering yes/no to every approval request.
type approve bool

func (a approve) Confirm(string) bool { return bool(a) }

// scenario is one graded task for the agent.
type scenario struct {
	name    string
	prompt  string
	approve bool // what the simulated human says at every danger prompt

	mustUseTool string                  // a tool that must be used (or "")
	mustNotUse  string                  // a tool that must NOT be used (or "")
	answerHas   string                  // substring required in the answer (case-insensitive)
	check       func(t *testing.T) bool // extra side-effect assertion
}

func TestEvalAgentBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("live eval; skipped in -short mode")
	}
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	client := llm.NewOpenRouter(key)

	scenarios := []scenario{
		{
			name:       "knowledge/no-tool",
			prompt:     "What is the capital of France? Answer in one word.",
			mustNotUse: "openrouter_web_search",
			answerHas:  "paris",
		},
		{
			name:        "web-search",
			prompt:      "Search the web and tell me: who is the current Prime Minister of the UK?",
			mustUseTool: "openrouter_web_search",
		},
		{
			name:        "weather",
			prompt:      "What is the temperature in Tokyo right now?",
			mustUseTool: "get_weather",
		},
		{
			// The answer is not something the model can know, so a right one is
			// evidence the program really ran rather than that the sandbox was
			// skipped and a plausible number written down.
			name: "sandboxed code",
			prompt: "Use run_code to compute the sum of all prime numbers below 1000, " +
				"then tell me the number as plain digits with no separators.",
			mustUseTool: "run_code",
			answerHas:   "76127",
		},
	}

	passed := 0
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			toolbox := tools.Default(approve(sc.approve),
				tools.WithOpenRouterSearch(client, evalModel),
				tools.WithSandbox(t.TempDir()))
			ag := New(client, evalModel, toolbox)

			var used []string
			ag.OnToolCall = func(name, _, _ string) { used = append(used, name) }

			answer, err := ag.Ask(context.Background(), sc.prompt)
			if err != nil {
				t.Fatalf("agent error: %v", err)
			}

			ok := true
			if sc.mustUseTool != "" && !contains(used, sc.mustUseTool) {
				t.Errorf("expected tool %q to be used; used: %v", sc.mustUseTool, used)
				ok = false
			}
			if sc.mustNotUse != "" && contains(used, sc.mustNotUse) {
				t.Errorf("tool %q should NOT have been used; used: %v", sc.mustNotUse, used)
				ok = false
			}
			if sc.answerHas != "" && !strings.Contains(strings.ToLower(answer), strings.ToLower(sc.answerHas)) {
				t.Errorf("answer %q missing %q", answer, sc.answerHas)
				ok = false
			}
			if sc.check != nil && !sc.check(t) {
				t.Errorf("side-effect check failed")
				ok = false
			}
			if ok {
				passed++
				t.Logf("PASS — tools used: %v", used)
			}
		})
	}
	t.Logf("SCORECARD: %d/%d scenarios passed", passed, len(scenarios))
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
