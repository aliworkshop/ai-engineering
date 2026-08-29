package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// SearchKnowledge is RAG: a private corpus the model can search, and the third
// way of getting information in front of it — a system prompt carries what the
// agent ALWAYS needs, web search fetches fresh public facts, and this fetches
// the house rule book on demand.
//
// Retrieval here is TF-IDF cosine over whole documents rather than embeddings.
// The two-step shape is the same as a hosted vector store's — turn the query
// into a vector, rank documents by cosine similarity, return the best few with
// their scores — and so is the tool's interface, which is the part the agent
// sees. Swapping in an embeddings API later changes vectorize() and nothing
// else.
//
// Whole documents, no chunking: the corpus is written as 200-500 word rule
// files, which is small enough to embed or index whole. Longer material would
// need splitting first.
type SearchKnowledge struct {
	// Dir holds the corpus: one markdown file per rule, named after it.
	Dir string

	once  sync.Once
	index index
	err   error
}

// KnowledgeTool is the advertised name, so a roster can name it without
// spelling it out again.
const KnowledgeTool = "search_knowledge"

func (*SearchKnowledge) Spec() components.ChatFunctionTool {
	return defineTool(KnowledgeTool,
		"Search the private grammar reference for the rule that covers a question. "+
			"Use it BEFORE explaining or correcting anything where the exact rule matters — "+
			"agreement, articles, tenses, prepositions, punctuation, conditionals, word order. "+
			"Returns the best-matching reference documents, each with a similarity score. "+
			"The score ranks these results against each other; whether the reference actually "+
			"covers the question is something you decide by reading the text that comes back.",
		`{"type":"object","properties":{"query":{"type":"string","description":"What to look up, in words a rule book would use, e.g. 'present perfect vs past simple' or 'comma before and'"}},"required":["query"]}`)
}

func (t *SearchKnowledge) Run(_ context.Context, args string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	t.once.Do(func() { t.index, t.err = loadCorpus(t.Dir) })
	if t.err != nil {
		return "", t.err
	}

	hits := t.index.search(a.Query, topK)
	if len(hits) == 0 {
		return "Nothing in the reference matches that. Answer from your own knowledge, and say so.", nil
	}
	out, err := json.Marshal(hits)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// topK is how many documents come back.
//
// One is brittle — if the top match is wrong there is nothing to fall back on.
// Five is noise on a corpus this size. Three, with the score attached so the
// model can judge whether a match is even relevant, is the useful middle.
const topK = 3

// Hit is one retrieved document. The score travels with it deliberately: a
// retriever that hands back its best guess without saying how good it was
// invites the model to treat a 0.05 match as an authority.
//
// It is a within-ranking signal, not an absolute one. Cosine against a whole
// 300-word document is small for any short query — "a vs an" scores 0.13 on
// exactly the right file — so "is this relevant?" is answered by reading the
// text, and the score only says which of the three came closest.
type Hit struct {
	Source  string  `json:"source"`
	Score   float64 `json:"score"`
	Content string  `json:"content"`
}

// index is the searchable corpus: every document as a normalized TF-IDF vector,
// plus the IDF weights a query is scored against.
type index struct {
	docs []indexed
	idf  map[string]float64
}

type indexed struct {
	source string
	text   string
	vector map[string]float64 // L2-normalized, so cosine is a plain dot product
}

// loadCorpus reads every markdown file in dir and indexes it. An empty or
// missing directory is an error rather than an empty index: an agent told to
// search a reference that isn't there should say so, not quietly answer from
// memory while claiming to have looked.
func loadCorpus(dir string) (index, error) {
	if dir == "" {
		dir = "corpus"
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return index{}, err
	}
	if len(paths) == 0 {
		return index{}, fmt.Errorf("no reference documents in %s", dir)
	}
	sort.Strings(paths)

	var (
		raw  []indexed
		docs = make([]map[string]float64, 0, len(paths))
		seen = make(map[string]int, 1024) // term -> documents containing it
	)
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return index{}, err
		}
		source := strings.TrimSuffix(filepath.Base(path), ".md")

		// The file name is part of the document's text. It is the closest thing
		// the corpus has to a title, and a query that names the topic exactly
		// ("subject verb agreement") should find the file called that.
		counts := count(tokenize(source + " " + string(b)))
		docs = append(docs, counts)
		raw = append(raw, indexed{source: source, text: string(b)})
		for term := range counts {
			seen[term]++
		}
	}

	// Smoothed IDF. A term in every document scores ~0, which is what keeps
	// "the" from deciding the ranking — without a stopword list, which this
	// corpus specifically must not have: in a grammar reference the function
	// words ARE the subject matter.
	n := float64(len(raw))
	idf := make(map[string]float64, len(seen))
	for term, df := range seen {
		idf[term] = math.Log(1 + n/float64(df))
	}
	for i, counts := range docs {
		raw[i].vector = weigh(counts, idf)
	}
	return index{docs: raw, idf: idf}, nil
}

// search ranks every document against the query by cosine similarity.
func (ix index) search(query string, k int) []Hit {
	q := weigh(count(tokenize(query)), ix.idf)
	if len(q) == 0 {
		return nil
	}

	hits := make([]Hit, 0, len(ix.docs))
	for _, d := range ix.docs {
		score := cosine(q, d.vector)
		if score <= 0 {
			continue
		}
		hits = append(hits, Hit{
			Source:  d.source,
			Score:   math.Round(score*1000) / 1000,
			Content: d.text,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// tokenize lowercases and splits on anything that isn't a letter or a digit.
// Nothing is dropped and nothing is stemmed: this corpus is about words like
// "a", "an", "the" and "since", and a stopword list would delete the answers.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 || f == "a" || f == "i" {
			out = append(out, f)
		}
	}
	return out
}

func count(terms []string) map[string]float64 {
	counts := make(map[string]float64, len(terms))
	for _, t := range terms {
		counts[t]++
	}
	return counts
}

// weigh turns term counts into an L2-normalized TF-IDF vector. Sublinear term
// frequency (1 + log tf) keeps a word repeated twenty times in one document
// from dominating it.
func weigh(counts map[string]float64, idf map[string]float64) map[string]float64 {
	vec := make(map[string]float64, len(counts))
	var norm float64
	for term, tf := range counts {
		w, ok := idf[term]
		if !ok || w == 0 {
			continue // unknown to the corpus, or common to every document
		}
		v := (1 + math.Log(tf)) * w
		vec[term] = v
		norm += v * v
	}
	if norm == 0 {
		return nil
	}
	norm = math.Sqrt(norm)
	for term := range vec {
		vec[term] /= norm
	}
	return vec
}

// cosine of two normalized vectors is their dot product. Iterating the shorter
// side keeps a one-line query from walking a whole document's vocabulary.
func cosine(a, b map[string]float64) float64 {
	if len(b) < len(a) {
		a, b = b, a
	}
	var dot float64
	for term, v := range a {
		dot += v * b[term]
	}
	return dot
}
