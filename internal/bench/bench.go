// Package bench measures the judgment model on the three jobs it does in the
// agent, and compares it with the obvious alternative: asking a language model
// the SAME questions through structured output.
//
// Two things make the comparison honest. Both systems are handed the same state
// and the same questions — imported from internal/triage, so what is graded is
// exactly what the agent asks, not a paraphrase of it. And both are scored
// against hand labels in dataset.go rather than against each other.
//
// The interesting column is not accuracy. It is that one of these systems
// returns a probability and the other returns a label, and only one of them can
// therefore answer "what would happen if we moved the threshold?" — which is
// the question you actually have when the gate blocks a reply it should not
// have. That is what the sweep at the bottom is for.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/triage"
)

// System is one of the two things being measured.
type System string

const (
	Jev System = "jev"
	LLM System = "llm"
)

// Options is what a run needs.
type Options struct {
	Jev    *jev.Client
	Client *openrouter.OpenRouter
	Model  string // the language model standing in as the baseline

	Systems     []System
	Concurrency int
	Dir         string // where results are written

	Progress func(done, total int, label string)
}

// Call is one system's answer to one case, normalised so the two are
// comparable: a category, and a probability per yes/no question — which for the
// language model is only ever 0 or 1, because a label is not a probability.
// That flattening is itself a finding, and the sweep is where it shows up.
type Call struct {
	Category  string             `json:"category,omitempty"`
	Nouls     map[string]float64 `json:"nouls"`
	Latency   time.Duration      `json:"-"`
	LatencyMS int64              `json:"latency_ms"`
	Tokens    int                `json:"tokens"`
}

type asker func(context.Context, any, map[string]jev.Question) (Call, error)

// ---------------------------------------------------------------- the systems

func (o Options) jevAsker() asker {
	return func(ctx context.Context, state any, questions map[string]jev.Question) (Call, error) {
		result, err := o.Jev.Call(ctx, state, questions)
		if err != nil {
			return Call{}, err
		}
		call := Call{Nouls: map[string]float64{}, Latency: result.Latency,
			Tokens: result.Usage.InputTokens + result.Usage.OutputTokens}
		for id, a := range result.Answers {
			if a.Type == jev.ChoiceType {
				call.Category = a.Choice
				continue
			}
			call.Nouls[id] = a.Noul
		}
		return call, nil
	}
}

// llmAsker asks the same questions of a language model, as a strict JSON schema:
// every choice becomes an enum field and every noul a boolean one. This is the
// fair version of the baseline — not "write me a paragraph and I will parse it",
// but the best structured-output shape the question admits.
func (o Options) llmAsker() asker {
	return func(ctx context.Context, state any, questions map[string]jev.Question) (Call, error) {
		properties := map[string]any{}
		required := make([]string, 0, len(questions))
		for id, q := range questions {
			if q.Type == jev.ChoiceType {
				properties[id] = map[string]any{"type": "string", "enum": optionsOf(q)}
			} else {
				properties[id] = map[string]any{"type": "boolean"}
			}
			required = append(required, id)
		}
		sort.Strings(required)

		stateJSON, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return Call{}, err
		}
		prompt := "STATE:\n" + string(stateJSON) + "\n\nQUESTIONS:\n"
		for _, id := range required {
			q := questions[id]
			prompt += fmt.Sprintf("- %s (%s): %v\n", id, q.Type, q.Instructions)
			if q.Criteria != nil {
				criteria, _ := json.Marshal(q.Criteria)
				prompt += "  criteria: " + string(criteria) + "\n"
			}
		}

		started := time.Now()
		res, err := o.Client.Chat.Send(ctx, components.ChatRequest{
			Model:       openrouter.String(o.Model),
			Temperature: optionalnullable.From(openrouter.Float64(0)),
			Messages: []components.ChatMessages{
				userMessage("Answer every question about STATE. Backticked paths refer to " +
					"fields of STATE. For boolean questions answer true or false.\n\n" + prompt),
			},
			ResponseFormat: jsonSchema(map[string]any{
				"type": "object", "properties": properties,
				"required": required, "additionalProperties": false,
			}),
		}, nil)
		if err != nil {
			return Call{}, err
		}
		if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
			return Call{}, fmt.Errorf("bench: the model returned no choices")
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(assistantText(res.ChatResult.Choices[0].Message)), &raw); err != nil {
			return Call{}, fmt.Errorf("bench: the model's JSON did not parse: %w", err)
		}

		call := Call{Nouls: map[string]float64{}, Latency: time.Since(started)}
		if res.ChatResult.Usage != nil {
			call.Tokens = int(res.ChatResult.Usage.TotalTokens)
		}
		for id, v := range raw {
			switch value := v.(type) {
			case bool:
				// A label, widened into the probability slot. It is the whole
				// difference: 0 or 1, never 0.31.
				call.Nouls[id] = boolTo(value)
			case string:
				call.Category = value
			}
		}
		return call, nil
	}
}

