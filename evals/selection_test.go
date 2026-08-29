package evals

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	braintrust "github.com/braintrustdata/braintrust-sdk-go"
	"github.com/braintrustdata/braintrust-sdk-go/eval"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// The tool-selection eval: one model call, no execution.
//
// This is the cheapest signal in the suite and the one most sensitive to prompt
// edits. It asks the narrow question — given this request and these tool specs,
// does the model reach for the right tool and fill in the right arguments on the
// first step? — and never runs anything, so it costs one call per case and can
// safely include "delete /tmp/old.log".
//
// It is also the eval most worth having history for. A tweak to a tool's
// description can quietly move selection accuracy, and that is invisible in a
// pass/fail run that still says 8/8.

type selectionInput struct {
	Prompt string `json:"prompt"`

	// ExpectTools is the exact set expected. Empty means "answer from
	// knowledge, call nothing" — the negative case, which matters as much as
	// the positives: an agent that reaches for a tool on every question is
	// slower, costlier, and worse.
	ExpectTools []string `json:"expect_tools"`

	// ExpectArgs is arg-name to a substring the value must contain, matched
	// case-insensitively. Loose on purpose: the model may normalise
	// "/tmp/old.log" or expand "Tokyo" to "Tokyo, Japan", and what matters is
	// that the salient value survived, not that it matches character for
	// character.
	ExpectArgs map[string]string `json:"expect_args,omitempty"`
}

type selectionOutput struct {
	Tools []string          `json:"tools_chosen"`
	Args  map[string]string `json:"args,omitempty"`
	Error string            `json:"error,omitempty"`
}

func TestToolSelectionEval(t *testing.T) {
	client, model := setup(t)

	// A real registry, so the specs advertised are exactly the ones production
	// advertises. The approver is never called — we stop before any tool runs.
	toolbox := tools.Default(approve(false), tools.WithOpenRouterSearch(model, evalModel))
	specs := toolbox.Specs()

	dataset := eval.NewDataset([]eval.Case[selectionInput, selectionOutput]{
		{Input: selectionInput{"Read the contents of go.mod", []string{"read_file"}, map[string]string{"path": "go.mod"}}, Tags: []string{"read"}},
		{Input: selectionInput{"Create hello.txt containing 'hi'", []string{"write_file"}, map[string]string{"path": "hello.txt", "content": "hi"}}, Tags: []string{"write"}},
		{Input: selectionInput{"Delete the file /tmp/old.log", []string{"delete_file"}, map[string]string{"path": "old.log"}}, Tags: []string{"delete"}},
		{Input: selectionInput{"What is 17 * 23?", nil, nil}, Tags: []string{"arithmetic", "negative"}},
		{Input: selectionInput{"Who is the current Prime Minister of the UK?", []string{"openrouter_web_search"}, nil}, Tags: []string{"search"}},
		{Input: selectionInput{"What's the weather like in Tokyo right now?", []string{"get_weather"}, map[string]string{"location": "Tokyo"}}, Tags: []string{"weather"}},
		{Input: selectionInput{"How windy is it in Chicago at the moment?", []string{"get_weather"}, map[string]string{"location": "Chicago"}}, Tags: []string{"weather"}},
		{Input: selectionInput{"What is the capital of France?", nil, nil}, Tags: []string{"knowledge", "negative"}},
	})

	task := eval.T(func(ctx context.Context, in selectionInput) (selectionOutput, error) {
		calls, err := chooseTools(ctx, model, specs, in.Prompt)
		if err != nil {
			return selectionOutput{Error: err.Error()}, nil
		}

		out := selectionOutput{Args: map[string]string{}}
		for _, c := range calls {
			out.Tools = append(out.Tools, c.Function.Name)

			// Flattened to name -> value so the arguments are readable in the
			// Braintrust UI next to the score. A raw JSON blob is technically
			// the same information and nobody reads it.
			var parsed map[string]any
			if json.Unmarshal([]byte(c.Function.Arguments), &parsed) == nil {
				for k, v := range parsed {
					if s, ok := v.(string); ok {
						out.Args[k] = s
					}
				}
			}
		}
		return out, nil
	})

	scorer := eval.NewScorer("selection", func(_ context.Context, r eval.TaskResult[selectionInput, selectionOutput]) (eval.Scores, error) {
		in, out := r.Input, r.Output
		if out.Error != "" {
			return eval.Scores{{Name: "correct_tool", Score: 0, Metadata: map[string]any{"error": out.Error}}}, nil
		}

		right := sameSet(out.Tools, in.ExpectTools)
		scores := eval.Scores{{
			Name:     "correct_tool",
			Score:    boolScore(right),
			Metadata: map[string]any{"chose": out.Tools, "wanted": in.ExpectTools},
		}}

		// Arguments are only scored when the tool was right. Grading the
		// arguments of a tool that should never have been called measures
		// nothing, and averaging that in would make a wrong choice look
		// half-correct.
		if len(in.ExpectArgs) > 0 && right {
			matched, missing := 0, map[string]string{}
			for name, want := range in.ExpectArgs {
				if strings.Contains(strings.ToLower(out.Args[name]), strings.ToLower(want)) {
					matched++
				} else {
					missing[name] = out.Args[name]
				}
			}
			scores = append(scores, eval.Score{
				Name:     "correct_args",
				Score:    float64(matched) / float64(len(in.ExpectArgs)),
				Metadata: map[string]any{"wanted": in.ExpectArgs, "mismatched": missing},
			})
		}
		return scores, nil
	})

	evaluator := braintrust.NewEvaluator[selectionInput, selectionOutput](client)
	res, runErr := evaluator.Run(context.Background(), eval.Opts[selectionInput, selectionOutput]{
		Experiment: "tool-selection",
		Dataset:    dataset,
		Task:       task,
		Scorers:    []eval.Scorer[selectionInput, selectionOutput]{scorer},
		Metadata:   eval.Metadata{"model": evalModel, "suite": "tool-selection"},
		// Safe to fan out: nothing here executes a tool or touches disk.
		Parallelism: 4,
		Quiet:       true,
	})
	report(t, "tool-selection", res, runErr)
}

// chooseTools makes ONE model call with the tools advertised and tool_choice
// auto, and returns what the model asked for. It mirrors the agent's first
// think step but stops before dispatch — which is what makes it safe to ask for
// a deletion.
func chooseTools(ctx context.Context, client *openrouter.OpenRouter, specs []components.ChatFunctionTool, prompt string) ([]components.ChatToolCall, error) {
	auto := components.CreateChatToolChoiceChatToolChoiceAuto(components.ChatToolChoiceAutoAuto)

	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model: openrouter.String(evalModel),
		Messages: []components.ChatMessages{
			components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Role:    components.ChatSystemMessageRoleSystem,
				Content: components.CreateChatSystemMessageContentStr(agent.SystemPrompt),
			}),
			components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(prompt),
			}),
		},
		Tools:      specs,
		ToolChoice: &auto,
	}, nil)
	if err != nil {
		return nil, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return nil, errNoChoices
	}
	return res.ChatResult.Choices[0].Message.ToolCalls, nil
}

// errNoChoices is its own value so a scorer reading the metadata can tell an
// empty response from a network failure.
var errNoChoices = errNoChoicesType{}

type errNoChoicesType struct{}

func (errNoChoicesType) Error() string { return "model returned no choices" }

// sameSet compares tool selections order-insensitively — the model may batch
// two calls in either order and both are correct.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
