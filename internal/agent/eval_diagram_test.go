package agent

// Diagram eval harness.
//
// Runs whole diagram tasks through the REAL agent loop and grades what lands on
// the canvas. Unlike the tools package's unit tests — which call the tools
// directly with known-good arguments — this measures the part that can actually
// regress silently: whether the model picks the right tool and fills it in
// sensibly from a plain-English prompt.
//
// Four scorers, each returning a score in 0..1 or nil for "doesn't apply to this
// case". One scorer set therefore covers the whole dataset, and a case only
// declares what's relevant to it:
//
//	schema       — is every element valid? Catches nothing / garbage. All cases.
//	structure    — expected characteristics ("3 rectangle elements") vs actual
//	               counts, proportional credit. Create cases only.
//	preservation — did the seeded diagram survive the edit, or did the agent nuke
//	               the canvas and start over? Modify cases only.
//	keywords     — does the domain vocabulary show up in the labels or the reply?
//	               Only cases that declare keywords.
//
// Run:  go test ./internal/agent -run EvalDiagram -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/evalscore"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// The dataset and the scorers now live in internal/evalscore, because they are
// not test logic: they decide whether a diagram is *good*, and that judgement
// has a second caller — the Braintrust eval in the evals/ module, which is a
// separate module and cannot import a _test.go file. One copy, two front-ends.
//
// What stays here is everything that is genuinely about running the test: the
// chdir isolation, driving the real agent loop, and the scorecard.

// ---------- the harness ----------

func TestEvalDiagramTools(t *testing.T) {
	if testing.Short() {
		t.Skip("live eval; skipped in -short mode")
	}
	// Loaded before any t.Chdir below, while the relative path still resolves.
	godotenv.Load("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	client := llm.NewOpenRouter(key)

	scorers := evalscore.All()
	cases := evalscore.Cases()

	// totals[scorer] accumulates only the cases that scorer applied to, so an
	// abstention never drags an average down.
	totals := map[string]float64{}
	counts := map[string]int{}
	var rows []string
	passed := 0

	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			// Each case gets its own directory, and the diagram tools write to
			// the working directory — so chdir is what isolates one case's
			// canvas from the next. t.Chdir restores it when the subtest ends.
			t.Chdir(t.TempDir())

			if c.Seed != "" {
				if err := seedCanvas(c.Seed); err != nil {
					t.Fatalf("seeding the canvas: %v", err)
				}
			}

			r := runCase(t, client, c)
			if r.Err != nil {
				t.Errorf("agent error: %v", r.Err)
			}

			// A case passes when every scorer that applied to it cleared the
			// bar. One bad dimension fails the case, the same way a wrong
			// argument fails a tool-selection case.
			ok := r.Err == nil
			var cells []string
			for _, s := range scorers {
				v := s.Score(c, r)
				if v != nil && r.Err != nil {
					// A run that errored scores zero across the board. Without
					// this, a modify case whose agent never got off the ground
					// scores preservation=1.00 for the seed "surviving" — an
					// agent that does nothing preserves everything perfectly.
					v = evalscore.Score(0)
				}
				if v == nil {
					cells = append(cells, fmt.Sprintf("%s=n/a", s.Name))
					continue
				}
				totals[s.Name] += *v
				counts[s.Name]++
				cells = append(cells, fmt.Sprintf("%s=%.2f", s.Name, *v))
				if *v < evalscore.PassingScore {
					ok = false
					t.Errorf("%s scored %.2f (below %.2f); tools used: %v", s.Name, *v, evalscore.PassingScore, r.ToolsUsed)
				}
			}
			if ok {
				passed++
			}
			row := fmt.Sprintf("[%s] %-24s %s", passLabel(ok), c.Name, strings.Join(cells, "  "))
			rows = append(rows, row)
			t.Logf("%s | tools: %v", row, r.ToolsUsed)
		})
	}

	// The scorecard is the point of the whole harness, so print it as a block:
	// per-case verdicts, then the mean of each scorer over the cases it applied
	// to, then the overall rate.
	t.Log("SCORECARD")
	for _, row := range rows {
		t.Log("  " + row)
	}
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("  mean %-12s %.2f  (%d/%d cases applied)",
			name, totals[name]/float64(counts[name]), counts[name], len(cases))
	}
	t.Logf("SCORECARD: %d/%d diagram cases passed (%.0f%%)",
		passed, len(cases), 100*float64(passed)/float64(len(cases)))
}

// seedCanvas draws the starting diagram for a modify case by calling the tool
// the agent would, so the seed is a real canvas and not a hand-written file
// that might not match what the tools produce.
func seedCanvas(seed string) error {
	reg := tools.Default(approve(true))
	if out := reg.Dispatch(context.Background(), "generate_diagram", seed); strings.HasPrefix(out, "error:") {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// runCase drives one prompt through the real agent loop and collects everything
// the scorers need.
func runCase(t *testing.T, client *openrouter.OpenRouter, c evalscore.Case) evalscore.RunResult {
	t.Helper()

	toolbox := tools.Default(approve(true))
	ag := New(client, evalModel, toolbox)

	var r evalscore.RunResult
	ag.OnToolCall = func(name, _, _ string) { r.ToolsUsed = append(r.ToolsUsed, name) }

	answer, err := ag.Ask(context.Background(), c.Prompt)
	r.Reply, r.Err = answer, err

	if raw, readErr := os.ReadFile(evalscore.SpecFile); readErr == nil {
		if json.Unmarshal(raw, &r.Spec) == nil {
			r.SpecRead = true
		}
	}
	if raw, readErr := os.ReadFile(evalscore.SVGFile); readErr == nil && len(raw) > 0 {
		r.SVGOK = xml.Unmarshal(raw, new(struct{ XMLName xml.Name })) == nil
	}
	if raw, readErr := os.ReadFile(evalscore.SceneFile); readErr == nil && len(raw) > 0 {
		r.SceneOK = json.Valid(raw)
	}
	return r
}