// ---------------------------------------------------------------- the scoring

// TriageScore grades one item against its labels.
type TriageScore struct {
	Category   string   `json:"category"`
	CategoryOK bool     `json:"category_ok"`
	Picked     []string `json:"picked"`
	TP         int      `json:"tp"`
	FP         int      `json:"fp"`
	FN         int      `json:"fn"`
	ArticlesOK bool     `json:"articles_ok"`
}

func scoreTriage(c TriageCase, call Call, threshold float64) TriageScore {
	score := TriageScore{Category: call.Category}
	score.CategoryOK = call.Category == c.Category || contains(c.AlsoOK, call.Category)

	allowed := append(append([]string{}, c.Need...), c.OK...)
	for _, topic := range triage.Topics() {
		if call.Nouls[triage.ArticleKey(topic)] < threshold {
			continue
		}
		score.Picked = append(score.Picked, topic)
		switch {
		case contains(c.Need, topic):
			score.TP++
		case !contains(allowed, topic):
			// Outside need ∪ ok: an article the reply had no business seeing.
			score.FP++
		}
	}
	for _, topic := range c.Need {
		if !contains(score.Picked, topic) {
			score.FN++
		}
	}
	score.ArticlesOK = score.FP == 0 && score.FN == 0
	return score
}

// VerifyScore grades one draft against its labels. DecisionOK is the one that
// matters: the two probabilities can both be off and still reach the right
// verdict, and it is the verdict that decides whether a customer is emailed.
type VerifyScore struct {
	Grounded    bool `json:"grounded"`
	OnTopic     bool `json:"on_topic"`
	GroundedOK  bool `json:"grounded_ok"`
	OnTopicOK   bool `json:"on_topic_ok"`
	Blocked     bool `json:"blocked"`
	ShouldBlock bool `json:"should_block"`
	DecisionOK  bool `json:"decision_ok"`
}

func scoreVerify(c VerifyCase, call Call, threshold float64) VerifyScore {
	score := VerifyScore{
		Grounded: call.Nouls[triage.GroundedKey] >= threshold,
		OnTopic:  call.Nouls[triage.OnTopicKey] >= threshold,
	}
	score.GroundedOK = score.Grounded == c.Grounded
	score.OnTopicOK = score.OnTopic == c.OnTopic
	score.Blocked = !score.Grounded || !score.OnTopic
	score.ShouldBlock = !c.Grounded || !c.OnTopic
	score.DecisionOK = score.Blocked == score.ShouldBlock
	return score
}

// ---------------------------------------------------------------- the results

// Accuracy is how often a system agreed with a human.
type Accuracy struct {
	Category          float64 `json:"category"`
	ArticlesExact     float64 `json:"articles_exact"`
	ArticlesPrecision float64 `json:"articles_precision"`
	ArticlesRecall    float64 `json:"articles_recall"`
	Grounded          float64 `json:"grounded"`
	OnTopic           float64 `json:"on_topic"`
	GateDecision      float64 `json:"gate_decision"`
	BadDraftsCaught   float64 `json:"bad_drafts_caught"`
	GoodDraftsPassed  float64 `json:"good_drafts_passed"`
}

// Latency in milliseconds. p95 rather than a mean, because the number that
// decides whether a gate is affordable is the slow call, not the typical one.
type Latency struct {
	P50  int64 `json:"p50"`
	P95  int64 `json:"p95"`
	Mean int64 `json:"mean"`
	Max  int64 `json:"max"`
}

// Summary is one system's whole result.
type Summary struct {
	Accuracy Accuracy           `json:"accuracy"`
	Latency  map[string]Latency `json:"latency_ms"`
	Tokens   map[string]float64 `json:"tokens"`
}

