// Package agent drives the tool-calling loop — but the loop is only half of
// what is here. The other half is the harness around it: the layer that decides
// what the model sees, checkpoints what it did, gates what it is allowed to do,
// and can pick the whole thing up again after the process dies.
//
// One sentence holds the design together, and every part of this package is
// that sentence applied again:
//
//	The LLM decides the next semantic step. The harness owns execution.
//
// The loop below is the spine, with each line's owner named:
//
//	workflow := store.Open(id, task)        // Part 2 · durable state
//	for {
//	    context := memory.Context(working)  // Part 4 · assembled fresh, bounded
//	    reply   := durable.Step(model)      // Parts 1, 5 · the LLM decides
//	    gate.Scope(call.ID)                 // Part 7 · approval as a durable state
//	    result  := durable.Step(tool)       // Part 3 · sandbox · resume from exactly here
//	    if handoff { switch specialist }    // Part 5 · lateral control transfer
//	}
package agent

import (
	"context"
	"fmt"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// maxSteps caps how many tool rounds the model may take before we force a stop,
// so a misbehaving model can't loop forever.
const maxSteps = 12

// maxHandoffs caps lateral transfers within one task. Two specialists that each
// think the other should handle it will ping-pong forever otherwise, and the
// symptom — a slowly climbing bill and no answer — is miserable to diagnose.
const maxHandoffs = 4

// compactPrompt instructs the model to distill older turns into a summary dense
// enough that follow-up questions still have the context they need.
const compactPrompt = `Summarize the conversation below into a concise briefing for continuing it.
Preserve: what the user is trying to do, decisions and answers reached, any file
paths / commands / values that matter, and anything still unresolved. Drop
pleasantries and redundant detail. Write it as notes, not prose.`

// ToolBox is the set of tools the agent can use. tools.Registry satisfies it;
// depending on an interface keeps this package free of tool implementations.
type ToolBox interface {
	Specs() []components.ChatFunctionTool
	Dispatch(ctx context.Context, name, args string) string
}

// Gate is the durable approval gate, if one is wired in (Part 7). The agent
// needs exactly two things from it: to say which tool call the next approval
// belongs to, and to find out afterwards whether the workflow parked itself
// rather than getting an answer.
type Gate interface {
	Scope(callID string)
	Parked() error
}

// Store is the durable workflow store, if one is wired in (Part 2). Declared as
// an interface for the same reason as ToolBox — and because an agent with no
// store is still a working agent, just one that cannot survive a crash.
type Store interface {
	Open(id, input string) (*durable.Workflow, error)
}

// Agent holds one running conversation and the loop that advances it.
type Agent struct {
	client *openrouter.OpenRouter
	model  string
	tools  ToolBox

	// mem is Part 4: history stays out of it, state lives in Summary, and
	// context is assembled per turn.
	mem *Memory

	// roster and current are Part 5. An empty roster means single-agent mode,
	// where tools and mem.System are used directly and handoff never fires.
	roster  Roster
	current string

	// store, gate and bus are the harness services. All three are optional, and
	// the agent degrades to a plain loop without them — which is what keeps the
	// eval harness and the unit tests from having to build a runtime.
	store Store
	gate  Gate
	bus   events.Emitter

	// OnToolCall, if set, is notified for each tool the agent runs — used by the
	// UI to show what's happening. Optional.
	OnToolCall func(name, args, result string)

	// OnCompact, if set, is notified after the history is compacted, with the
	// summary that replaced it — lets the UI tell the user it happened. Optional.
	OnCompact func(summary string)
}

// New starts an agent with the single-agent prompt and every tool it was given.
func New(client *openrouter.OpenRouter, model string, tools ToolBox) *Agent {
	return &Agent{
		client: client,
		model:  model,
		tools:  tools,
		mem:    NewMemory(SystemPrompt),
	}
}

// WithRoster puts the agent in multi-agent mode, starting at the named spec.
// The roster's prompt and tool subset replace the ones New installed, so the
// agent you talk to holds only what that spec holds (Part 5).
func (a *Agent) WithRoster(roster Roster, start string) *Agent {
	spec, ok := roster[start]
	if !ok {
		return a // an unknown starting agent leaves single-agent mode intact
	}
	a.roster, a.current = roster, start
	a.tools, a.mem.System = spec.Tools, spec.Prompt
	return a
}

// WithStore makes the agent durable: each task becomes a workflow whose model
// turns and tool calls are checkpointed (Part 2).
func (a *Agent) WithStore(store Store) *Agent {
	a.store = store
	return a
}

// WithGate wires in the durable approval gate (Part 7).
func (a *Agent) WithGate(gate Gate) *Agent {
	a.gate = gate
	return a
}

// WithEvents points the agent's event stream at a sink (Part 1).
func (a *Agent) WithEvents(bus events.Emitter) *Agent {
	a.bus = bus
	return a
}

// Memory exposes working memory, for a UI or a test that wants to see what the
// agent is actually carrying.
func (a *Agent) Memory() *Memory { return a.mem }

// Current reports which agent in the roster holds the conversation.
func (a *Agent) Current() string {
	if a.current == "" {
		return AssistantName
	}
	return a.current
}

// Ask answers one question, running any tools the model requests along the way.
//
// With a store wired in, the whole question is one durable workflow: kill the
// process halfway through and calling Resume with the same id replays the
// checkpointed steps — no repeated model calls, no repeated side effects — and
// carries on from exactly where it stopped.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	answer, _, err := a.ask(ctx, input, "")
	return answer, err
}

