package evals

import (
	"context"
	"strings"
	"testing"

	braintrust "github.com/braintrustdata/braintrust-sdk-go"
	"github.com/braintrustdata/braintrust-sdk-go/eval"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// The behavioral eval: whole tasks through the real agent loop, graded on what
// the agent chose to do and what actually came back.
//
// This is the mirror of internal/agent/eval_test.go — the same scenarios; what
// changes is that each dimension is now its own named score instead of
// collapsing into one pass/fail, so a run that regresses only on side effects
// is distinguishable from one that regresses on tool choice.

type behaviorInput struct {
	Prompt  string `json:"prompt"`
	Approve bool   `json:"approve"` // what the simulated human says at every gate

	MustUse    string `json:"must_use,omitempty"`
	MustNotUse string `json:"must_not_use,omitempty"`
	AnswerHas  string `json:"answer_has,omitempty"`

	// Check names a side-effect assertion rather than holding one. A func in
	// the input would not survive being serialized into a dataset, and the
	// input is the thing Braintrust stores and shows you next to the score.
	Check string `json:"check,omitempty"`
}

type behaviorOutput struct {
	Answer string   `json:"answer"`
	Tools  []string `json:"tools_used"`
	Error  string   `json:"error,omitempty"`
}

func TestBehaviorEval(t *testing.T) {
	client, model := setup(t)

	// Side-effect checks, by name. The agent's answer is the least interesting
	// thing about a task that was supposed to change something — these are what
	// catch an agent that says it did the work and didn't.
	//
	// Empty while the toolset holds nothing that changes the machine: a case
	// naming no check simply abstains from the side_effect score. A tool with a
	// side effect brings its check back here, and nothing else has to move.
	checks := map[string]func() bool{}

	dataset := eval.NewDataset([]eval.Case[behaviorInput, behaviorOutput]{
		{
			Input: behaviorInput{
				Prompt:     "What is the capital of France? Answer in one word.",
				MustNotUse: "openrouter_web_search",
				AnswerHas:  "paris",
			},
			Tags: []string{"knowledge", "no-tool"},
		},
		{
			Input: behaviorInput{
				Prompt:  "Search the web and tell me: who is the current Prime Minister of the UK?",
				MustUse: "openrouter_web_search",
			},
			Tags: []string{"web-search"},
		},
		{
			Input: behaviorInput{
				Prompt:  "What is the temperature in Tokyo right now?",
				MustUse: "get_weather",
			},
			Tags: []string{"weather"},
		},
		{
			// The answer is not something the model can know, so a right one is
			// evidence the program really ran rather than that the sandbox was
			// skipped and a plausible number written down.
			Input: behaviorInput{
				Prompt: "Use run_code to compute the sum of all prime numbers below 1000, " +
					"then tell me the number as plain digits with no separators.",
				MustUse:   "run_code",
				AnswerHas: "76127",
			},
			Tags: []string{"code-mode"},
		},
	})

	task := eval.T(func(ctx context.Context, in behaviorInput) (behaviorOutput, error) {
		toolbox := tools.Default(approve(in.Approve),
			tools.WithOpenRouterSearch(model, evalModel),
			tools.WithSandbox(t.TempDir()))
		ag := agent.New(model, evalModel, toolbox)

		var out behaviorOutput
		ag.OnToolCall = func(name, _, _ string) { out.Tools = append(out.Tools, name) }

		answer, err := ag.Ask(ctx, in.Prompt)
		out.Answer = answer
		if err != nil {
			// Returned as data rather than an error: a run that failed is a
			// result worth scoring zero and keeping, not a case to drop from
			// the experiment.
			out.Error = err.Error()
		}
		return out, nil
	})

	// One scorer, several named scores, each abstaining when the case does not
	// declare it. That is the same "n/a" idea the in-repo evals use, and it is
	// what lets one scorer cover a mixed dataset — an abstention never drags an
	// average down, because it is simply not in the series.
	scorer := eval.NewScorer("behavior", func(_ context.Context, r eval.TaskResult[behaviorInput, behaviorOutput]) (eval.Scores, error) {
		in, out := r.Input, r.Output
		var scores eval.Scores

		failed := out.Error != ""

		if in.MustUse != "" || in.MustNotUse != "" {
			ok := true
			if in.MustUse != "" && !contains(out.Tools, in.MustUse) {
				ok = false
			}
			if in.MustNotUse != "" && contains(out.Tools, in.MustNotUse) {
				ok = false
			}
			scores = append(scores, eval.Score{
				Name:  "tool_choice",
				Score: boolScore(ok && !failed),
				Metadata: map[string]any{
					"used": out.Tools, "must_use": in.MustUse, "must_not_use": in.MustNotUse,
				},
			})
		}

		if in.AnswerHas != "" {
			hit := strings.Contains(strings.ToLower(out.Answer), strings.ToLower(in.AnswerHas))
			scores = append(scores, eval.Score{
				Name:     "answer_match",
				Score:    boolScore(hit && !failed),
				Metadata: map[string]any{"wanted": in.AnswerHas},
			})
		}

		if in.Check != "" {
			check, known := checks[in.Check]
			if !known {
				return nil, nil
			}
			scores = append(scores, eval.Score{
				Name:     "side_effect",
				Score:    boolScore(check() && !failed),
				Metadata: map[string]any{"check": in.Check},
			})
		}

		return scores, nil
	})

	evaluator := braintrust.NewEvaluator[behaviorInput, behaviorOutput](client)
	res, runErr := evaluator.Run(context.Background(), eval.Opts[behaviorInput, behaviorOutput]{
		Experiment: "behavior",
		Dataset:    dataset,
		Task:       task,
		Scorers:    []eval.Scorer[behaviorInput, behaviorOutput]{scorer},
		Metadata:   eval.Metadata{"model": evalModel, "suite": "behavior"},
		// Serial: side-effect checks look at shared state, so cases that change
		// something must not overlap.
		Parallelism: 1,
		Quiet:       true,
	})
	report(t, "behavior", res, runErr)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// boolScore maps a pass/fail dimension onto the 0..1 range Braintrust averages.
func boolScore(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}
