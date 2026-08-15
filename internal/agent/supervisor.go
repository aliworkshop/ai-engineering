package agent

// Part 6 · supervision.
//
// Same honesty as Part 5: you usually don't need this either. One agent works
// through a multi-part request serially and does fine. Supervision earns its
// keep on three specific wins:
//
//   - Context isolation. Each investigator works in its own window and returns
//     a short finding, so the parent never sees three subjects' worth of raw
//     tool output. This is the big one — it is a memory win before it is a
//     speed win.
//   - Parallelism. Independent sub-tasks run at the same time.
//   - Synthesis that survives partial failure. Two findings and one honest
//     "this one failed" beats an error.
//
// Whether it actually makes your agent better is a question for evals, not for
// an architecture diagram.
//
// Four phases: plan → dispatch → fan in → synthesize. Two choices matter. The
// plan is a first-class artifact — emitted, checkpointed, and read by synthesis
// rather than left as ephemeral reasoning. And investigators are strictly
// read-only, which is what lets a whole investigation be one durable step:
// replaying it after a crash is harmless because it changed nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/toolspec"
)

// Bounds. maxTasks keeps a planner that got excited from opening twenty
// connections; maxConcurrent keeps even a legal plan from doing so all at once.
const (
	maxTasks          = 4
	maxConcurrent     = 3
	investigatorTurns = 5
)

// Supervisor plans a multi-part objective, fans it out to read-only
// investigators, and writes up what came back.
type Supervisor struct {
	client *openrouter.OpenRouter
	model  string

	// tools is the read-only toolbox investigators share. It is built from a
	// registry subset rather than filtered here, so "read-only" is a property
	// of the box they are handed, not a rule they are asked to follow.
	tools ToolBox

	bus events.Emitter
}

// NewSupervisor builds a supervisor over a read-only toolbox.
func NewSupervisor(client *openrouter.OpenRouter, model string, readOnly ToolBox, bus events.Emitter) *Supervisor {
	return &Supervisor{client: client, model: model, tools: readOnly, bus: bus}
}

// Plan is the structured artifact phase one produces.
type Plan struct {
	Objective string `json:"objective"`
	Tasks     []Task `json:"tasks"`
}

// Task is one investigator's assignment. Lens is a short label — "pricing",
// "the codebase", "recent news" — that both names the sub-agent in the event
// stream and shapes its prompt.
type Task struct {
	Lens      string `json:"lens"`
	Objective string `json:"objective"`
}

// Finding is what one investigator returns, or why it didn't.
type Finding struct {
	Lens   string `json:"lens"`
	Report string `json:"report,omitempty"`
	Error  string `json:"error,omitempty"`
}

const plannerPrompt = `You break a request into independent lines of investigation.

Return ONLY a JSON object, no prose and no code fence:
{"objective": "...", "tasks": [{"lens": "short-label", "objective": "one sentence"}]}

Rules:
- Between 1 and %d tasks. Fewer is better; use one task if the request really
  is one thing.
- Tasks must be INDEPENDENT — each is researched without seeing the others.
  If step two needs step one's answer, they are one task, not two.
- Each objective must be answerable with read-only tools: reading files,
  searching the web, running sandboxed code. Nothing that changes anything.`

const investigatorPrompt = `You are a research investigator working on one narrow question: %s.

Use your read-only tools to find out. You cannot change anything and should not
try. Report what you found in a few sentences — concrete facts, numbers, paths,
and URLs, not a description of your process. If you could not find out, say so
plainly and say what you tried.`

const synthesisPrompt = `You are writing up the results of several parallel investigations.

Answer the original objective using the findings below. Rules:
- Use only what the findings actually say. Do not fill gaps from your own
  knowledge without labelling it as such.
- If an investigation failed, say which one and what is therefore unknown.
  A partial answer that is honest about its holes is the goal.
- Be concise and concrete. Keep any source URLs the findings carry.`

// Run executes all four phases and returns the write-up.
func (s *Supervisor) Run(ctx context.Context, objective string) (string, error) {
	plan, err := s.plan(ctx, objective)
	if err != nil {
		return "", err
	}
	events.Emit(s.bus, events.Event{
		Type: events.PlanCreated, Name: fmt.Sprintf("%d tasks", len(plan.Tasks)),
		Text: objective,
	})

	findings := s.dispatch(ctx, plan)
	return s.synthesize(ctx, plan, findings)
}

// plan asks the model for a structured decomposition. If the model returns
// something unusable we fall back to a single task covering the whole
// objective — a degraded plan still answers the question, where a hard failure
// answers nothing.
func (s *Supervisor) plan(ctx context.Context, objective string) (Plan, error) {
	reply, err := send(ctx, s.client, s.model, []Msg{
		{Role: "system", Text: fmt.Sprintf(plannerPrompt, maxTasks)},
		{Role: "user", Text: objective},
	}, nil)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{Objective: objective}
	if err := json.Unmarshal([]byte(stripFence(assistantText(reply))), &plan); err != nil || len(plan.Tasks) == 0 {
		plan.Tasks = []Task{{Lens: "overall", Objective: objective}}
	}
	if plan.Objective == "" {
		plan.Objective = objective
	}
	if len(plan.Tasks) > maxTasks {
		plan.Tasks = plan.Tasks[:maxTasks]
	}
	return plan, nil
}

