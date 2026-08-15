package evals

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"testing"

	braintrust "github.com/braintrustdata/braintrust-sdk-go"
	"github.com/braintrustdata/braintrust-sdk-go/eval"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/evalscore"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// The diagram eval: four scored dimensions per case, which is why this is the
// suite that most wants a dashboard.
//
// The dataset and every scorer come from internal/evalscore — the same code the
// in-repo Go eval runs. Nothing here re-implements a judgement; this file is
// only the Braintrust front-end for it.
//
// The dimensions abstain individually (structure applies only to create cases,
// preservation only to modify cases), and Braintrust handles that natively: a
// score simply is not in the list, so it is not averaged. That maps exactly onto
// the "n/a" the Go scorecard prints.

type diagramOutput struct {
	Reply     string         `json:"reply"`
	Spec      evalscore.Spec `json:"spec"`
	SpecRead  bool           `json:"spec_read"`
	SVGOK     bool           `json:"svg_ok"`
	SceneOK   bool           `json:"scene_ok"`
	ToolsUsed []string       `json:"tools_used"`
	Error     string         `json:"error,omitempty"`
}

// runResult rebuilds the shape the shared scorers expect. The wire type above
// carries a string error because an error interface does not serialize into
// anything a human can read in a dashboard.
func (o diagramOutput) runResult() evalscore.RunResult {
	r := evalscore.RunResult{
		Reply: o.Reply, Spec: o.Spec, SpecRead: o.SpecRead,
		SVGOK: o.SVGOK, SceneOK: o.SceneOK, ToolsUsed: o.ToolsUsed,
	}
	if o.Error != "" {
		r.Err = fmt.Errorf("%s", o.Error)
	}
	return r
}

func TestDiagramEval(t *testing.T) {
	client, model := setup(t)

	task := eval.T(func(ctx context.Context, c evalscore.Case) (diagramOutput, error) {
		// The diagram tools write to the working directory, so each case runs
		// in its own directory and chdir is what isolates one canvas from the
		// next. chdir is process-global, which is exactly why this experiment
		// runs serially — see Parallelism below.
		dir, err := os.MkdirTemp("", "diagram-eval-")
		if err != nil {
			return diagramOutput{}, err
		}
		defer os.RemoveAll(dir)

		previous, err := os.Getwd()
		if err != nil {
			return diagramOutput{}, err
		}
		if err := os.Chdir(dir); err != nil {
			return diagramOutput{}, err
		}
		defer os.Chdir(previous)

		if c.Seed != "" {
			// Seeded by calling the tool the agent would, so the starting point
			// is a real canvas rather than a hand-written file that might not
			// match what the tools produce.
			reg := tools.Default(approve(true))
			if out := reg.Dispatch(ctx, "generate_diagram", c.Seed); len(out) > 6 && out[:6] == "error:" {
				return diagramOutput{}, fmt.Errorf("seeding the canvas: %s", out)
			}
		}

		toolbox := tools.Default(approve(true))
		ag := agent.New(model, evalModel, toolbox)

		var out diagramOutput
		ag.OnToolCall = func(name, _, _ string) { out.ToolsUsed = append(out.ToolsUsed, name) }

		answer, askErr := ag.Ask(ctx, c.Prompt)
		out.Reply = answer
		if askErr != nil {
			out.Error = askErr.Error()
		}

		// Read back exactly the artifacts a user would open.
		if raw, readErr := os.ReadFile(evalscore.SpecFile); readErr == nil {
			if json.Unmarshal(raw, &out.Spec) == nil {
				out.SpecRead = true
			}
		}
		if raw, readErr := os.ReadFile(evalscore.SVGFile); readErr == nil && len(raw) > 0 {
			out.SVGOK = xml.Unmarshal(raw, new(struct{ XMLName xml.Name })) == nil
		}
		if raw, readErr := os.ReadFile(evalscore.SceneFile); readErr == nil && len(raw) > 0 {
			out.SceneOK = json.Valid(raw)
		}
		return out, nil
	})

	// One Braintrust scorer wrapping all four shared ones. They are wrapped
	// together rather than registered separately because they share the
	// zero-on-error rule below, and splitting them would mean four copies of it.
	scorer := eval.NewScorer("diagram", func(_ context.Context, r eval.TaskResult[evalscore.Case, diagramOutput]) (eval.Scores, error) {
		run := r.Output.runResult()

		var scores eval.Scores
		for _, s := range evalscore.All() {
			v := s.Score(r.Input, run)
			if v == nil {
				continue // this dimension does not apply to this case
			}
			if run.Err != nil {
				// A run that errored scores zero across the board. Without this
				// a modify case whose agent never got off the ground scores
				// preservation=1.00 for the seed "surviving" — an agent that
				// does nothing preserves everything perfectly.
				v = evalscore.Score(0)
			}
			scores = append(scores, eval.Score{Name: s.Name, Score: *v})
		}
		return scores, nil
	})

	// The dataset is the shared one, tagged so create and modify cases can be
	// filtered apart in the UI — they fail for different reasons and it is
	// worth being able to look at them separately.
	cases := evalscore.Cases()
	rows := make([]eval.Case[evalscore.Case, diagramOutput], 0, len(cases))
	for _, c := range cases {
		kind := "create"
		if c.Seed != "" {
			kind = "modify"
		}
		rows = append(rows, eval.Case[evalscore.Case, diagramOutput]{
			Input:    c,
			Tags:     []string{kind},
			Metadata: map[string]any{"case": c.Name},
		})
	}

	evaluator := braintrust.NewEvaluator[evalscore.Case, diagramOutput](client)
	res, runErr := evaluator.Run(context.Background(), eval.Opts[evalscore.Case, diagramOutput]{
		Experiment: "diagram",
		Dataset:    eval.NewDataset(rows),
		Task:       task,
		Scorers:    []eval.Scorer[evalscore.Case, diagramOutput]{scorer},
		Metadata:   eval.Metadata{"model": evalModel, "suite": "diagram"},
		// Serial, and not negotiable: the task chdirs, and the working
		// directory belongs to the process, not the goroutine. Two cases in
		// flight would write over each other's canvas.
		Parallelism: 1,
		Quiet:       true,
	})
	report(t, "diagram", res, runErr)
}
