// Package evalscore holds the scorers the evals share.
//
// It exists as its own package because the same judgement has two front-ends:
// the Go eval tests in internal/agent, which print a scorecard offline, and the
// Braintrust experiment in evals/, which is a separate module and cannot import
// a _test.go file. One implementation, two callers — two copies of a metric
// drift within a week, and the first symptom is two dashboards disagreeing
// about the same run.
package evalscore

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// AnswerRelevancy is deepeval's AnswerRelevancyMetric, in Go.
//
// deepeval is a Python library with no Go SDK, but the metric is a recipe
// rather than a library trick, and it is a short one:
//
//  1. an LLM splits the answer into statements;
//  2. the same LLM says, per statement, whether it addresses the question;
//  3. score = relevant statements / total statements;
//  4. one more call turns the "no" verdicts into a sentence a human can read.
//
// What it measures is worth being precise about, because the name oversells it:
// this is whether the reply is ABOUT the question, not whether the reply is
// right. An agent that teaches a wrong rule fluently and on topic scores 1.00.
// What it catches is the other failure — the neighbouring rule nobody asked
// about, the correction that wanders into a style rewrite — which is exactly
// what the teacher's prompt is told not to do.
//
// The prompts below are ours, not deepeval's internal ones, so a score lands
// close to the Python original without being identical. Treat the series as the
// signal, not any single number, which is true of an LLM judge anyway.
type AnswerRelevancy struct {
	Client *openrouter.OpenRouter
	Model  string
}

// Result is one graded answer.
type Result struct {
	Score      float64  `json:"score"`
	Reason     string   `json:"reason"`
	Statements []string `json:"statements"`
	Irrelevant []string `json:"irrelevant"`
}

// Passed reports whether the score clears a threshold. 0.7 is deepeval's usual
// bar: below it an answer is carrying enough unrelated material to notice.
func (r Result) Passed(threshold float64) bool { return r.Score >= threshold }

