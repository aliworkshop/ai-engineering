package agent

// Single-turn tool-SELECTION eval.
//
// The behavioral eval (eval_test.go) runs whole tasks and grades side effects.
// This one asks a narrower question: given a one-shot request, does the model
// reach for the RIGHT tool on the very first step? We stop after ONE model call
// and never execute anything — we only look at which tools it *chose*. That
// isolates tool selection from execution, so a failure points at the tool
// descriptions or the system prompt, not at the tools' behavior.
//
// Run:  go test ./agent/internal/agent -run EvalToolSelection -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

	"agent/internal/llm"
	"agent/internal/tools"
)

// selectionCase is one prompt and the exact set of tools we expect the model to
// call on the first step. An empty expect means "answer from knowledge, no tool".
//
// expectArgs, when set, additionally checks the arguments the model passed to
// the chosen tool: each entry is arg-name -> a substring that value must contain
// (case-insensitive). We match loosely on purpose — the model may normalize
// "/tmp/old.log" or expand "Tokyo" to "Tokyo, Japan", and we care that the
// salient value made it through, not that it matches character-for-character.
type selectionCase struct {
	prompt     string
	expect     []string
	expectArgs map[string]string
}

func TestEvalToolSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("live eval; skipped in -short mode")
	}
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	client := llm.NewOpenRouter(key)

	// A real registry so we advertise the exact specs production uses. The
	// approver is never called — we stop before any tool executes.
	toolbox := tools.Default(approve(false))
	specs := toolbox.Specs()

	cases := []selectionCase{
		{"Read the contents of go.mod", []string{"read_file"}, map[string]string{"path": "go.mod"}},
		{"Create hello.txt containing 'hi'", []string{"write_file"}, map[string]string{"path": "hello.txt", "content": "hi"}},
		{"Delete the file /tmp/old.log", []string{"delete_file"}, map[string]string{"path": "old.log"}},
		{"Run the command `ls -la` and show me the output", []string{"run_command"}, map[string]string{"command": "ls -la"}},
		{"Who is the current Prime Minister of the UK?", []string{"web_search"}, nil},
		{"What's the weather like in Tokyo right now?", []string{"get_weather"}, map[string]string{"location": "Tokyo"}},
		{"How windy is it in Chicago at the moment?", []string{"get_weather"}, map[string]string{"location": "Chicago"}},
		{"What is the capital of France?", nil, nil}, // negative: knowledge, no tool
	}

	passed := 0
	for _, c := range cases {
		calls := toolsChosen(t, client, specs, c.prompt)
		chosen := callNames(calls)

		ok := sameSet(chosen, c.expect)
		if reason := argsMismatch(calls, c.expectArgs); ok && reason != "" {
			ok = false
			t.Errorf("args wrong for %q: %s", c.prompt, reason)
		} else if !ok {
			t.Errorf("wrong tool selection for %q: chose=%v want=%v", c.prompt, chosen, c.expect)
		}
		if ok {
			passed++
		}
		t.Logf("[%s] %q chose=%v want=%v args=%v", passLabel(ok), c.prompt, chosen, c.expect, callArgs(calls))
	}
	t.Logf("SCORECARD: %d/%d tool-selection cases passed", passed, len(cases))
}

// toolsChosen makes ONE model call with tools advertised and tool_choice=auto,
// then returns the tool calls the model asked for (nil if none). It mirrors the
// real agent's first `think` step but never runs the tools — so we can inspect
// both which tools it chose and the arguments it passed them.
func toolsChosen(t *testing.T, client *openrouter.OpenRouter, specs []components.ChatFunctionTool, prompt string) []components.ChatToolCall {
	t.Helper()
	auto := components.CreateChatToolChoiceChatToolChoiceAuto(components.ChatToolChoiceAutoAuto)
	res, err := client.Chat.Send(context.Background(), components.ChatRequest{
		Model: openrouter.String(evalModel),
		Messages: []components.ChatMessages{
			systemMsg(SystemPrompt),
			userMsg(prompt),
		},
		Tools:      specs,
		ToolChoice: &auto,
	}, nil)
	if err != nil {
		t.Fatalf("completion error: %v", err)
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		t.Fatalf("model returned no choices")
	}
	return res.ChatResult.Choices[0].Message.ToolCalls
}

// callNames pulls the tool names out of a set of calls.
func callNames(calls []components.ChatToolCall) []string {
	var names []string
	for _, c := range calls {
		names = append(names, c.Function.Name)
	}
	return names
}

// callArgs pulls the raw argument JSON out of each call, for readable logging.
func callArgs(calls []components.ChatToolCall) []string {
	var args []string
	for _, c := range calls {
		args = append(args, c.Function.Arguments)
	}
	return args
}

// argsMismatch returns "" if the chosen tool's arguments satisfy want, or a
// human-readable reason otherwise. Each want entry requires that argument's
// value to contain the given substring (case-insensitive). An empty want passes
// trivially — it's how cases opt out of argument checking.
func argsMismatch(calls []components.ChatToolCall, want map[string]string) string {
	if len(want) == 0 {
		return ""
	}
	if len(calls) == 0 {
		return "no tool was called, so there are no arguments to check"
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &got); err != nil {
		return fmt.Sprintf("arguments are not valid JSON: %v", err)
	}
	for name, sub := range want {
		raw, present := got[name]
		if !present {
			return fmt.Sprintf("missing argument %q", name)
		}
		val := fmt.Sprintf("%v", raw)
		if !strings.Contains(strings.ToLower(val), strings.ToLower(sub)) {
			return fmt.Sprintf("argument %q = %q; expected it to contain %q", name, val, sub)
		}
	}
	return ""
}

// sameSet reports whether two lists hold the same set of names (order- and
// duplicate-insensitive), matching Python's set(chosen) == set(expect).
func sameSet(a, b []string) bool {
	return keyOf(a) == keyOf(b)
}

// keyOf collapses a list into a sorted, de-duplicated string key — a stand-in
// for a set so two lists with the same members compare equal.
func keyOf(list []string) string {
	seen := map[string]struct{}{}
	for _, s := range list {
		seen[s] = struct{}{}
	}
	uniq := make([]string, 0, len(seen))
	for s := range seen {
		uniq = append(uniq, s)
	}
	sort.Strings(uniq)
	return strings.Join(uniq, ",")
}

func passLabel(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
