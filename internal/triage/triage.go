// Package triage is the judgment half of the agent: the three jobs that moved
// off the language model and onto a System One model, and — more importantly —
// the code that decides what to do with the probabilities they come back with.
//
// That second half is the whole point. The model says an article is relevant
// with probability 0.94 and that a draft is grounded with probability 0.31. It
// does not say to include the article or to block the send. Those are POLICY,
// they are three constants at the top of this file, and they are in Go where you
// can read them, test them, sweep them, and change one without touching a prompt.
//
// It knows nothing about durability. The runtime wraps these calls in
// checkpointed steps; this package just answers questions, which is what lets
// the benchmark import it without starting an engine.
package triage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// The policy. Three numbers, and every decision this package makes is one of
// them compared against a probability.
//
// They are separate constants even though two of them are 0.5, because they
// answer different questions and they will not move together. Showing a model
// one article too many costs a few tokens; letting one wrong price reach a
// customer costs a customer. The benchmark's threshold sweep is how you find
// out what each one should be rather than arguing about it.
const (
	// ArticleThreshold is how sure we have to be that an article helps before
	// it goes in the briefing.
	ArticleThreshold = 0.5

	// GateThreshold is how sure we have to be that a draft is grounded and on
	// topic before it may be sent.
	GateThreshold = 0.5

	// LowConfidence flags a category the model was not sure about. It does not
	// change what happens — it is written into the briefing so the model
	// drafting the reply knows to be careful, which is the cheapest possible
	// use of a number the old classifier never produced.
	LowConfidence = 0.6
)

// Article is one knowledge-base entry the model judged relevant, and how
// relevant it judged it.
type Article struct {
	Topic string  `json:"topic"`
	P     float64 `json:"p"`
	Text  string  `json:"text"`
}

// Result is one item, triaged: what it is about, how sure we are, and what the
// reply may draw its facts from.
type Result struct {
	ItemID        string    `json:"item_id"`
	Category      string    `json:"category"`
	Confidence    float64   `json:"confidence"`
	LowConfidence bool      `json:"low_confidence"`
	Articles      []Article `json:"articles"`
}

// Verdict is the gate's answer. Reason is written for the MODEL to read: when a
// send is blocked it goes back as the tool result, and a reason it can act on is
// the difference between a redraft and the same draft sent again.
type Verdict struct {
	Pass     bool    `json:"pass"`
	Reason   string  `json:"reason"`
	Grounded float64 `json:"grounded"`
	OnTopic  float64 `json:"on_topic"`
}

// Asker is what this package needs from a judgment model: one call, many
// questions, typed answers.
//
// Declared here and satisfied by *jev.Client — the same arrangement as
// agent.ToolBox. It is also what makes every test below run without a network:
// a fake Asker that returns fixed probabilities is four lines, and the thing
// worth testing is not the HTTP call, it is what the thresholds do with 0.49.
type Asker interface {
	Ask(ctx context.Context, workflow, key, purpose string, state any,
		questions map[string]jev.Question) (jev.Result, error)
}

// Triager runs the three jobs.
type Triager struct {
	ask Asker
	bus events.Emitter
}

// New wires a triager to a model and an event stream. A nil bus is a silent one.
func New(ask Asker, bus events.Emitter) *Triager {
	return &Triager{ask: ask, bus: bus}
}

// Classify is jobs 1 and 2: what is this item, and which articles help with it.
// One request, one round trip, every question answered against the same state.
func (t *Triager) Classify(ctx context.Context, workflow string, item Item) (Result, error) {
	state := map[string]any{"item": item, "kb": tools.KnowledgeBase}

	answered, err := t.ask.Ask(ctx, workflow, "triage-"+item.ID, "triage "+item.ID,
		state, TriageQuestions())
	if err != nil {
		return Result{}, err
	}

	category, ok := answered.Answers["category"]
	if !ok {
		return Result{}, fmt.Errorf("triage %s: no category in the answer", item.ID)
	}

	// Probabilities in, decisions out. Everything from here down is ours.
	result := Result{
		ItemID:        item.ID,
		Category:      category.Choice,
		Confidence:    category.Confidence,
		LowConfidence: category.Confidence < LowConfidence,
	}
	for _, topic := range Topics() {
		p := answered.Answers[ArticleKey(topic)].Noul
		if p >= ArticleThreshold {
			result.Articles = append(result.Articles,
				Article{Topic: topic, P: p, Text: tools.KnowledgeBase[topic]})
		}
	}
	// Most relevant first, and topic-ordered on a tie, so the briefing a model
	// reads is the same briefing on a replay.
	sort.SliceStable(result.Articles, func(i, j int) bool {
		if result.Articles[i].P != result.Articles[j].P {
			return result.Articles[i].P > result.Articles[j].P
		}
		return result.Articles[i].Topic < result.Articles[j].Topic
	})

	events.Emit(t.bus, events.Event{
		Type: events.JevTriaged, Workflow: workflow, Name: item.ID,
		Output: fmt.Sprintf("category=%s confidence=%.2f%s articles=[%s] (keep>=%.2f, low<%.2f)",
			result.Category, result.Confidence, lowNote(result.LowConfidence),
			strings.Join(topicsOf(result.Articles), " "), ArticleThreshold, LowConfidence),
	})
	return result, nil
}