// Score grades one answer against the question it was given.
func (m AnswerRelevancy) Score(ctx context.Context, input, output string) (Result, error) {
	if strings.TrimSpace(output) == "" {
		return Result{Score: 0, Reason: "the agent said nothing"}, nil
	}

	statements, err := m.statements(ctx, output)
	if err != nil {
		return Result{}, err
	}
	if len(statements) == 0 {
		return Result{Score: 0, Reason: "no statements could be extracted from the answer"}, nil
	}

	verdicts, err := m.verdicts(ctx, input, statements)
	if err != nil {
		return Result{}, err
	}

	var relevant int
	var irrelevant []string
	for i, v := range verdicts {
		// "idk" counts as relevant, following deepeval: the metric penalises
		// what is demonstrably off-topic, not what the judge found ambiguous.
		if strings.EqualFold(v.Verdict, "no") {
			irrelevant = append(irrelevant, statements[i])
			continue
		}
		relevant++
	}

	result := Result{
		Score:      float64(relevant) / float64(len(statements)),
		Statements: statements,
		Irrelevant: irrelevant,
	}
	result.Reason, err = m.reason(ctx, input, result.Score, irrelevant)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

const statementsPrompt = `Break the text below into its individual statements — each a single,
self-contained claim, instruction or piece of information. Keep the wording of the original as
closely as you can. Headings and formatting are not statements; the content under them is.

Text:
%s

Return ONLY JSON: {"statements": ["...", "..."]}`

func (m AnswerRelevancy) statements(ctx context.Context, output string) ([]string, error) {
	var parsed struct {
		Statements []string `json:"statements"`
	}
	if err := m.ask(ctx, fmt.Sprintf(statementsPrompt, output), &parsed); err != nil {
		return nil, err
	}
	kept := parsed.Statements[:0]
	for _, s := range parsed.Statements {
		if strings.TrimSpace(s) != "" {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

const verdictsPrompt = `For each numbered statement, decide whether it is relevant to addressing
the question below. Relevant is a low bar: a statement counts as "yes" if it is part of a useful
answer, not only if it is the answer.

"yes"  it addresses the question or any part of it — including a corrected sentence, one item in
       a list of corrections, an explanation of a correction, a supporting example of the right
       or wrong form, the name of the rule, and a citation of the source the rule came from
"no"   it is about something else entirely — a different topic, a rule nobody asked about,
       filler, an apology, or a remark that would be there whatever the question was
"idk"  you genuinely cannot tell

Judge relevance ONLY. Do not judge whether the statement is true, well written, complete or
necessary, and never mark something "no" for being short, formatted, or one piece of a longer
answer.

Question:
%s

The %d statements:
%s

Return ONLY JSON with exactly %d verdicts, one per numbered statement, each carrying that
statement's number:
{"verdicts": [{"index": 1, "verdict": "yes"}, {"index": 2, "verdict": "no", "reason": "why"}]}`

type verdict struct {
	Index   int    `json:"index"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// verdicts classifies every statement, and insists on getting one verdict per
// statement back.
//
// Each verdict carries the number of the statement it belongs to, because
// position alone is not reliable: asked for five verdicts on a formatted answer
// a model will occasionally return four, or six, and a list quietly off by one
// scores the wrong statements. Indices make a miscount detectable instead of
// silent — and a miscount is worth one retry before it becomes an error.
func (m AnswerRelevancy) verdicts(ctx context.Context, input string, statements []string) ([]verdict, error) {
	numbered := make([]string, len(statements))
	for i, s := range statements {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
	}
	prompt := fmt.Sprintf(verdictsPrompt, input, len(statements), strings.Join(numbered, "\n"), len(statements))

	var last error
	for attempt := 0; attempt < 2; attempt++ {
		var parsed struct {
			Verdicts []verdict `json:"verdicts"`
		}
		if err := m.ask(ctx, prompt, &parsed); err != nil {
			return nil, err
		}

		byIndex := make([]verdict, len(statements))
		var filled int
		for _, v := range parsed.Verdicts {
			i := v.Index - 1
			if i < 0 || i >= len(statements) || byIndex[i].Verdict != "" {
				continue // out of range, or a duplicate of one already placed
			}
			byIndex[i] = v
			filled++
		}
		if filled == len(statements) {
			return byIndex, nil
		}
		last = fmt.Errorf("judge returned %d usable verdicts for %d statements", filled, len(statements))
	}
	return nil, last
}

const reasonPrompt = `An answer scored %.2f for relevance to this question, where 1.00 means every
statement in it addressed the question.

Question:
%s

The statements judged off-topic:
%s

In ONE sentence, say why the score is what it is. Start with "The score is %.2f because".
Do not mention JSON, statements counts, or this instruction.

Return ONLY JSON: {"reason": "..."}`

func (m AnswerRelevancy) reason(ctx context.Context, input string, score float64, irrelevant []string) (string, error) {
	listed := "none — every statement addressed the question"
	if len(irrelevant) > 0 {
		listed = "- " + strings.Join(irrelevant, "\n- ")
	}
	var parsed struct {
		Reason string `json:"reason"`
	}
	prompt := fmt.Sprintf(reasonPrompt, score, input, listed, score)
	if err := m.ask(ctx, prompt, &parsed); err != nil {
		return "", err
	}
	return parsed.Reason, nil
}

// ask makes one judging call and unmarshals the JSON it comes back with.
//
// Temperature 0: a judge that disagrees with itself between runs is not a
// metric. There is no response_format here on purpose — OpenRouter passes it
// through to whichever provider serves the model, and support varies, so the
// prompt asks for JSON and jsonOnly cleans up what comes back.
func (m AnswerRelevancy) ask(ctx context.Context, prompt string, into any) error {
	zero := 0.0
	res, err := m.Client.Chat.Send(ctx, components.ChatRequest{
		Model:       openrouter.String(m.Model),
		Temperature: optionalnullable.From(&zero),
		Messages: []components.ChatMessages{
			components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(prompt),
			}),
		},
	}, nil)
	if err != nil {
		return err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return fmt.Errorf("the judge returned no choices")
	}
	text := messageText(res.ChatResult.Choices[0].Message)
	if err := json.Unmarshal([]byte(jsonOnly(text)), into); err != nil {
		return fmt.Errorf("the judge did not return usable JSON (%w): %s", err, truncate(text, 200))
	}
	return nil
}

func messageText(msg components.ChatAssistantMessage) string {
	if c, ok := msg.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}

var fence = regexp.MustCompile("(?s)```(?:json)?\\s*(.+?)```")

// jsonOnly strips a code fence or any prose around the object.
func jsonOnly(text string) string {
	if m := fence.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
