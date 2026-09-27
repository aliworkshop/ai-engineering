// Package jev talks to a System One model: you hand it STATE and typed
// QUESTIONS, and it hands back typed ANSWERS with probabilities. It never
// writes text.
//
// That is the whole difference from internal/llm, and it is worth being precise
// about. A language model answers "is this draft grounded in these articles?"
// with a sentence, which your code then has to interpret — and a sentence is
// exactly the wrong thing to hang a decision on. Jev answers it with a number
// between 0 and 1. The number is not the decision; it is the input to one, and
// the decision itself stays in Go where you can read it, test it, and change it
// without re-writing a prompt.
//
// Two calls, deliberately: Call is the raw request, used by the benchmark,
// which wants timing and nothing else. Ask is the same request wrapped in
// events, used by the agent, so the stream shows what was asked and what came
// back. Neither one knows what the answers mean — that lives in internal/triage.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// Endpoint is OpenRouter's System One path. It speaks TypeSafe's own wire
// format unchanged, which is why this package needs no second credential and no
// second provider: the OPENROUTER_API_KEY the agent already has is the one that
// works here.
//
// OpenRouter serves the identical schema at /api/alpha/decisions. This path is
// the compatible one, so a request written against TypeSafe's documentation
// works without translation.
const Endpoint = "https://openrouter.ai/api/v1/systemone"

// Model is pinned to a version rather than an alias. An alias moves when a new
// release ships, and the thresholds in internal/triage are tuned against a
// specific model's calibration — a model that silently changes underneath them
// changes what the agent will and will not send.
const Model = "typesafe/jev-1.13"

// The three question types. Strings rather than an enum because they are wire
// values, not Go identifiers.
const (
	NoulType   = "noul"
	ChoiceType = "choice"
	ScoreType  = "score"
)

// maxRetries is how many times a rate-limited or overloaded request is retried
// before giving up. The delay doubles each time from retryBase.
const (
	maxRetries = 3
	retryBase  = 500 * time.Millisecond

	// noRetry is the wait that means "this will fail again".
	noRetry = -1 * time.Second

	// statusOverloaded is 529, which net/http has no constant for.
	statusOverloaded = 529
)

// Question is one typed question. Instructions and Criteria are `any` because
// the API accepts a string, an object or an array in both positions — you can
// hand a question structured data and point at its fields by name. The
// constructors below cover every shape this repo needs, so no call site has to
// build a criteria map by hand and get its keys wrong.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Noul asks a yes/no question and gets back the probability of yes. The two
// criteria say what a yes and a no mean; they are optional to the API and
// always worth writing anyway, because "would this article help?" means
// something different to the asker than to the model.
func Noul(instructions, yes, no string) Question {
	return Question{
		Type:         NoulType,
		Instructions: instructions,
		Criteria:     map[string]string{"true": yes, "false": no},
	}
}

// Choice picks one option from a set, and returns the whole distribution over
// it plus a confidence. The options are the map keys and their rubrics are the
// values — a closed set, so there is no such thing as an answer outside it.
func Choice(instructions string, criteria map[string]string) Question {
	return Question{Type: ChoiceType, Instructions: instructions, Criteria: criteria}
}

// Score rates the state against ordered levels, lowest first, and can land
// between two of them. Unused by the agent today; here because a question type
// the client cannot express is a client that quietly shapes what you ask.
func Score(instructions string, levels ...string) Question {
	return Question{Type: ScoreType, Instructions: instructions, Criteria: levels}
}

// Answer is one answer, flattened.
//
// The API returns a union — a noul answer has no `choice`, a choice answer has
// no `score` — and Go's answer to a union is an interface plus a type switch at
// every call site. This is the same trade agent.Msg already makes against the
// SDK's message types: one plain struct in our own vocabulary, read the field
// that matches the question you asked. Type says which that is.
//
// Confidence and Probabilities are absent on a noul, and OpenRouter's schema
// does not require them even on a choice, so nothing here may assume they
// arrived. A zero Confidence means "not reported", which is why the policy in
// internal/triage compares it against a threshold rather than trusting it.
type Answer struct {
	Type string `json:"type"`

	Noul   float64 `json:"noul"`   // noul: 0 (no) to 1 (yes)
	Choice string  `json:"choice"` // choice: the highest-probability option
	Score  float64 `json:"score"`  // score: probability-weighted across levels

	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// Usage is what the request cost. Output tokens are free on this model, so the
// number that matters is the input one — and since every question in a request
// is evaluated against the state once, asking ten questions together costs
// barely more than asking one.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost,omitempty"`
}

