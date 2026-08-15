package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/retry"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/toolspec"
)

// routedModel answers based on what was ASKED rather than on call order,
// because the supervisor's investigators run concurrently and a sequence-based
// fake would be racing the thing under test.
type routedModel struct {
	mu       sync.Mutex
	requests []string
	route    func(body string) (reply string, status int)
}

func routedClient(t *testing.T, m *routedModel) *openrouter.OpenRouter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)

		m.mu.Lock()
		m.requests = append(m.requests, body)
		m.mu.Unlock()

		reply, status := m.route(body)
		if status != 0 && status != http.StatusOK {
			http.Error(w, `{"error":"upstream exploded"}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"gen","model":"fake","object":"chat.completion","created":0,
			"choices":[{"index":0,"finish_reason":"stop","message":%s}]}`, reply)
	}))
	t.Cleanup(server.Close)

	return openrouter.New(
		openrouter.WithSecurity("test-key"),
		openrouter.WithServerURL(server.URL),
		// One attempt: the failure this test injects is deliberate, and the
		// SDK's retries would turn a fast assertion into a slow one.
		openrouter.WithRetryConfig(retry.Config{Strategy: "none"}),
	)
}

func (m *routedModel) seen(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, req := range m.requests {
		if strings.Contains(req, substr) {
			n++
		}
	}
	return n
}

// readOnlyTools is what an investigator holds: lookups, nothing more.
type readOnlyTools struct {
	mu    sync.Mutex
	calls int
}

