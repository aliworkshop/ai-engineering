package triage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// fake is a judgment model that answers whatever the test says it answers. It is
// the whole reason the Asker interface exists: the thing worth testing here is
// not the HTTP call, it is what the policy does with 0.49.
type fake struct {
	answers map[string]jev.Answer
	err     error

	keys     []string
	purposes []string
	states   []any
}

func (f *fake) Ask(_ context.Context, _, key, purpose string, state any,
	_ map[string]jev.Question) (jev.Result, error) {
	f.keys = append(f.keys, key)
	f.purposes = append(f.purposes, purpose)
	f.states = append(f.states, state)
	if f.err != nil {
		return jev.Result{}, f.err
	}
	return jev.Result{Answers: f.answers}, nil
}

func noul(p float64) jev.Answer { return jev.Answer{Type: jev.NoulType, Noul: p} }

func choice(pick string, confidence float64) jev.Answer {
	return jev.Answer{Type: jev.ChoiceType, Choice: pick, Confidence: confidence}
}

// The question's options and the enum the tools advertise have to be the same
// four categories. Two lists that must agree are a bug waiting for whoever edits
// one of them.
func TestCategoryOptionsMatchTheTools(t *testing.T) {
	criteria, ok := Category().Criteria.(map[string]string)
	if !ok {
		t.Fatalf("category criteria is %T, want a map of option to rubric", Category().Criteria)
	}
	for _, want := range tools.Categories {
		if _, ok := criteria[want]; !ok {
			t.Errorf("category question cannot answer %q", want)
		}
	}
	if len(criteria) != len(tools.Categories) {
		t.Errorf("category question has %d options, tools advertise %d",
			len(criteria), len(tools.Categories))
	}
}

func TestTriageQuestionsCoverEveryArticle(t *testing.T) {
	questions := TriageQuestions()
	if _, ok := questions["category"]; !ok {
		t.Error("no category question")
	}
	for _, topic := range Topics() {
		if _, ok := questions[ArticleKey(topic)]; !ok {
			t.Errorf("no question for the %s article", topic)
		}
	}
	if len(questions) != len(tools.KnowledgeBase)+1 {
		t.Errorf("asked %d questions, want one per article plus the category", len(questions))
	}
}

// The threshold is a >=, and an article that misses it by 0.01 does not reach
// the model. This is the test that would catch someone "tidying" it into a >.
func TestClassifyKeepsArticlesAtTheThreshold(t *testing.T) {
	model := &fake{answers: map[string]jev.Answer{
		"category":            choice("billing", 0.93),
		ArticleKey("billing"): noul(ArticleThreshold), // exactly at it: kept
		ArticleKey("refund"):  noul(ArticleThreshold - 0.01),
		ArticleKey("export"):  noul(0.9),
		ArticleKey("pricing"): noul(0),
	}}

	got, err := New(model, nil).Classify(context.Background(), "wf",
		Item{ID: "item-1", Kind: "customer_message", Text: "I was charged twice."})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if got.Category != "billing" || got.Confidence != 0.93 || got.LowConfidence {
		t.Errorf("triage = %+v, want billing at 0.93 and not flagged", got)
	}
	// Most relevant first.
	if want := []string{"export", "billing"}; !reflect.DeepEqual(topicsOf(got.Articles), want) {
		t.Errorf("articles = %v, want %v", topicsOf(got.Articles), want)
	}
	if got.Articles[0].Text != tools.KnowledgeBase["export"] {
		t.Error("an article came back without its text")
	}
	if len(model.purposes) != 1 || model.purposes[0] != "triage item-1" {
		t.Errorf("purposes = %v, want one labelled call", model.purposes)
	}
}

