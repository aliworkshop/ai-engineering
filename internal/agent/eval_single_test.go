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

	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
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

	// The teacher's own toolset, not the whole registry: tool selection is a
	// property of the agent, and the teacher can only choose between the two
	// tools it actually holds.
	specs := teacherSpecs(t)

	cases := []selectionCase{
		{"Is it 'a hour' or 'an hour'?", []string{tools.KnowledgeTool}, map[string]string{"query": "an"}},
		{"When do I use the present perfect instead of the past simple?", []string{tools.KnowledgeTool}, map[string]string{"query": "perfect"}},
		{"Correct this: she dont like when i writes letters.", []string{tools.KnowledgeTool}, nil},
		// Not its job, and the point of the roster: the teacher has no tool that
		// touches the machine, so the only right move is to hand over.
		{"Delete the file /tmp/draft.txt for me.", []string{tools.HandoffTool}, map[string]string{"to": "operator"}},
		{"Thanks, that was helpful!", nil, nil}, // negative: nothing to look up
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
		Messages: toSDK([]Msg{
			{Role: "system", Text: TeacherPrompt},
			{Role: "user", Text: prompt},
		}),
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

// teacherSpecs is the toolset the teacher is advertised, built the way
// production builds it: the full registry, then the teacher's subset of it.
// Advertising the whole registry instead would grade an agent that does not
// exist — and would make "chose the right tool" easier than it really is.
func teacherSpecs(t *testing.T) []components.ChatFunctionTool {
	t.Helper()
	registry := tools.Default(approve(false),
		tools.WithKnowledge(corpusDir),
		tools.WithSpecialists(map[string]string{
			TeacherName:  TeacherPurpose,
			OperatorName: OperatorPurpose,
		}))
	return registry.Subset(TeacherTools...).Specs()
}
