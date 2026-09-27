package bench

import (
	"fmt"
	"math"
	"sort"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/triage"
)

// The state each case is presented as. It has to be the SHAPE the agent uses,
// or the benchmark measures a question the agent never asks: the questions
// refer to `item`, `kb.billing`, `draft` and `customer_message` by name, and a
// state with different field names is a different question.

func triageState(c TriageCase) map[string]any {
	return map[string]any{
		"item": triage.Item{ID: c.ID, Kind: "message", Text: c.Text},
		"kb":   tools.KnowledgeBase,
	}
}

func verifyState(c VerifyCase) map[string]any {
	articles := make([]string, 0, len(c.Topics))
	for _, topic := range c.Topics {
		articles = append(articles, tools.KnowledgeBase[topic])
	}
	return map[string]any{
		"customer_message": c.Customer,
		"articles":         articles,
		"draft":            c.Draft,
	}
}

// optionsOf lists a choice question's options, sorted, for the baseline's enum.
func optionsOf(q jev.Question) []string {
	criteria, ok := q.Criteria.(map[string]string)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(criteria))
	for option := range criteria {
		out = append(out, option)
	}
	sort.Strings(out)
	return out
}

func jsonSchema(schema map[string]any) *components.ResponseFormat {
	strict := true
	format := components.CreateResponseFormatJSONSchema(components.ChatFormatJSONSchemaConfig{
		Type: components.ChatFormatJSONSchemaConfigTypeJSONSchema,
		JSONSchema: components.ChatJSONSchemaConfig{
			Name:   "answers",
			Strict: optionalnullable.From(&strict),
			Schema: schema,
		},
	})
	return &format
}

func userMessage(text string) components.ChatMessages {
	return components.CreateChatMessagesUser(components.ChatUserMessage{
		Role:    components.ChatUserMessageRoleUser,
		Content: components.CreateChatUserMessageContentStr(text),
	})
}

func assistantText(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}

// ------------------------------------------------------------------ arithmetic

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func boolTo(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func square(f float64) float64 { return f * f }

func rate(xs []bool) float64 {
	if len(xs) == 0 {
		return 0
	}
	var n int
	for _, x := range xs {
		if x {
			n++
		}
	}
	return float64(n) / float64(len(xs))
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 1 // nothing to get wrong
	}
	return float64(n) / float64(d)
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func tokensOf(calls []Call) []float64 {
	out := make([]float64, len(calls))
	for i, c := range calls {
		out[i] = float64(c.Tokens)
	}
	return out
}

func latency(calls []Call) Latency {
	if len(calls) == 0 {
		return Latency{}
	}
	ms := make([]float64, len(calls))
	for i, c := range calls {
		ms[i] = float64(c.Latency.Milliseconds())
	}
	return Latency{
		P50:  int64(percentile(ms, 0.5)),
		P95:  int64(percentile(ms, 0.95)),
		Mean: int64(math.Round(meanOf(ms))),
		Max:  int64(percentile(ms, 1)),
	}
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

func pct(f float64) string { return fmt.Sprintf("%5.0f%%", f*100) }