// A confidence below the line does not change the decision, only the warning
// that travels with it.
func TestClassifyFlagsLowConfidence(t *testing.T) {
	model := &fake{answers: map[string]jev.Answer{
		"category": choice("other", LowConfidence-0.01),
	}}

	got, err := New(model, nil).Classify(context.Background(), "wf", Item{ID: "item-9"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !got.LowConfidence {
		t.Errorf("confidence %.2f was not flagged as low", got.Confidence)
	}
	if got.Category != "other" {
		t.Errorf("category = %q — a low confidence must not change the answer", got.Category)
	}
}

func TestClassifyWithoutACategoryIsAnError(t *testing.T) {
	model := &fake{answers: map[string]jev.Answer{ArticleKey("billing"): noul(1)}}
	if _, err := New(model, nil).Classify(context.Background(), "wf", Item{ID: "item-1"}); err == nil {
		t.Fatal("Classify succeeded with no category answered")
	}
}

func TestClassifyPassesTheModelError(t *testing.T) {
	model := &fake{err: errors.New("429")}
	if _, err := New(model, nil).Classify(context.Background(), "wf", Item{ID: "item-1"}); err == nil {
		t.Fatal("Classify swallowed the model's error")
	}
}

// The four corners of the gate. Only the first may send.
func TestVerify(t *testing.T) {
	cases := []struct {
		name       string
		grounded   float64
		onTopic    float64
		pass       bool
		wantReason string
	}{
		{"good draft", 0.97, 0.99, true, "ok"},
		{"invented a price", 0.10, 0.99, false,
			"the draft states facts the KB articles do not support"},
		{"answered a different question", 0.98, 0.05, false,
			"the draft does not address the customer's request"},
		{"both", 0.1, 0.1, false,
			"the draft states facts the KB articles do not support; " +
				"the draft does not address the customer's request"},
		{"exactly at the threshold", GateThreshold, GateThreshold, true, "ok"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := &fake{answers: map[string]jev.Answer{
				GroundedKey: noul(c.grounded),
				OnTopicKey:  noul(c.onTopic),
			}}
			item := Item{ID: "item-3", Text: "Can you send pricing for 50 seats?"}
			result := Result{ItemID: item.ID, Articles: []Article{
				{Topic: "pricing", P: 0.99, Text: tools.KnowledgeBase["pricing"]},
			}}

			got, err := New(model, nil).Verify(context.Background(), "wf", "call_1", item, result, "a draft")
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Pass != c.pass {
				t.Errorf("pass = %v, want %v", got.Pass, c.pass)
			}
			if got.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, c.wantReason)
			}

			// The gate must judge the draft against the articles the drafter
			// was actually given, and nothing else.
			state, _ := model.states[0].(map[string]any)
			if state["draft"] != "a draft" || state["customer_message"] != item.Text {
				t.Errorf("state = %v, want the draft and the customer's message", state)
			}
			if articles, _ := state["articles"].([]string); len(articles) != 1 ||
				articles[0] != tools.KnowledgeBase["pricing"] {
				t.Errorf("articles in state = %v, want only the triaged one", state["articles"])
			}
		})
	}
}

// Fail loud, not closed. A zero would read as "definitely not grounded" and
// block a good reply for a reason nobody could find.
func TestVerifyWithoutAnAnswerIsAnError(t *testing.T) {
	model := &fake{answers: map[string]jev.Answer{GroundedKey: noul(0.99)}}
	_, err := New(model, nil).Verify(context.Background(), "wf", "call_1", Item{ID: "item-1"}, Result{}, "draft")
	if err == nil {
		t.Fatal("Verify decided without an on_topic answer")
	}
	if !strings.Contains(err.Error(), OnTopicKey) {
		t.Errorf("error = %q, want it to name the missing question", err)
	}
}

func TestParseItemsReadsTheSampleTask(t *testing.T) {
	got := ParseItems(agent.SampleTask)
	want := []Item{
		{ID: "item-1", Kind: "customer_message", Text: "I was charged twice and need help."},
		{ID: "item-2", Kind: "bug_report", Text: "The export button fails on Safari."},
		{ID: "item-3", Kind: "sales_request", Text: "Can you send pricing for 50 seats?"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseItems(SampleTask) =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseItemsFallsBackToOneItem(t *testing.T) {
	got := ParseItems("  the export button is broken  ")
	want := []Item{{ID: "item-1", Kind: "message", Text: "the export button is broken"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseItems = %+v, want %+v", got, want)
	}
}

func TestBriefing(t *testing.T) {
	items := []Item{{ID: "item-1"}, {ID: "item-2"}}
	byID := map[string]Result{
		"item-1": {Category: "billing", Confidence: 0.93, Articles: []Article{
			{Topic: "billing", P: 0.97, Text: "double charges drop off"},
		}},
		"item-2": {Category: "other", Confidence: 0.4, LowConfidence: true},
	}

	want := "- item-1: category=billing (confidence 0.93)\n" +
		"    article [billing]: double charges drop off\n" +
		"- item-2: category=other (confidence 0.40, LOW — be cautious)\n" +
		"    (no relevant articles)"
	if got := Briefing(items, byID); got != want {
		t.Errorf("Briefing() =\n%s\nwant\n%s", got, want)
	}
}
