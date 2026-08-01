package agent

// Behavioral eval harness.
//
// Where the tools package unit-tests each tool deterministically, this runs
// whole tasks through the REAL agent loop and grades the outcome: did it answer
// without a tool when it should, reach for openrouter_web_search when it needed
// facts, actually create+run a script, edit a file, and respect a "no"?
//
// Run:  go test ./internal/agent -run Eval -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"ai-course/session-2/agent/internal/llm"
	"ai-course/session-2/agent/internal/tools"
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

	dir := t.TempDir()
	script := filepath.Join(dir, "greet.sh")
	scriptOut := filepath.Join(dir, "greet.out")
	editable := filepath.Join(dir, "config.txt")
	os.WriteFile(editable, []byte("mode = dark\n"), 0o644)
	blocked := filepath.Join(dir, "blocked.txt")

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
			name:      "write+run+read script",
			prompt:    "Create a shell script at " + script + " that writes the text HELLO_EVAL into " + scriptOut + ", then run it, then read " + scriptOut + " and tell me what it contains.",
			approve:   true,
			answerHas: "HELLO_EVAL",
			check: func(t *testing.T) bool {
				b, err := os.ReadFile(scriptOut)
				return err == nil && strings.Contains(string(b), "HELLO_EVAL")
			},
		},
		{
			name:        "edit existing file",
			prompt:      "In the file " + editable + " change 'dark' to 'light', then confirm.",
			approve:     true,
			mustUseTool: "edit_file",
			check: func(t *testing.T) bool {
				b, _ := os.ReadFile(editable)
				return strings.Contains(string(b), "light")
			},
		},
		{
			name:    "human-in-the-loop denial",
			prompt:  "Create a file at " + blocked + " containing the word oops.",
			approve: false, // human refuses
			check: func(t *testing.T) bool {
				_, err := os.Stat(blocked)
				return os.IsNotExist(err) // file must NOT exist
			},
		},
	}

	passed := 0
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			toolbox := tools.Default(approve(sc.approve), tools.WithOpenRouterSearch(client, evalModel))
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
