package agent

// Behavioral eval harness.
//
// Where the tools package unit-tests each tool deterministically, this runs
// whole tasks through the REAL teacher — the same roster main.go wires, the
// same corpus, the same prompt — and grades what came back: did it look the
// rule up before explaining it, did it correct the text instead of answering
// it, did it cite the file it actually used, and is the reply about what was
// asked?
//
// That last one is answer relevancy: deepeval's metric, ported to Go in
// internal/evalscore and run over every scenario here. It is graded on all of
// them rather than a chosen few, because "the reply wandered" is the failure
// this agent's prompt spends three sentences preventing, and a metric you only
// run sometimes is a metric that regresses quietly.
//
// Run:  go test ./internal/agent -run Eval -v
// (needs OPENROUTER_API_KEY; skipped with -short)

import (
	"context"
	"os"
	"strings"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/evalscore"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

const evalModel = "openai/gpt-4o-mini"

// corpusDir is the teacher's reference, relative to this package. The evals
// point at the real one: retrieval that works against a fixture but not against
// the corpus you ship is not a signal.
const corpusDir = "../../corpus"

// relevancyBar is deepeval's usual threshold for AnswerRelevancyMetric. Below
// it, an answer is carrying enough unrelated material to notice.
const relevancyBar = 0.7

// approve is a stub human answering yes/no to every approval request.
type approve bool

func (a approve) Confirm(string) bool { return bool(a) }

// scenario is one graded task for the agent.
type scenario struct {
	name   string
	prompt string

	mustUseTool string // a tool that must be used (or "")
	mustNotUse  string // a tool that must NOT be used (or "")
	answerHas   string // substring required in the answer (case-insensitive)
	answerLacks string // substring that must NOT appear (or "")
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
	judge := evalscore.AnswerRelevancy{Client: client, Model: evalModel}

	scenarios := []scenario{
		{
			// The everyday case, and the one that has to look the rule up: the
			// prompt says search before you name a rule.
			name:        "correct/basic-errors",
			prompt:      "Please correct this: she dont like when i writes letters to her.",
			mustUseTool: tools.KnowledgeTool,
			answerHas:   "doesn't",
		},
		{
			// Nothing to fix. An agent that always finds something is worse than
			// useless — it teaches the writer to distrust it.
			name:      "correct/already-correct",
			prompt:    "Is there anything wrong with this sentence? The report was finished on time, and everyone signed it.",
			answerHas: "correct",
		},
		{
			// The failure every correction agent has: answering the sentence
			// instead of fixing it.
			name:        "correct/question-not-answer",
			prompt:      "Fix the grammar and punctuation: where is the nearest station can you tell me",
			answerHas:   "Where is the nearest station",
			answerLacks: "I don't know where",
		},
		{
			name:      "question/a-vs-an",
			prompt:    "Is it 'a hour' or 'an hour', and what is the rule?",
			answerHas: "an hour",
		},
		{
			// Retrieval end to end: the answer has to name the file the rule
			// actually came from, and only pronouns.md carries this one.
			name:      "question/who-vs-whom",
			prompt:    "What is the difference between 'who' and 'whom'?",
			answerHas: "pronouns",
		},
	}

	var passed int
	var relevancy []float64
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			ag, used := teacher(t, client)

			answer, err := ag.Ask(context.Background(), sc.prompt)
			if err != nil {
				t.Fatalf("agent error: %v", err)
			}

			ok := true
			if sc.mustUseTool != "" && !contains(*used, sc.mustUseTool) {
				t.Errorf("expected tool %q to be used; used: %v", sc.mustUseTool, *used)
				ok = false
			}
			if sc.mustNotUse != "" && contains(*used, sc.mustNotUse) {
				t.Errorf("tool %q should NOT have been used; used: %v", sc.mustNotUse, *used)
				ok = false
			}
			if sc.answerHas != "" && !strings.Contains(strings.ToLower(answer), strings.ToLower(sc.answerHas)) {
				t.Errorf("answer missing %q:\n%s", sc.answerHas, answer)
				ok = false
			}
			if sc.answerLacks != "" && strings.Contains(strings.ToLower(answer), strings.ToLower(sc.answerLacks)) {
				t.Errorf("answer should not contain %q:\n%s", sc.answerLacks, answer)
				ok = false
			}

			// Relevancy is scored on every scenario, and a judging failure is
			// reported as one rather than as a zero: a broken judge and a
			// rambling agent are different problems.
			score, err := judge.Score(context.Background(), sc.prompt, answer)
			if err != nil {
				t.Fatalf("relevancy judge failed: %v", err)
			}
			relevancy = append(relevancy, score.Score)
			if !score.Passed(relevancyBar) {
				t.Errorf("relevancy %.2f below %.2f — %s", score.Score, relevancyBar, score.Reason)
				ok = false
			}

			if ok {
				passed++
			}
			t.Logf("[%s] relevancy %.2f — tools used: %v\n      %s",
				passLabel(ok), score.Score, *used, score.Reason)
		})
	}

	var sum float64
	for _, r := range relevancy {
		sum += r
	}
	t.Logf("SCORECARD: %d/%d scenarios passed, mean relevancy %.2f",
		passed, len(scenarios), sum/float64(len(relevancy)))
}

// teacher builds the agent under test the way production builds it: the whole
// registry, then TeacherRoster over it, entering as the teacher. The returned
// pointer collects every tool name the run used.
//
// An eval that assembles its own simplified agent grades something nobody runs,
// and the gap between the two stays invisible until the day it matters.
func teacher(t *testing.T, client *openrouter.OpenRouter) (*Agent, *[]string) {
	t.Helper()
	registry := tools.Default(approve(false),
		tools.WithKnowledge(corpusDir),
		tools.WithSpecialists(map[string]string{
			TeacherName:  TeacherPurpose,
			OperatorName: OperatorPurpose,
		}))

	ag := New(client, evalModel, registry).WithRoster(TeacherRoster(registry), TeacherName)
	used := new([]string)
	ag.OnToolCall = func(name, _, _ string) { *used = append(*used, name) }
	return ag, used
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