// SweepRow is what the thresholds would have done. Only a system that returns
// probabilities can produce this table, which is the point of printing it.
type SweepRow struct {
	Threshold        float64 `json:"threshold"`
	ArticlesExact    float64 `json:"articles_exact"`
	GateDecision     float64 `json:"gate_decision"`
	BadDraftsCaught  float64 `json:"bad_drafts_caught"`
	GoodDraftsPassed float64 `json:"good_drafts_passed"`
}

// Brier is the mean squared error of the probabilities themselves: 0 is
// perfect, 0.25 is a coin flip. It asks a harder question than accuracy — not
// "was it right" but "was it right by the margin it claimed" — and it is the
// number that tells you whether a threshold means anything.
type Brier struct {
	Articles float64 `json:"articles"`
	Gate     float64 `json:"gate"`
}

// Result is one whole run, and it is written to disk as JSON so two runs can be
// compared without re-running either.
type Result struct {
	RanAt      string             `json:"ran_at"`
	Systems    []System           `json:"systems"`
	LLMModel   string             `json:"llm_model"`
	JevModel   string             `json:"jev_model"`
	Thresholds map[string]float64 `json:"thresholds"`
	N          map[string]int     `json:"n"`

	Summary map[System]Summary `json:"summary"`
	Sweep   []SweepRow         `json:"sweep,omitempty"`
	Brier   *Brier             `json:"brier,omitempty"`

	Triage map[System][]TriageScore `json:"triage"`
	Verify map[System][]VerifyScore `json:"verify"`

	Path string `json:"-"`
}

// ----------------------------------------------------------------- the runner

// Run measures every system over every case and writes the result.
func Run(ctx context.Context, opt Options) (Result, error) {
	if len(opt.Systems) == 0 {
		opt.Systems = []System{Jev, LLM}
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 4
	}
	if opt.Dir == "" {
		opt.Dir = filepath.Join(".harness", "bench")
	}

	askers := map[System]asker{}
	for _, s := range opt.Systems {
		switch s {
		case Jev:
			if opt.Jev == nil {
				return Result{}, fmt.Errorf("bench: no judgment model configured")
			}
			askers[s] = opt.jevAsker()
		case LLM:
			if opt.Client == nil {
				return Result{}, fmt.Errorf("bench: no language model configured")
			}
			askers[s] = opt.llmAsker()
		default:
			return Result{}, fmt.Errorf("bench: unknown system %q", s)
		}
	}

	total := len(opt.Systems) * (len(Triage) + len(Verify))
	var done int
	var progressMu sync.Mutex
	tick := func(label string) {
		if opt.Progress == nil {
			return
		}
		progressMu.Lock()
		done++
		opt.Progress(done, total, label)
		progressMu.Unlock()
	}

	triageCalls := map[System][]Call{}
	verifyCalls := map[System][]Call{}
	for _, system := range opt.Systems {
		ask := askers[system]

		// One unmeasured call first: the first request of a run pays for DNS
		// and a TLS handshake, and charging that to case v01 would make the
		// first case in the list look like the slowest question in the set.
		_, _ = ask(ctx, verifyState(Verify[0]), triage.VerifyQuestions())

		calls, err := pool(ctx, opt.Concurrency, len(Triage), func(ctx context.Context, i int) (Call, error) {
			call, err := ask(ctx, triageState(Triage[i]), triage.TriageQuestions())
			tick(string(system) + ": " + Triage[i].ID)
			return call, err
		})
		if err != nil {
			return Result{}, err
		}
		triageCalls[system] = calls

		calls, err = pool(ctx, opt.Concurrency, len(Verify), func(ctx context.Context, i int) (Call, error) {
			call, err := ask(ctx, verifyState(Verify[i]), triage.VerifyQuestions())
			tick(string(system) + ": " + Verify[i].ID)
			return call, err
		})
		if err != nil {
			return Result{}, err
		}
		verifyCalls[system] = calls
	}

	result := Result{
		RanAt:    time.Now().Format(time.RFC3339),
		Systems:  opt.Systems,
		LLMModel: opt.Model,
		JevModel: jev.Model,
		Thresholds: map[string]float64{
			"article": triage.ArticleThreshold, "gate": triage.GateThreshold,
		},
		N:       map[string]int{"triage": len(Triage), "verify": len(Verify)},
		Summary: map[System]Summary{},
		Triage:  map[System][]TriageScore{},
		Verify:  map[System][]VerifyScore{},
	}

	for _, system := range opt.Systems {
		triaged := make([]TriageScore, len(Triage))
		for i, c := range Triage {
			triaged[i] = scoreTriage(c, triageCalls[system][i], triage.ArticleThreshold)
		}
		verified := make([]VerifyScore, len(Verify))
		for i, c := range Verify {
			verified[i] = scoreVerify(c, verifyCalls[system][i], triage.GateThreshold)
		}
		result.Triage[system] = triaged
		result.Verify[system] = verified
		result.Summary[system] = summarise(triaged, verified,
			triageCalls[system], verifyCalls[system])
	}

	if _, ok := triageCalls[Jev]; ok {
		result.Sweep = sweep(triageCalls[Jev], verifyCalls[Jev])
		brier := brier(triageCalls[Jev], verifyCalls[Jev])
		result.Brier = &brier
	}

	path, err := write(opt.Dir, result)
	if err != nil {
		return result, err
	}
	result.Path = path
	return result, nil
}