// AskDurable is Ask with the workflow id surfaced, so a caller that gets a
// suspension back knows which id to approve. The id is generated when empty.
func (a *Agent) AskDurable(ctx context.Context, input string) (answer, workflowID string, err error) {
	return a.ask(ctx, input, "")
}

// Resume re-runs a workflow that stopped early — because it crashed, or because
// it parked itself waiting for a human. There is no separate recovery path:
// resuming is running, with the steps already done served from cache.
func (a *Agent) Resume(ctx context.Context, workflowID string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("agent: cannot resume without a durable store")
	}
	answer, _, err := a.ask(ctx, "", workflowID)
	return answer, err
}

func (a *Agent) ask(ctx context.Context, input, workflowID string) (string, string, error) {
	// Without a store this is an ordinary in-memory loop — brittle, and honest
	// about it. Everything below still runs; the steps just aren't checkpointed.
	var wf *durable.Workflow
	if a.store != nil {
		if workflowID == "" {
			workflowID = newWorkflowID()
		}
		opened, err := a.store.Open(workflowID, input)
		if err != nil {
			return "", workflowID, err
		}
		wf = opened
		if input == "" {
			input = wf.Input() // resuming: the task came off disk
		}
		// Note what is deliberately NOT done here: the workflow records which
		// specialist held the conversation, but we do not restore it before
		// replaying. Replay re-derives the handoff from the cached tool result
		// that caused it, at the same point in the run — and pre-applying it
		// would put the loop in a state the first pass was never in. The
		// recorded agent is for the human reading `-list`, not for the runtime.
		if binder, ok := a.gate.(attachable); ok {
			binder.Attach(wf)
		}
	}

	answer, err := a.run(ctx, wf, input)

	switch {
	case err == nil:
		if wf != nil {
			_ = wf.Finish(durable.StatusDone)
		}
		events.Emit(a.bus, events.Event{
			Type: events.WorkflowCompleted, Workflow: workflowID,
			Agent: a.Current(), Text: truncate(answer, 80),
		})

	default:
		if _, parked := durable.IsSuspended(err); parked {
			// Already marked suspended by the gate, and deliberately not
			// finished: the workflow is alive on disk with a question
			// outstanding, which is a state, not a failure.
			return "", workflowID, err
		}
		if wf != nil {
			_ = wf.Finish(durable.StatusFailed)
		}
		events.Emit(a.bus, events.Event{
			Type: events.WorkflowFailed, Workflow: workflowID,
			Agent: a.Current(), Error: err.Error(),
		})
	}
	return answer, workflowID, err
}

// attachable is the optional half of Gate: a gate that tracks workflows needs
// to be told which one is running. Kept out of the Gate interface so a simpler
// gate — a test stub, say — doesn't have to implement it.
type attachable interface{ Attach(*durable.Workflow) }