func (r *readOnlyTools) Specs() []components.ChatFunctionTool {
	return []components.ChatFunctionTool{
		toolspec.Define("look_up", "Look something up.",
			`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
}

func (r *readOnlyTools) Dispatch(context.Context, string, string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return `{"found":"a fact"}`
}

// TestSupervisorDegradesWhenAnInvestigatorFails is the Part 6 claim that
// actually justifies the machinery: two findings and one honest "this failed"
// beats an error.
func TestSupervisorDegradesWhenAnInvestigatorFails(t *testing.T) {
	var synthesisInput string
	var mu sync.Mutex

	model := &routedModel{}
	model.route = func(body string) (string, int) {
		switch {
		case strings.Contains(body, "break a request into independent lines"):
			plan := `{"objective":"compare the two","tasks":[
				{"lens":"pricing","objective":"what does it cost"},
				{"lens":"reliability","objective":"how often does it break"}]}`
			return quoted(plan), 0

		case strings.Contains(body, "research investigator") && strings.Contains(body, "reliability"):
			return "", http.StatusInternalServerError // this one dies

		case strings.Contains(body, "research investigator"):
			return quoted("It costs $20 per seat per month."), 0

		case strings.Contains(body, "writing up the results"):
			mu.Lock()
			synthesisInput = body
			mu.Unlock()
			return quoted("Pricing is $20/seat. Reliability is unknown — that investigation failed."), 0
		}
		return quoted("unexpected request"), 0
	}

	tools := &readOnlyTools{}
	sup := NewSupervisor(routedClient(t, model), "fake", tools, nil)

	answer, err := sup.Run(context.Background(), "compare the two")
	if err != nil {
		t.Fatalf("a failed investigator must not fail the supervisor: %v", err)
	}
	if !strings.Contains(answer, "$20/seat") {
		t.Fatalf("the surviving finding was lost: %q", answer)
	}

	// Synthesis has to be TOLD about the failure, or it writes a confident
	// answer with a silent hole in it.
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(synthesisInput, "FAILED") {
		t.Fatalf("synthesis was not told an investigation failed:\n%s", synthesisInput)
	}
	if !strings.Contains(synthesisInput, "reliability") {
		t.Fatalf("synthesis cannot name the gap it should report:\n%s", synthesisInput)
	}
}

// TestInvestigatorsGetTheirOwnContext is the win that comes before parallelism:
// the parent never sees the raw tool output, only a short finding.
func TestInvestigatorsGetTheirOwnContext(t *testing.T) {
	model := &routedModel{}
	var toolTurn sync.Map

	model.route = func(body string) (string, int) {
		switch {
		case strings.Contains(body, "break a request into independent lines"):
			return quoted(`{"objective":"o","tasks":[{"lens":"pricing","objective":"cost"}]}`), 0

		case strings.Contains(body, "research investigator"):
			// First time: ask for a tool. Second time: report.
			if _, already := toolTurn.LoadOrStore("pricing", true); already {
				return quoted("Costs $20."), 0
			}
			return `{"role":"assistant","tool_calls":[{"id":"c1","type":"function",` +
				`"function":{"name":"look_up","arguments":"{\"q\":\"price\"}"}}]}`, 0

		case strings.Contains(body, "writing up the results"):
			return quoted("It costs $20."), 0
		}
		return quoted("?"), 0
	}

	tools := &readOnlyTools{}
	sup := NewSupervisor(routedClient(t, model), "fake", tools, nil)

	if _, err := sup.Run(context.Background(), "what does it cost"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("investigator made %d tool calls, expected 1", tools.calls)
	}

	// The synthesis request must carry the FINDING, not the investigator's raw
	// tool traffic — that is the context isolation the fan-out is for.
	model.mu.Lock()
	defer model.mu.Unlock()
	for _, req := range model.requests {
		if strings.Contains(req, "writing up the results") && strings.Contains(req, "a fact") {
			t.Fatalf("raw tool output leaked into the parent's context:\n%s", req)
		}
	}
}

// TestPlanFallsBackToASingleTask keeps a planner that returned nonsense from
// costing the user an answer.
func TestPlanFallsBackToASingleTask(t *testing.T) {
	model := &routedModel{}
	model.route = func(body string) (string, int) {
		switch {
		case strings.Contains(body, "break a request into independent lines"):
			return quoted("I'm afraid I can't do that."), 0 // not JSON at all
		case strings.Contains(body, "research investigator"):
			return quoted("Here is what I found."), 0
		}
		return quoted("A write-up."), 0
	}

	sup := NewSupervisor(routedClient(t, model), "fake", &readOnlyTools{}, nil)
	answer, err := sup.Run(context.Background(), "just answer this")
	if err != nil {
		t.Fatalf("an unusable plan must degrade, not fail: %v", err)
	}
	if answer == "" {
		t.Fatalf("expected an answer from the fallback single task")
	}
	if model.seen("research investigator") != 1 {
		t.Fatalf("expected exactly one fallback investigation, got %d", model.seen("research investigator"))
	}
}

// TestFencedPlanIsAccepted covers the model wrapping its JSON in a code fence
// despite being told not to — cheaper to strip than to retry.
func TestFencedPlanIsAccepted(t *testing.T) {
	for _, raw := range []string{
		"```json\n{\"objective\":\"o\",\"tasks\":[{\"lens\":\"a\",\"objective\":\"x\"}]}\n```",
		"{\"objective\":\"o\",\"tasks\":[{\"lens\":\"a\",\"objective\":\"x\"}]}",
	} {
		var plan Plan
		if err := json.Unmarshal([]byte(stripFence(raw)), &plan); err != nil {
			t.Fatalf("stripFence left unparsable JSON for %q: %v", raw, err)
		}
		if len(plan.Tasks) != 1 || plan.Tasks[0].Lens != "a" {
			t.Fatalf("plan decoded wrong: %+v", plan)
		}
	}
}

// TestPlanIsCappedGuardsAgainstAnExcitedPlanner.
func TestPlanIsCapped(t *testing.T) {
	model := &routedModel{}
	model.route = func(body string) (string, int) {
		if strings.Contains(body, "break a request into independent lines") {
			var tasks []string
			for i := 0; i < 20; i++ {
				tasks = append(tasks, fmt.Sprintf(`{"lens":"l%d","objective":"o%d"}`, i, i))
			}
			return quoted(`{"objective":"o","tasks":[` + strings.Join(tasks, ",") + `]}`), 0
		}
		if strings.Contains(body, "research investigator") {
			return quoted("finding"), 0
		}
		return quoted("write-up"), 0
	}

	sup := NewSupervisor(routedClient(t, model), "fake", &readOnlyTools{}, events.EmitterFunc(func(events.Event) {}))
	if _, err := sup.Run(context.Background(), "do everything"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := model.seen("research investigator"); got > maxTasks {
		t.Fatalf("ran %d investigations against a cap of %d", got, maxTasks)
	}
}

// quoted wraps text as an assistant message body.
func quoted(text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"role":"assistant","content":%s}`, encoded)
}
