// Package agent is the loop: think, act, repeat, until the model answers.
//
// It knows there is a model and that there are tools, and nothing else. It does
// not know which tools exist (that is tools.Default), how they are built (that
// is main), or where the answer is printed (that is ui). Everything it needs
// from the outside arrives through a small interface or a callback, which is
// what makes the loop testable without a terminal, a network, or a real tool.
//
// Since Part 2 the loop is also DETERMINISTIC, and that word is load-bearing.
// Every model call and every tool call is a checkpointed step, and everything
// else in the body — building the message list, deciding what to do next — is
// derived from the workflow's own input and the results of those steps. That
// is what makes a replay exact: re-run the body after a crash and it rebuilds
// itself from disk, without a model call or a side effect, up to the moment it
// died.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
)

// maxSteps caps how many tool rounds one question may take before we give up.
// Without it, a model that keeps asking for the same tool loops until your
// credit does.
const maxSteps = 10

// ToolBox is the set of tools the agent can use. tools.Registry satisfies it;
// depending on the interface rather than the struct keeps this package free of
// tool implementations — and lets a test pass three lines of stub.
type ToolBox interface {
	Specs() []components.ChatFunctionTool
	Dispatch(ctx context.Context, name, args string) string
}

// Store is where workflows live, if one is wired in. Declared as an interface
// for the same reason as ToolBox — and because an agent with no store is still
// a working agent, just one that cannot survive a crash. That is the Part 1
// agent, and it is kept: the claim only means something if you can run the
// other version.
type Store interface {
	Open(id, input string) (*durable.Workflow, error)
}

// Agent holds the loop and everything it was wired with.
//
// Notice what it no longer holds: a conversation. A run's messages are local
// to that run, rebuilt from the workflow's input and its checkpointed steps.
// A field would have survived across REPL turns and quietly broken replay — a
// resumed run in a fresh process would have rebuilt a different context, and
// the first live model call after recovery would have seen a conversation the
// original never had.
type Agent struct {
	client *openrouter.OpenRouter
	model  string
	tools  ToolBox

	// store, bus and crashAt are all optional. A nil store is the brittle
	// loop, a nil bus is a silent one, and crashAt below zero never fires.
	store   Store
	bus     events.Emitter
	crashAt int
}

// New starts an agent with the standing prompt and the tools it was given.
func New(client *openrouter.OpenRouter, model string, tools ToolBox) *Agent {
	return &Agent{client: client, model: model, tools: tools, crashAt: -1}
}

// WithEvents points the agent at an event stream.
func (a *Agent) WithEvents(bus events.Emitter) *Agent {
	a.bus = bus
	return a
}

// WithStore makes the agent durable: each task becomes a workflow whose model
// turns and tool calls are checkpointed. One line, and it is the whole
// difference between a script and a runtime.
func (a *Agent) WithStore(store Store) *Agent {
	a.store = store
	return a
}

// WithCrashAt is a DEMO HOOK, not a feature: before the given step the process
// exits, simulating a machine dying mid-task after real side effects have
// already happened. It is here because the durability claim is worth nothing
// until you have watched it fail and recover.
func (a *Agent) WithCrashAt(step int) *Agent {
	a.crashAt = step
	return a
}

// Ask works one task and returns the answer. One Ask is one WORKFLOW: it gets
// an id, and with a store behind it, it gets a file.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	return a.run(ctx, newWorkflowID(), input)
}

// Resume re-runs a workflow that stopped early. There is no separate recovery
// code path: replay serves the checkpointed steps from disk — no model calls,
// no repeated side effects — and execution races forward to exactly where it
// stopped.
func (a *Agent) Resume(ctx context.Context, workflowID string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("agent: cannot resume without a durable store")
	}
	return a.run(ctx, workflowID, "")
}

// newWorkflowID names one run. Eight hex characters is enough to tell today's
// runs apart and short enough to read in a stream.
func newWorkflowID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// run opens the workflow, drives the loop, and records how it ended.
func (a *Agent) run(ctx context.Context, id, input string) (string, error) {
	// No store: the Part 1 loop, unchanged and honest about it.
	if a.store == nil {
		events.Emit(a.bus, events.Event{Type: events.WorkflowStarted, Workflow: id, Input: input})
		answer, err := a.loop(ctx, nil, id, input)
		a.finished(id, answer, err)
		return answer, err
	}

	wf, err := a.store.Open(id, input)
	if err != nil {
		return "", err
	}

	started := events.WorkflowStarted
	if wf.Resumed() {
		started = events.WorkflowResumed
	}
	events.Emit(a.bus, events.Event{Type: started, Workflow: wf.ID(), Input: wf.Input()})

	answer, err := a.loop(ctx, wf, wf.ID(), wf.Input())
	if err != nil {
		// Failed, not finished: the file stays on disk with every completed
		// step in it, which is what makes the next attempt cheap.
		_ = wf.Finish(durable.StatusFailed)
		a.finished(id, answer, err)
		return "", err
	}
	if err := wf.Finish(durable.StatusDone); err != nil {
		return "", err
	}
	a.finished(id, answer, nil)
	return answer, nil
}