func summarise(triaged []TriageScore, verified []VerifyScore, triageCalls, verifyCalls []Call) Summary {
	var tp, fp, fn int
	categoryOK := make([]bool, len(triaged))
	articlesOK := make([]bool, len(triaged))
	for i, s := range triaged {
		tp, fp, fn = tp+s.TP, fp+s.FP, fn+s.FN
		categoryOK[i], articlesOK[i] = s.CategoryOK, s.ArticlesOK
	}

	groundedOK := make([]bool, len(verified))
	onTopicOK := make([]bool, len(verified))
	decisionOK := make([]bool, len(verified))
	var caught, passed []bool
	for i, s := range verified {
		groundedOK[i], onTopicOK[i], decisionOK[i] = s.GroundedOK, s.OnTopicOK, s.DecisionOK
		if s.ShouldBlock {
			caught = append(caught, s.Blocked)
		} else {
			passed = append(passed, !s.Blocked)
		}
	}

	return Summary{
		Accuracy: Accuracy{
			Category:          rate(categoryOK),
			ArticlesExact:     rate(articlesOK),
			ArticlesPrecision: ratio(tp, tp+fp),
			ArticlesRecall:    ratio(tp, tp+fn),
			Grounded:          rate(groundedOK),
			OnTopic:           rate(onTopicOK),
			GateDecision:      rate(decisionOK),
			BadDraftsCaught:   rate(caught),
			GoodDraftsPassed:  rate(passed),
		},
		Latency: map[string]Latency{
			"triage": latency(triageCalls),
			"verify": latency(verifyCalls),
			"all":    latency(append(append([]Call{}, triageCalls...), verifyCalls...)),
		},
		Tokens: map[string]float64{
			"triage": meanOf(tokensOf(triageCalls)),
			"verify": meanOf(tokensOf(verifyCalls)),
		},
	}
}

// sweep re-scores everything at nine thresholds without asking anything again.
// The probabilities are already in hand; only the comparison moves.
func sweep(triageCalls, verifyCalls []Call) []SweepRow {
	var rows []SweepRow
	for th := 0.1; th < 0.95; th += 0.1 {
		th = math.Round(th*10) / 10

		exact := make([]bool, len(Triage))
		for i, c := range Triage {
			exact[i] = scoreTriage(c, triageCalls[i], th).ArticlesOK
		}
		decision := make([]bool, len(Verify))
		var caught, passed []bool
		for i, c := range Verify {
			s := scoreVerify(c, verifyCalls[i], th)
			decision[i] = s.DecisionOK
			if s.ShouldBlock {
				caught = append(caught, s.Blocked)
			} else {
				passed = append(passed, !s.Blocked)
			}
		}
		rows = append(rows, SweepRow{
			Threshold: th, ArticlesExact: rate(exact), GateDecision: rate(decision),
			BadDraftsCaught: rate(caught), GoodDraftsPassed: rate(passed),
		})
	}
	return rows
}

func brier(triageCalls, verifyCalls []Call) Brier {
	var articles []float64
	for i, c := range Triage {
		for _, topic := range triage.Topics() {
			if contains(c.OK, topic) {
				continue // an optional article has no truth to be wrong about
			}
			articles = append(articles,
				square(triageCalls[i].Nouls[triage.ArticleKey(topic)]-boolTo(contains(c.Need, topic))))
		}
	}
	var gate []float64
	for i, c := range Verify {
		gate = append(gate,
			square(verifyCalls[i].Nouls[triage.GroundedKey]-boolTo(c.Grounded)),
			square(verifyCalls[i].Nouls[triage.OnTopicKey]-boolTo(c.OnTopic)))
	}
	return Brier{Articles: meanOf(articles), Gate: meanOf(gate)}
}