// dispatch runs the investigations concurrently and fans them back in.
//
// It never returns an error. A failed investigation becomes a Finding with an
// Error set, and synthesis is told to write around it — degrading gracefully is
// the whole reason to build this rather than call the tools in a row.
func (s *Supervisor) dispatch(ctx context.Context, plan Plan) []Finding {
	findings := make([]Finding, len(plan.Tasks))
	slots := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	for i, task := range plan.Tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			// A panic in one investigator must not take the supervisor — and
			// with it the other investigations — down with it.
			defer func() {
				if r := recover(); r != nil {
					findings[i] = Finding{Lens: task.Lens, Error: fmt.Sprintf("investigator panicked: %v", r)}
				}
			}()

			events.Emit(s.bus, events.Event{
				Type: events.SubagentStarted, Name: task.Lens, Text: task.Objective,
			})

			report, err := s.investigate(ctx, task)
			if err != nil {
				findings[i] = Finding{Lens: task.Lens, Error: err.Error()}
				events.Emit(s.bus, events.Event{
					Type: events.SubagentFailed, Name: task.Lens, Error: err.Error(),
				})
				return
			}
			findings[i] = Finding{Lens: task.Lens, Report: report}
			events.Emit(s.bus, events.Event{
				Type: events.SubagentCompleted, Name: task.Lens, Result: truncate(report, 70),
			})
		}()
	}
	wg.Wait()
	return findings
}

// investigate is one bounded agent loop in its own context window. It is
// deliberately not an *Agent: no memory, no compaction, no handoffs, no
// approval gate — an investigator that could hand off or ask for approval would
// break the read-only promise the parent is relying on.
func (s *Supervisor) investigate(ctx context.Context, task Task) (string, error) {
	msgs := []Msg{
		{Role: "system", Text: fmt.Sprintf(investigatorPrompt, task.Lens)},
		{Role: "user", Text: task.Objective},
	}

	for turn := 0; turn < investigatorTurns; turn++ {
		reply, err := send(ctx, s.client, s.model, msgs, s.tools.Specs())
		if err != nil {
			return "", err
		}
		msg := fromAssistant(reply)
		msgs = append(msgs, msg)

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Text) == "" {
				return "", fmt.Errorf("investigator %q returned nothing", task.Lens)
			}
			return msg.Text, nil
		}
		for _, call := range msg.ToolCalls {
			result := s.tools.Dispatch(ctx, call.Name, call.Args)
			msgs = append(msgs, Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
	}
	return "", fmt.Errorf("investigator %q ran out of steps", task.Lens)
}

// synthesize writes the findings up against the plan. The plan is passed in
// rather than re-derived: it is the artifact phase one produced, and reading it
// here is what lets the write-up notice that a task is missing an answer.
func (s *Supervisor) synthesize(ctx context.Context, plan Plan, findings []Finding) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Objective: %s\n\n", plan.Objective)
	for _, f := range findings {
		if f.Error != "" {
			fmt.Fprintf(&b, "## %s\nFAILED: %s\n\n", f.Lens, f.Error)
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", f.Lens, f.Report)
	}

	reply, err := send(ctx, s.client, s.model, []Msg{
		{Role: "system", Text: synthesisPrompt},
		{Role: "user", Text: b.String()},
	}, nil)
	if err != nil {
		// Even synthesis failing is recoverable: hand back the raw findings.
		// The user gets something useful, clearly labelled as unsynthesized.
		return "(synthesis failed: " + err.Error() + ")\n\n" + b.String(), nil
	}
	return assistantText(reply), nil
}

// stripFence removes a ```json wrapper the model adds despite being asked not
// to. Cheaper than a retry and it works nearly always.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// InvestigateTool exposes the supervisor to the model as an ordinary tool.
//
// Making it a tool rather than a separate mode is what keeps both front-ends
// working unchanged: the agent decides for itself when a request has
// independent parts worth fanning out, and the terminal and the browser see it
// as one more tool call. It satisfies tools.Tool structurally, so the tools
// package never has to know the agent package exists.
type InvestigateTool struct{ Sup *Supervisor }

func (InvestigateTool) Spec() components.ChatFunctionTool {
	return toolspec.Define("investigate",
		"Research a request that has several INDEPENDENT parts. It plans the parts, "+
			"researches them in parallel with read-only sub-agents that each get their own "+
			"context, and returns one write-up. Use it when a question spans separate "+
			"subjects; do not use it for a single question you could answer directly, and "+
			"never for anything that changes files or state.",
		`{"type":"object","properties":{"objective":{"type":"string"}},"required":["objective"]}`)
}

func (t InvestigateTool) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Objective string `json:"objective"`
	}
	if err := toolspec.Decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Objective) == "" {
		return "", fmt.Errorf("investigate needs an objective")
	}
	return t.Sup.Run(ctx, a.Objective)
}