// finished emits the one event that says how a run ended.
func (a *Agent) finished(id, answer string, err error) {
	if err != nil {
		events.Emit(a.bus, events.Event{Type: events.WorkflowFailed, Workflow: id, Error: err.Error()})
		return
	}
	events.Emit(a.bus, events.Event{Type: events.WorkflowCompleted, Workflow: id, Output: answer})
}

// loop is the agent itself: think, run whatever tools the model asked for,
// think again with the results, and stop when a reply carries no tool calls.
//
// Everything non-deterministic happens inside a step. Everything outside one —
// this slice, this counter, this if — is rebuilt identically on a replay,
// which is the golden rule of durable workflows and the only reason recovery
// can be this simple.
func (a *Agent) loop(ctx context.Context, wf *durable.Workflow, id, input string) (string, error) {
	working := []Msg{
		{Role: "system", Text: SystemPrompt},
		{Role: "user", Text: input},
	}

	for i := 0; i < maxSteps; i++ {
		a.maybeCrash(wf, i)

		name := fmt.Sprintf("model-%02d", i)
		reply, err := step(wf, name, func() (Msg, error) { return a.think(ctx, working) })
		if err != nil {
			return "", err
		}
		working = append(working, reply)

		// No tool calls means the model is answering rather than acting.
		if len(reply.ToolCalls) == 0 {
			return reply.Text, nil
		}

		for _, call := range reply.ToolCalls {
			result, err := a.runTool(ctx, wf, id, call)
			if err != nil {
				return "", err
			}
			working = append(working, Msg{Role: "tool", Text: result, ToolCallID: call.ID})
		}
	}
	return "", fmt.Errorf("hit the step limit after %d tool rounds", maxSteps)
}

// runTool runs one tool as a checkpointed step.
//
// The two tool events are emitted from INSIDE the step, which is a small
// decision with a large consequence. A replay does not re-run the step, so it
// does not re-emit them — which means a tool.requested in the log is a tool
// that actually ran. Counting them is then a real answer to "did any side
// effect happen twice?", and that is exactly what -audit counts. Emit them
// outside the step and every successful recovery would look like a duplicate.
func (a *Agent) runTool(ctx context.Context, wf *durable.Workflow, id string, call ToolCall) (string, error) {
	return step(wf, "tool-"+call.ID, func() (string, error) {
		events.Emit(a.bus, events.Event{
			Type: events.ToolRequested, Workflow: id,
			Name: call.Name, Call: call.ID, Args: call.Args,
		})

		// Dispatch turns a failure into text rather than an error, so a broken
		// tool becomes something the model can read and work around instead of
		// the end of the conversation — and, here, instead of a step that
		// refuses to checkpoint.
		result := a.tools.Dispatch(ctx, call.Name, call.Args)

		events.Emit(a.bus, events.Event{
			Type: events.ToolCompleted, Workflow: id,
			Name: call.Name, Call: call.ID,
		})
		return result, nil
	})
}

// step runs fn as a checkpointed step when there is a workflow, and just runs
// it when there is not. One code path for the durable agent and the brittle
// one, so the comparison is between two runs of the same loop rather than
// between two loops.
//
// A free function because Go does not allow type parameters on methods.
func step[T any](wf *durable.Workflow, name string, fn func() (T, error)) (T, error) {
	if wf == nil {
		return fn()
	}
	return durable.Step(wf, name, fn)
}

// maybeCrash is the demo hook. It fires only when the step has NOT already
// been checkpointed, so a recovery run replays straight past the crash point
// instead of dying there forever.
func (a *Agent) maybeCrash(wf *durable.Workflow, i int) {
	if a.crashAt < 0 || i != a.crashAt {
		return
	}
	if wf != nil && wf.Cached(fmt.Sprintf("model-%02d", i)) {
		return
	}
	fmt.Printf("\n  💥 simulated crash before step %d — the process is gone.\n", i)
	os.Exit(1)
}

// think is one model call: the conversation so far goes out, with the tools
// the model is allowed to ask for, and one reply comes back.
func (a *Agent) think(ctx context.Context, working []Msg) (Msg, error) {
	return Think(ctx, a.client, a.model, working, a.tools.Specs())
}

// Think is one model call, outside the loop: given a conversation and a
// toolset, what does the model want to do next?
//
// It is exported for one caller — the DBOS runner in internal/dbosrun, which
// drives the same steps through a different durable engine. Sharing this
// rather than letting that package write its own model call is what keeps the
// comparison honest: the two engines are running the same agent.
func Think(ctx context.Context, client *openrouter.OpenRouter, model string, working []Msg, specs []components.ChatFunctionTool) (Msg, error) {
	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(model),
		Messages: toSDK(working),
		Tools:    specs,
	}, nil)
	if err != nil {
		return Msg{}, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return Msg{}, fmt.Errorf("model returned no choices")
	}
	return fromAssistant(res.ChatResult.Choices[0].Message), nil
}
