package jev

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve stands up a fake System One endpoint that replies with the given bodies
// and statuses in order, and records every request it received.
func serve(t *testing.T, replies ...reply) (*Client, *[][]byte) {
	t.Helper()

	var bodies [][]byte
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)

		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want the bearer key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		res := replies[min(n, len(replies)-1)]
		n++
		if res.retryAfter != "" {
			w.Header().Set("Retry-After", res.retryAfter)
		}
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	}))
	t.Cleanup(srv.Close)

	return New("test-key").WithEndpoint(srv.URL), &bodies
}

type reply struct {
	status     int
	body       string
	retryAfter string
}

func ok(body string) reply { return reply{status: 200, body: body} }

// A request carries the model, the state and the questions, and nothing else
// has to be true for the model to answer it.
func TestCallSendsStateAndQuestions(t *testing.T) {
	client, bodies := serve(t, ok(`{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.9}},
		"usage":{"input_tokens":10,"output_tokens":2}}`))

	_, err := client.Call(context.Background(), map[string]any{"item": "help"},
		map[string]Question{"urgent": Noul("Is this urgent?", "yes", "no")})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var sent struct {
		Model     string              `json:"model"`
		State     map[string]any      `json:"state"`
		Questions map[string]Question `json:"questions"`
	}
	if err := json.Unmarshal((*bodies)[0], &sent); err != nil {
		t.Fatalf("request body is not the documented shape: %v", err)
	}
	if sent.Model != Model {
		t.Errorf("model = %q, want %q", sent.Model, Model)
	}
	if sent.State["item"] != "help" {
		t.Errorf("state = %v, want the item we passed", sent.State)
	}
	q := sent.Questions["urgent"]
	if q.Type != NoulType {
		t.Errorf("question type = %q, want %q", q.Type, NoulType)
	}
	criteria, _ := q.Criteria.(map[string]any)
	if criteria["true"] != "yes" || criteria["false"] != "no" {
		t.Errorf("noul criteria = %v, want both branches described", q.Criteria)
	}
}

// Every answer type decodes into the one flat struct, and a choice answer that
// arrives WITHOUT probabilities or confidence is still a usable answer —
// OpenRouter's schema does not promise them.
func TestAnswerTypesDecode(t *testing.T) {
	client, _ := serve(t, ok(`{"model":"jev-1.13.0","answers":{
		"yes_no":{"type":"noul","noul":0.94},
		"team":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"sales":0.12},"confidence":0.81},
		"bare":{"type":"choice","choice":"sales"},
		"mood":{"type":"score","score":1.05,"legend":{"0":"calm","1":"cross"},"probabilities":{"0":0.1,"1":0.9},"confidence":0.92}},
		"usage":{"input_tokens":300,"output_tokens":34,"cost":0.000013}}`))

	got, err := client.Call(context.Background(), "state", map[string]Question{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if a := got.Answers["yes_no"]; a.Noul != 0.94 {
		t.Errorf("noul = %v, want 0.94", a.Noul)
	}
	if a := got.Answers["team"]; a.Choice != "billing" || a.Confidence != 0.81 ||
		a.Probabilities["billing"] != 0.88 {
		t.Errorf("choice answer = %+v, want billing at 0.88 with confidence 0.81", a)
	}
	if a := got.Answers["bare"]; a.Choice != "sales" || a.Confidence != 0 {
		t.Errorf("choice with no confidence = %+v, want the choice and a zero confidence", a)
	}
	if a := got.Answers["mood"]; a.Score != 1.05 || a.Legend["1"] != "cross" {
		t.Errorf("score answer = %+v, want 1.05 with its legend", a)
	}
	if got.Usage.InputTokens != 300 || got.Model != "jev-1.13.0" {
		t.Errorf("usage/model = %+v / %q, want the versioned id that answered", got.Usage, got.Model)
	}
	if got.Latency <= 0 {
		t.Error("latency was not measured")
	}
}

// A rate-limited request is retried, and the answer that eventually arrives is
// the one the caller gets — with a count of what it took to get there.
func TestRateLimitIsRetried(t *testing.T) {
	client, bodies := serve(t,
		reply{status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, retryAfter: "0"},
		ok(`{"model":"jev-1.13.0","answers":{"a":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1,"output_tokens":1}}`),
	)

	got, err := client.Call(context.Background(), "state", map[string]Question{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Retries != 1 {
		t.Errorf("retries = %d, want 1", got.Retries)
	}
	if len(*bodies) != 2 {
		t.Errorf("sent %d requests, want 2", len(*bodies))
	}
}

// A bad key will be a bad key next time too, so it is not retried.
func TestUnauthorizedIsNotRetried(t *testing.T) {
	client, bodies := serve(t, reply{status: http.StatusUnauthorized, body: `{"error":"no key"}`})

	_, err := client.Call(context.Background(), "state", map[string]Question{})
	if err == nil {
		t.Fatal("Call succeeded against a 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want it to name the status", err)
	}
	if len(*bodies) != 1 {
		t.Errorf("sent %d requests, want 1 — a 401 is not worth retrying", len(*bodies))
	}
}

// The summary line is what a human reads in the stream, so it has to be stable
// and it has to show the number each question type actually answered with.
func TestSummary(t *testing.T) {
	got := Summary(map[string]Answer{
		"kb_billing": {Type: NoulType, Noul: 0.9412},
		"category":   {Type: ChoiceType, Choice: "billing", Confidence: 0.8149},
		"mood":       {Type: ScoreType, Score: 1.05},
	})
	want := "category=billing/0.81 kb_billing=0.94 mood=1.05"
	if got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}