// Result is one round trip. Model is what actually answered, which is worth
// keeping: it is the versioned id behind the name we asked for.
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`

	Latency time.Duration `json:"-"`
	Retries int           `json:"-"`
}

// Client calls the model. It holds an http.Client so the timeout is ours rather
// than the default transport's, which has none.
type Client struct {
	key      string
	model    string
	endpoint string
	http     *http.Client
	bus      events.Emitter
}

// New returns a client authenticated with an OpenRouter API key.
func New(apiKey string) *Client {
	return &Client{
		key:      apiKey,
		model:    Model,
		endpoint: Endpoint,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

// WithEvents attaches an emitter, so Ask narrates. Optional, like every other
// service in this repo: a nil emitter is a silent client.
func (c *Client) WithEvents(bus events.Emitter) *Client {
	c.bus = bus
	return c
}

// WithEndpoint points the client somewhere else. The tests use it; nothing else
// should.
func (c *Client) WithEndpoint(url string) *Client {
	c.endpoint = url
	return c
}

// Call sends one request and returns the answers. No events, no interpretation:
// this is the wire, and the benchmark uses it directly so that what it measures
// is the model rather than the harness around it.
func (c *Client) Call(ctx context.Context, state any, questions map[string]Question) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.model, "state": state, "questions": questions,
	})
	if err != nil {
		return Result{}, fmt.Errorf("jev: encoding request: %w", err)
	}

	started := time.Now()
	for attempt := 0; ; attempt++ {
		result, wait, err := c.once(ctx, body)
		if err == nil {
			result.Latency = time.Since(started)
			result.Retries = attempt
			return result, nil
		}
		if wait < 0 || attempt >= maxRetries {
			return Result{}, err
		}
		if wait == 0 {
			wait = retryBase << attempt // 500ms, 1s, 2s
		}
		// Sleeping on the context rather than on the clock, so a cancelled run
		// stops waiting instead of holding the process open for a retry nobody
		// is listening for any more.
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
}

// Ask is Call, narrated.
//
// Two labels, and the difference between them matters. purpose is for a human
// reading the stream — "triage item-1", "verify item-3". key identifies the
// DECISION: it is what makes "this judgment was bought twice" a question the
// log can answer, and it is not the purpose, because a purpose legitimately
// repeats. A blocked draft is redrafted and verified again, and that second
// verification is a different question about a different draft.
//
// The events carry a SUMMARY, not the payload. The console prints whole events
// since it stopped truncating them, and the state here is a work item plus the
// entire knowledge base: logging it per item would bury every other line in the
// run. What you get instead is which questions were asked and what came back.
func (c *Client) Ask(ctx context.Context, workflow, key, purpose string, state any, questions map[string]Question) (Result, error) {
	events.Emit(c.bus, events.Event{
		Type:     events.JevRequested,
		Workflow: workflow,
		Call:     key,
		Name:     purpose,
		Args:     strings.Join(names(questions), ","),
		Input:    c.model,
	})

	result, err := c.Call(ctx, state, questions)
	if err != nil {
		events.Emit(c.bus, events.Event{
			Type: events.JevAnswered, Workflow: workflow, Call: key, Name: purpose,
			Error: err.Error(),
		})
		return Result{}, err
	}

	events.Emit(c.bus, events.Event{
		Type:     events.JevAnswered,
		Workflow: workflow,
		Call:     key,
		Name:     purpose,
		Output:   Summary(result.Answers),
		Millis:   result.Latency.Milliseconds(),
	})
	return result, nil
}

// once performs a single request. Its middle return says what to do with a
// failure — a negative wait means "do not retry", zero means "retry on the
// caller's own backoff", and a positive one is a delay the server asked for.
// That is the only thing the caller needs from a status code it never sees.
func (c *Client) once(ctx context.Context, body []byte) (Result, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, noRetry, fmt.Errorf("jev: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error is worth one more try: it is usually a connection
		// the other end closed, not a request the other end refused.
		return Result{}, 0, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, 0, fmt.Errorf("jev: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 429 and 529 are the two the model's own docs tell you to back off
		// from. Everything else — a bad key, a malformed question — will fail
		// the same way however many times you send it.
		wait := noRetry
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == statusOverloaded {
			wait = retryAfter(resp.Header.Get("Retry-After"))
		}
		return Result{}, wait, fmt.Errorf("jev: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, noRetry, fmt.Errorf("jev: decoding response: %w", err)
	}
	if len(result.Answers) == 0 {
		return Result{}, noRetry, fmt.Errorf("jev: no answers in response")
	}
	return result, noRetry, nil
}

// retryAfter reads the header of that name, which carries a number of seconds.
// Honouring it matters on a shared rate limit: our own doubling backoff is a
// guess, and the server just told us the answer.
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs <= 0 {
		return 0 // no usable header: back off on our own schedule
	}
	return time.Duration(secs) * time.Second
}

// Summary renders answers as one scannable line: the question id, then the
// value that question type actually returned.
func Summary(answers map[string]Answer) string {
	parts := make([]string, 0, len(answers))
	for _, id := range sortedKeys(answers) {
		a := answers[id]
		switch a.Type {
		case ChoiceType:
			parts = append(parts, fmt.Sprintf("%s=%s/%s", id, a.Choice, round(a.Confidence)))
		case ScoreType:
			parts = append(parts, fmt.Sprintf("%s=%s", id, round(a.Score)))
		default:
			parts = append(parts, fmt.Sprintf("%s=%s", id, round(a.Noul)))
		}
	}
	return strings.Join(parts, " ")
}

// names lists the question ids in a stable order, so two identical requests
// produce two identical event lines.
func names(questions map[string]Question) []string {
	out := make([]string, 0, len(questions))
	for id := range questions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(answers map[string]Answer) []string {
	out := make([]string, 0, len(answers))
	for id := range answers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// round formats a probability to two places. Calibration this model reports to
// more digits than a human reading a terminal can use.
func round(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}
