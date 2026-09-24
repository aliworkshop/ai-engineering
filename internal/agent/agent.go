// Package agent is the loop: think, act, repeat, until the model answers.
//
// It knows there is a model and that there are tools, and nothing else. It does
// not know which tools exist (that is tools.Default), how they are built (that
// is main), or where the answer is printed (that is ui). Everything it needs
// from the outside arrives through a small interface or a callback, which is
// what makes the loop testable without a terminal, a network, or a real tool.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"

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

// Agent holds one running conversation and the loop that advances it.
type Agent struct {
	client *openrouter.OpenRouter
	model  string
	tools  ToolBox

	// history is the whole conversation, in the order it happened, and the
	// agent's entire memory. It is sent whole on every turn, which is why a
	// long chat gets slower and more expensive as it goes.
	//
	// BRITTLE STATE: it is a slice, and nothing else. Kill the process and the
	// conversation dies with it — mid-task, mid-spend, mid-sendReply — with no
	// way to know which of those three it was. Everything above this line is a
	// harness; this line is why it is not yet a runtime.
	//
	// The system prompt is message zero and never moves. Anything that trims
	// this later has to keep it: an agent that compacts away its own
	// instructions forgets what it is mid-conversation.
	history []Msg

	// bus is where the agent reports what it is doing. Optional: a nil bus is
	// a silent agent, not a broken one, which is what keeps the loop runnable
	// from a test with no terminal in sight.
	bus events.Emitter
}

// WithEvents points the agent at an event stream. Separate from New because
// every service in this harness is opt-in: the loop is the same loop with or
// without one, and only its visibility changes.
func (a *Agent) WithEvents(bus events.Emitter) *Agent {
	a.bus = bus
	return a
}

// New starts an agent with the standing prompt and the tools it was given.
func New(client *openrouter.OpenRouter, model string, tools ToolBox) *Agent {
	return &Agent{
		client: client,
		model:  model,
		tools:  tools,
		history: []Msg{
			{Role: "system", Text: SystemPrompt},
		},
	}
}

// Ask answers one question, running whatever tools the model asks for along the
// way, and returns its final reply.
//
// One Ask is one WORKFLOW: it gets an id, and every event it produces carries
// that id, so two runs interleaved in a log can still be told apart. The id is
// the only thing about this run that outlives it — which is to say, nothing
// about this run outlives it. See the note on history above.
//
// A failed turn rewinds the history to where it started. A turn can append
// several messages — the model's request, each tool's result — and a half
// written turn is worse than no turn: a tool call with no result is a
// conversation the API will refuse on the next question.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	wf := newWorkflowID()
	events.Emit(a.bus, events.Event{
		Type: events.WorkflowStarted, Workflow: wf, Input: input,
	})

	start := len(a.history)
	a.history = append(a.history, Msg{Role: "user", Text: input})

	answer, err := a.run(ctx, wf)
	if err != nil {
		a.history = a.history[:start]
		events.Emit(a.bus, events.Event{
			Type: events.WorkflowFailed, Workflow: wf, Error: err.Error(),
		})
		return "", err
	}

	events.Emit(a.bus, events.Event{
		Type: events.WorkflowCompleted, Workflow: wf, Output: answer,
	})
	return answer, nil
}

// newWorkflowID names one run. Eight hex characters is enough to tell today's
// runs apart and short enough to read in a stream, and crypto/rand keeps it a
// name rather than a sequence — nothing here is counting workflows.
func newWorkflowID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// History exposes the conversation for inspection. It returns a copy, because
// the loop rewrites this slice as it works and a caller holding the original
// would be reading a moving target.
func (a *Agent) History() []Msg {
	return append([]Msg(nil), a.history...)
}

// run is the loop itself: think, run whatever tools the model asked for, think
// again with the results, and stop when a reply carries no tool calls. That
// sentence is the whole agent.
func (a *Agent) run(ctx context.Context, wf string) (string, error) {
	for step := 0; step < maxSteps; step++ {
		reply, err := a.think(ctx)
		if err != nil {
			return "", err
		}
		a.history = append(a.history, reply)

		// No tool calls means the model is answering rather than acting.
		if len(reply.ToolCalls) == 0 {
			return reply.Text, nil
		}
		a.runTools(ctx, wf, reply.ToolCalls)
	}
	return "", fmt.Errorf("hit the step limit after %d tool rounds", maxSteps)
}

// think is one model call: the whole conversation goes out, with the tools the
// model is allowed to ask for, and one reply comes back.
func (a *Agent) think(ctx context.Context) (Msg, error) {
	res, err := a.client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(a.model),
		Messages: toSDK(a.history),
		Tools:    a.tools.Specs(),
	}, nil)
	if err != nil {
		return Msg{}, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return Msg{}, fmt.Errorf("model returned no choices")
	}
	return fromAssistant(res.ChatResult.Choices[0].Message), nil
}

// runTools runs each requested tool and appends its result to the conversation.
//
// The model never runs anything itself — it only asks, and this is the code
// that decides what actually happens. Dispatch turns a failure into text rather
// than an error, so a broken tool becomes something the model can read and work
// around instead of the end of the conversation.
//
// Two events per call, and the gap between them is the interesting part: it is
// where a harness would check a policy, ask a human, or write the call down
// before running it. Today there is no gap. The tool runs the moment it is
// requested, sendReply included.
func (a *Agent) runTools(ctx context.Context, wf string, calls []ToolCall) {
	for _, call := range calls {
		events.Emit(a.bus, events.Event{
			Type: events.ToolRequested, Workflow: wf,
			Name: call.Name, Call: call.ID, Args: call.Args,
		})

		result := a.tools.Dispatch(ctx, call.Name, call.Args)

		events.Emit(a.bus, events.Event{
			Type: events.ToolCompleted, Workflow: wf,
			Name: call.Name, Call: call.ID,
		})
		a.history = append(a.history, Msg{
			Role: "tool", Text: result, ToolCallID: call.ID,
		})
	}
}