// Verify is job 3: the check that stands between a draft and the customer.
//
// It is handed only the articles that survived triage, which is what makes
// "grounded" a meaningful question: grounded in WHAT is the whole of it, and the
// answer has to be the same set of facts the drafting model was given.
// attempt names the send this verdict is about — the model's own id for the
// tool call it is trying to make. It is what tells a repeated judgment (the
// same send, judged twice, which durable execution exists to prevent) from a
// second one (a redraft, judged again, which is the gate working).
func (t *Triager) Verify(ctx context.Context, workflow, attempt string, item Item, r Result, draft string) (Verdict, error) {
	articles := make([]string, 0, len(r.Articles))
	for _, a := range r.Articles {
		articles = append(articles, a.Text)
	}
	state := map[string]any{
		"customer_message": item.Text,
		"articles":         articles,
		"draft":            draft,
	}

	answered, err := t.ask.Ask(ctx, workflow, "verify-"+attempt, "verify "+item.ID,
		state, VerifyQuestions())
	if err != nil {
		return Verdict{}, err
	}

	// A gate that cannot evaluate must not decide. Missing answers are an
	// error rather than a silent zero: a zero would read as "definitely not
	// grounded" and block a perfectly good reply for a reason nobody could
	// find.
	grounded, ok := answered.Answers[GroundedKey]
	if !ok {
		return Verdict{}, fmt.Errorf("verify %s: no %s in the answer", item.ID, GroundedKey)
	}
	onTopic, ok := answered.Answers[OnTopicKey]
	if !ok {
		return Verdict{}, fmt.Errorf("verify %s: no %s in the answer", item.ID, OnTopicKey)
	}

	verdict := Verdict{Grounded: grounded.Noul, OnTopic: onTopic.Noul}
	verdict.Pass = verdict.Grounded >= GateThreshold && verdict.OnTopic >= GateThreshold

	var reasons []string
	if verdict.Grounded < GateThreshold {
		reasons = append(reasons, "the draft states facts the KB articles do not support")
	}
	if verdict.OnTopic < GateThreshold {
		reasons = append(reasons, "the draft does not address the customer's request")
	}
	verdict.Reason = "ok"
	if len(reasons) > 0 {
		verdict.Reason = strings.Join(reasons, "; ")
	}

	events.Emit(t.bus, events.Event{
		Type: events.JevGate, Workflow: workflow, Name: item.ID,
		Output: fmt.Sprintf("%s grounded=%.2f on_topic=%.2f (>=%.2f)%s",
			verdictWord(verdict.Pass), verdict.Grounded, verdict.OnTopic, GateThreshold,
			blockNote(verdict)),
	})
	return verdict, nil
}

// Briefing is what the drafting model is told about the items it is about to
// answer: the category, how sure we are, and the articles it may use.
//
// It is assembled from checkpointed triage results rather than re-derived, so a
// recovered run rebuilds the identical briefing and the identical prompt.
func Briefing(items []Item, byID map[string]Result) string {
	var b strings.Builder
	for i, item := range items {
		r, ok := byID[item.ID]
		if !ok {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s: category=%s (confidence %.2f%s)",
			item.ID, r.Category, r.Confidence, lowNote(r.LowConfidence))
		if len(r.Articles) == 0 {
			// Saying so beats saying nothing. A model shown an item with no
			// articles and no comment invents the facts it is missing; one
			// told there are none tends to say a specialist will follow up.
			b.WriteString("\n    (no relevant articles)")
			continue
		}
		for _, a := range r.Articles {
			fmt.Fprintf(&b, "\n    article [%s]: %s", a.Topic, a.Text)
		}
	}
	return b.String()
}

func lowNote(low bool) string {
	if low {
		return ", LOW — be cautious"
	}
	return ""
}

func verdictWord(pass bool) string {
	if pass {
		return "pass"
	}
	return "blocked"
}

func blockNote(v Verdict) string {
	if v.Pass {
		return ""
	}
	return " — " + v.Reason
}

func topicsOf(articles []Article) []string {
	out := make([]string, 0, len(articles))
	for _, a := range articles {
		out = append(out, a.Topic)
	}
	return out
}