// pool runs n jobs with at most workers in flight, and keeps the results in
// order. The first error wins and cancels the rest.
func pool[T any](ctx context.Context, workers, n int, job func(context.Context, int) (T, error)) ([]T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]T, n)
	errs := make([]error, n)
	next := make(chan int)

	go func() {
		defer close(next)
		for i := 0; i < n; i++ {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < min(workers, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				out[i], errs[i] = job(ctx, i)
				if errs[i] != nil {
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// write drops the result next to the event log: latest.json for whatever reads
// it, and a timestamped copy so a run is never overwritten by the next one.
func write(dir string, result Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	latest := filepath.Join(dir, "latest.json")
	if err := os.WriteFile(latest, raw, 0o644); err != nil {
		return "", err
	}
	stamped := filepath.Join(dir, "bench-"+time.Now().Format("20060102-150405")+".json")
	if err := os.WriteFile(stamped, raw, 0o644); err != nil {
		return "", err
	}
	return latest, nil
}

// ------------------------------------------------------------------ the table

// Table prints the comparison.
func (r Result) Table(w io.Writer) {
	rows := []struct {
		name string
		of   func(Summary) string
	}{
		{"category accuracy", func(s Summary) string { return pct(s.Accuracy.Category) }},
		{"articles exact", func(s Summary) string { return pct(s.Accuracy.ArticlesExact) }},
		{"articles precision", func(s Summary) string { return pct(s.Accuracy.ArticlesPrecision) }},
		{"articles recall", func(s Summary) string { return pct(s.Accuracy.ArticlesRecall) }},
		{"grounded accuracy", func(s Summary) string { return pct(s.Accuracy.Grounded) }},
		{"on_topic accuracy", func(s Summary) string { return pct(s.Accuracy.OnTopic) }},
		{"gate decision", func(s Summary) string { return pct(s.Accuracy.GateDecision) }},
		{"bad drafts caught", func(s Summary) string { return pct(s.Accuracy.BadDraftsCaught) }},
		{"good drafts passed", func(s Summary) string { return pct(s.Accuracy.GoodDraftsPassed) }},
		{"latency p50 (ms)", func(s Summary) string { return fmt.Sprintf("%6d", s.Latency["all"].P50) }},
		{"latency p95 (ms)", func(s Summary) string { return fmt.Sprintf("%6d", s.Latency["all"].P95) }},
		{"mean tokens", func(s Summary) string { return fmt.Sprintf("%6.0f", s.Tokens["triage"]) }},
	}

	fmt.Fprintf(w, "\n  %-20s", "metric")
	for _, s := range r.Systems {
		fmt.Fprintf(w, "%8s", s)
	}
	fmt.Fprintln(w)
	for _, row := range rows {
		fmt.Fprintf(w, "  %-20s", row.name)
		for _, s := range r.Systems {
			fmt.Fprintf(w, "%8s", row.of(r.Summary[s]))
		}
		fmt.Fprintln(w)
	}

	if r.Brier != nil {
		fmt.Fprintf(w, "\n  Jev Brier score: articles %.3f, gate %.3f "+
			"(0 = perfect, 0.25 = a coin flip)\n", r.Brier.Articles, r.Brier.Gate)
	}
	if len(r.Sweep) > 0 {
		// The table the baseline cannot produce: a label has no probability,
		// so there is no threshold to move.
		fmt.Fprintf(w, "\n  threshold sweep (Jev only — a label has no threshold)\n")
		fmt.Fprintf(w, "  %9s %14s %14s %12s %13s\n",
			"threshold", "articles exact", "gate decision", "bad caught", "good passed")
		for _, row := range r.Sweep {
			fmt.Fprintf(w, "  %9.1f %14s %14s %12s %13s\n", row.Threshold,
				pct(row.ArticlesExact), pct(row.GateDecision),
				pct(row.BadDraftsCaught), pct(row.GoodDraftsPassed))
		}
	}
	fmt.Fprintf(w, "\n  wrote %s\n", r.Path)
}
