package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// Memory is three different things, and they are kept apart ON PURPOSE:
//
//	HISTORY  everything that happened       the event log and the workflow file
//	STATE    a running summary of old work  compacted working memory
//	CONTEXT  what the model sees THIS turn  assembled fresh, every pass
//
// Until now the three were one slice. Every message ever produced was history,
// was state, and was sent — so the bill for a turn grew with every turn before
// it, and a long enough task would not fit at all. Pulled apart, history keeps
// everything (it already did: that is what the checkpoints are), state keeps
// what still matters, and the context is built from state each time rather
// than appended to forever.

// Compaction is by TOKEN BUDGET, not turn count. A model can batch many tool
// calls into one turn, so a turn is a poor proxy for size.
//
// These are deliberately tiny, so compaction fires on a short demo task.
// Production sets them near the model's real window — Claude Code compacts at
// around 80% of it. The numbers are a toy; the shape is the real thing.
const (
	maxContextTokens  = 500 // over this, compact
	keepContextTokens = 200 // and peel old turns until under this
)

// Turn is one pass of the loop: an assistant message and the results of every
// tool it asked for. It is the unit compaction cuts at, because it is the
// smallest piece that can be removed whole — split a turn and you leave a tool
// call with no result, or a result with no call, and the next model call
// refuses the conversation.
type Turn []Msg

// estimateTokens is about four characters per token, which is plenty: the
// budget is a ceiling to stay under, not an invoice. A message that is only
// tool calls has no text, so it is measured as the JSON it will be sent as.
func estimateTokens(msgs []Msg) int {
	chars := 0
	for _, m := range msgs {
		if len(m.ToolCalls) == 0 {
			chars += len(m.Text)
			continue
		}
		raw, _ := json.Marshal(m)
		chars += len(raw)
	}
	return (chars + 3) / 4
}

func flatten(turns []Turn) []Msg {
	var out []Msg
	for _, t := range turns {
		out = append(out, t...)
	}
	return out
}

// buildContext assembles what the model sees this turn, from scratch.
//
// The order is the point. The standing prompt first, then the conversation this
// task follows on from, then the TASK ITSELF — pinned, and never summarized
// away, because a summary of the goal is a goal that has drifted. Then what
// has been compacted, then the recent turns verbatim.
func buildContext(seed []Msg, task, summary string, turns []Turn) []Msg {
	out := make([]Msg, 0, len(seed)+len(turns)*2+3)
	out = append(out, Msg{Role: "system", Text: SystemPrompt})
	out = append(out, seed...)
	out = append(out, Msg{Role: "user", Text: task})
	if summary != "" {
		out = append(out, Msg{Role: "system", Text: "Summary of earlier work so far:\n" + summary})
	}
	return append(out, flatten(turns)...)
}

// peel takes the oldest turns off until what is left fits the keep budget.
//
// The newest turn always stays, however large it is: it holds the results the
// model is about to read, and summarizing those away before it has seen them
// would be summarizing work nobody has looked at.
func peel(turns []Turn) (old, kept []Turn) {
	kept = turns
	for len(kept) > 1 && estimateTokens(flatten(kept)) > keepContextTokens {
		old = append(old, kept[0])
		kept = kept[1:]
	}
	return old, kept
}

// summaryPrompt is what the summarizer is told. The list of things to keep is
// what makes it safe: a draft id that falls out of the summary is a reply that
// cannot be sent, and an item marked sent that falls out is one sent twice.
const summaryPrompt = "You compress an agent's work log into a short running summary. " +
	"Preserve concrete facts: item ids, customer ids, categories, charge ids, " +
	"draft ids, amounts, and what was already sent. Be terse."

// summarize folds old turns into the running summary. It is a model call like
// any other, so the loop runs it as a checkpointed step: a crash never pays
// for the same summary twice, and a replay rebuilds exactly the state the
// crashed run had.
func (a *Agent) summarize(ctx context.Context, old []Turn, prior string) (string, error) {
	var log strings.Builder
	for _, m := range flatten(old) {
		text := m.Text
		if len(m.ToolCalls) > 0 {
			raw, _ := json.Marshal(m)
			text = string(raw)
		}
		fmt.Fprintf(&log, "%s: %s\n", m.Role, text)
	}
	transcript := log.String()
	if len(transcript) > 6000 {
		transcript = transcript[:6000]
	}
	if prior == "" {
		prior = "(none)"
	}

	res, err := a.client.Chat.Send(ctx, components.ChatRequest{
		Model: openrouter.String(a.model),
		Messages: toSDK([]Msg{
			{Role: "system", Text: summaryPrompt},
			{Role: "user", Text: "Prior summary:\n" + prior +
				"\n\nFold in this newer work:\n" + transcript +
				"\n\nReturn the updated summary."},
		}),
	}, nil)
	if err != nil {
		return "", err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return "", fmt.Errorf("summarizer returned no choices")
	}
	return assistantText(res.ChatResult.Choices[0].Message), nil
}
