package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// corpusDir is the real reference, not a fixture. The thing worth testing is
// whether a question a learner would actually ask lands on the right rule file,
// and a two-document fixture cannot fail that way.
const corpusDir = "../../corpus"

func search(t *testing.T, query string) []Hit {
	t.Helper()
	tool := &SearchKnowledge{Dir: corpusDir}
	out, err := tool.Run(context.Background(), jsonArgs(t, map[string]any{"query": query}))
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	var hits []Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("search %q returned %q: %v", query, out, err)
	}
	return hits
}

// The retrieval that matters: the question a learner asks reaches the file that
// answers it. Ranking is the whole tool — everything else is plumbing.
func TestSearchKnowledgeFindsTheRightRule(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"should I write a hour or an hour", "articles"},
		{"present perfect versus past simple", "verb-tenses"},
		{"is a comma splice wrong", "commas-and-punctuation"},
		{"if I had known I would have called", "conditionals"},
		{"neither of the answers is or are correct", "subject-verb-agreement"},
		{"in on at for times and dates", "prepositions"},
		{"dangling modifier and parallel lists", "modifiers-and-word-order"},
		{"plural of information and advice", "common-l2-errors"},
	}

	for _, c := range cases {
		hits := search(t, c.query)
		if len(hits) == 0 {
			t.Errorf("%q found nothing", c.query)
			continue
		}
		if hits[0].Source != c.want {
			t.Errorf("%q ranked %q first (%.3f), want %q; full ranking: %s",
				c.query, hits[0].Source, hits[0].Score, c.want, ranking(hits))
		}
	}
}

// topK, scores, and their order are part of the tool's contract: the model is
// told to judge relevance by the score, so a score that doesn't rank means the
// instruction is a lie.
func TestSearchKnowledgeReturnsRankedTopK(t *testing.T) {
	hits := search(t, "when should I use the present perfect")

	if len(hits) == 0 || len(hits) > topK {
		t.Fatalf("got %d hits, want between 1 and %d", len(hits), topK)
	}
	for i, h := range hits {
		if h.Score <= 0 || h.Score > 1 {
			t.Errorf("hit %d has score %v, want a cosine in (0,1]", i, h.Score)
		}
		if h.Content == "" {
			t.Errorf("hit %d (%s) came back without its text", i, h.Source)
		}
		if i > 0 && hits[i-1].Score < h.Score {
			t.Errorf("hits are not ranked: %s (%.3f) before %s (%.3f)",
				hits[i-1].Source, hits[i-1].Score, h.Source, h.Score)
		}
	}
}

// A query about something the corpus does not cover must not come back looking
// authoritative. It either finds nothing or scores low enough for the model to
// see that it did.
func TestSearchKnowledgeScoresAnUnrelatedQueryLow(t *testing.T) {
	tool := &SearchKnowledge{Dir: corpusDir}
	out, err := tool.Run(context.Background(), jsonArgs(t, map[string]any{
		"query": "kubernetes pod networking",
	}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.HasPrefix(out, "[") {
		return // nothing matched at all, which is the clearest answer of the two
	}
	var hits []Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(hits) > 0 && hits[0].Score > 0.2 {
		t.Fatalf("an off-topic query scored %.3f on %s — the model would trust it",
			hits[0].Score, hits[0].Source)
	}
}

// A missing corpus is an error, not an empty result: an agent whose prompt says
// "search the reference first" must not be able to report that it looked and
// found nothing when there was nothing to look in.
func TestSearchKnowledgeWithoutACorpusFails(t *testing.T) {
	tool := &SearchKnowledge{Dir: t.TempDir()}
	if _, err := tool.Run(context.Background(), `{"query":"articles"}`); err == nil {
		t.Fatal("an empty corpus directory should be an error")
	}
}

func TestSearchKnowledgeRejectsAnEmptyQuery(t *testing.T) {
	tool := &SearchKnowledge{Dir: corpusDir}
	if _, err := tool.Run(context.Background(), `{"query":"  "}`); err == nil {
		t.Fatal("an empty query should be an error")
	}
}

func ranking(hits []Hit) string {
	var b strings.Builder
	for i, h := range hits {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(h.Source)
	}
	return b.String()
}