func (a *Agent) run(ctx context.Context, wf *durable.Workflow, input string) (string, error) {
	// working is the turn in flight: the question plus everything that happens
	// while answering it. It is committed to memory as one unit at the end, so
	// a turn that fails halfway never leaves a half-turn behind for the next
	// request to trip over.
	working := []Msg{{Role: "user", Text: input}}
	handoffs := 0

	for step := 0; step < maxSteps; step++ {
		// 1. Assemble context fresh and ask the model what to do next. The
		//    model turn is a durable step: on replay it returns the same reply
		//    without a second API call, which is what makes the tool call ids
		//    below stable across a crash.
		reply, err := a.think(ctx, wf, step, working)
		if err != nil {
			return "", err
		}
		working = append(working, reply)

		events.Emit(a.bus, events.Event{
			Type: events.ModelCompleted, Workflow: workflowIDOf(wf),
			Agent: a.Current(), Tokens: EstimateTokens(a.mem.Context(working)),
		})

		// 2. No tools requested? A plain message is the final answer, and the
		//    conversation is at a safe boundary — no tool call awaiting a
		//    result — which is the only moment compaction may run.
		if len(reply.ToolCalls) == 0 {
			a.mem.Commit(working)
			a.maybeCompact(ctx)
			return reply.Text, nil
		}

		// 3. Run the tools it asked for.
		transferred, err := a.runTools(ctx, wf, reply.ToolCalls, &working)
		if err != nil {
			return "", err
		}
		if transferred != "" {
			handoffs++
			if handoffs > maxHandoffs {
				return "", fmt.Errorf("agent: handed off %d times without finishing", handoffs)
			}
			// A handoff swaps the prompt and the toolset mid-task. The turn so
			// far is kept: the specialist should see how the conversation got
			// here, not start from a one-line brief.
			if err := a.handOff(wf, transferred); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("stopped after %d tool steps without a final answer", maxSteps)
}

// think asks the model for its next move, wrapped in a durable step.
func (a *Agent) think(ctx context.Context, wf *durable.Workflow, step int, working []Msg) (Msg, error) {
	call := func() (Msg, error) {
		reply, err := a.send(ctx, a.mem.Context(working), a.tools.Specs())
		if err != nil {
			return Msg{}, err
		}
		return fromAssistant(reply), nil
	}
	if wf == nil {
		return call()
	}
	// Keyed by position ALONE, deliberately — not by the current agent.
	//
	// Folding the agent's name in reads like extra safety and is the opposite.
	// A handoff happens partway through a run, so the same position is
	// "assistant" on the first pass and "operator" on the replay; the names
	// stop matching, the replay misses its cache, and it makes a fresh model
	// call that produces fresh tool call ids — which then miss the approval
	// recorded against the old ones. The whole run diverges, and the symptom is
	// a resumed workflow asking for the same approval a second time.
	//
	// Position is the one key that is stable, because replay re-runs the same
	// loop in the same order. Which agent is speaking is already determined by
	// the cached handoff result that got it there.
	return durable.Step(wf, fmt.Sprintf("model-%02d", step), call)
}

// runTools executes every tool the model asked for and appends the results to
// the turn. It returns the name of an agent to hand off to, if one was
// requested, and an error if the workflow parked itself waiting for a human.
func (a *Agent) runTools(ctx context.Context, wf *durable.Workflow, calls []ToolCall, working *[]Msg) (string, error) {
	var transferTo string

	for _, call := range calls {
		events.Emit(a.bus, events.Event{
			Type: events.ToolRequested, Workflow: workflowIDOf(wf),
			Agent: a.Current(), Name: call.Name, Args: compactArgs(call.Args),
		})

		// Tell the gate which call the approval it is about to be asked for
		// belongs to, so the human's answer is checkpointed under an id that
		// survives a replay (Part 7).
		if a.gate != nil {
			a.gate.Scope(call.ID)
		}

		// A park comes back as an error, so it unwinds before anything is
		// recorded — see dispatch for why that ordering is the whole trick.
		result, err := a.dispatch(ctx, wf, call)
		if err != nil {
			return "", err
		}

		events.Emit(a.bus, events.Event{
			Type: events.ToolCompleted, Workflow: workflowIDOf(wf),
			Agent: a.Current(), Name: call.Name, Result: truncate(result, 70),
		})
		if a.OnToolCall != nil {
			a.OnToolCall(call.Name, call.Args, result)
		}
		*working = append(*working, Msg{Role: "tool", Text: result, ToolCallID: call.ID})

		// A handoff is a tool result like any other in the transcript, but the
		// harness is what acts on it — a tool cannot swap out the agent that
		// called it. Later calls in the same batch still run: they were part of
		// the same decision, and dropping them silently would confuse the model
		// more than finishing them does.
		if h, ok := decodeHandoff(result); ok && a.roster != nil {
			if _, known := a.roster[h.To]; known && h.To != a.Current() {
				transferTo = h.To
			}
		}
	}
	return transferTo, nil
}

// dispatch runs one tool, as a durable step when there is a workflow. The step
// is keyed by the model's own call id, so the checkpoint and the approval
// recorded against that same id line up on replay.
//
// The park check lives INSIDE the step, and that placement is load-bearing.
// When no human answers, the gate returns false and the tool politely reports
// "denied" — which is a perfectly good string, and checkpointing it would be a
// disaster: the resumed run would replay that cached refusal forever and the
// approved action would never happen. Turning the park into an error instead
// means Step declines to cache anything, and the replay after approval reaches
// this exact call again, which is the entire point of parking.
func (a *Agent) dispatch(ctx context.Context, wf *durable.Workflow, call ToolCall) (string, error) {
	run := func() (string, error) {
		result := a.tools.Dispatch(ctx, call.Name, call.Args)
		if a.gate != nil {
			if parked := a.gate.Parked(); parked != nil {
				return "", parked
			}
		}
		return result, nil
	}
	if wf == nil {
		return run()
	}
	return durable.Step(wf, "tool-"+call.ID, run)
}

// handOff switches the running spec. The switch is checkpointed in the workflow
// file, not just in memory, so a crash mid-task resumes as the specialist
// rather than bouncing back to the generalist and starting the argument again.
func (a *Agent) handOff(wf *durable.Workflow, to string) error {
	from := a.Current()
	a.switchTo(to)
	events.Emit(a.bus, events.Event{
		Type: events.AgentHandoff, Workflow: workflowIDOf(wf), From: from, To: to,
	})
	if wf != nil {
		return wf.SetAgent(to)
	}
	return nil
}

func (a *Agent) switchTo(name string) {
	spec, ok := a.roster[name]
	if !ok {
		return
	}
	a.current = name
	a.tools = spec.Tools
	a.mem.System = spec.Prompt
}

// maybeCompact folds the oldest turns into the summary once the assembled
// context outgrows its budget. Best-effort: if summarizing fails, the full
// turns are kept, because a lost summary costs tokens and a lost turn costs
// the conversation.
func (a *Agent) maybeCompact(ctx context.Context) {
	if !a.mem.Overflowing() {
		return
	}
	before := EstimateTokens(a.mem.Context(nil))

	old := a.mem.Overflow()
	if len(old) == 0 {
		return
	}

	summary, err := a.summarize(ctx, old)
	if err != nil || strings.TrimSpace(summary) == "" {
		// Put them back. Dropping turns we failed to summarize would be the
		// one failure mode worse than an oversized prompt: silent amnesia.
		a.mem.Turns = append(old, a.mem.Turns...)
		if a.OnCompact != nil && err != nil {
			a.OnCompact("(compaction skipped: " + err.Error() + ")")
		}
		return
	}

	a.mem.Summary = summary
	events.Emit(a.bus, events.Event{
		Type: events.MemoryCompacted, Agent: a.Current(),
		Tokens: EstimateTokens(a.mem.Context(nil)), Text: truncate(summary, 80),
		Millis: int64(before), // the before-size, so the log shows the saving
	})
	if a.OnCompact != nil {
		a.OnCompact(summary)
	}
}

// summarize folds turns into a briefing, folding in whatever summary is already
// there so nothing is lost across repeated compactions.
func (a *Agent) summarize(ctx context.Context, old []Turn) (string, error) {
	prior := a.mem.Summary
	if prior == "" {
		prior = "(none)"
	}
	reply, err := a.send(ctx, []Msg{
		{Role: "system", Text: compactPrompt},
		{Role: "user", Text: "Prior summary:\n" + prior +
			"\n\nFold in this newer work:\n" + Transcript(old) +
			"\n\nReturn the updated summary."},
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(assistantText(reply)), nil
}

// send issues one chat completion and returns the assistant message from the
// first choice. It's the single place we call the OpenRouter SDK, so the main
// loop, compaction, and the supervisor share one request shape and one error
// path.
func (a *Agent) send(ctx context.Context, messages []Msg, toolSpecs []components.ChatFunctionTool) (components.ChatAssistantMessage, error) {
	return send(ctx, a.client, a.model, messages, toolSpecs)
}

func send(ctx context.Context, client *openrouter.OpenRouter, model string, messages []Msg, toolSpecs []components.ChatFunctionTool) (components.ChatAssistantMessage, error) {
	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(model),
		Messages: toSDK(messages),
		Tools:    toolSpecs,
	}, nil)
	if err != nil {
		return components.ChatAssistantMessage{}, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return components.ChatAssistantMessage{}, fmt.Errorf("model returned no choices")
	}
	return res.ChatResult.Choices[0].Message, nil
}

func workflowIDOf(wf *durable.Workflow) string {
	if wf == nil {
		return ""
	}
	return wf.ID()
}

func truncate(s string, max int) string {
	flat := []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(s))
	if len(flat) <= max {
		return string(flat)
	}
	return string(flat[:max]) + "…"
}
