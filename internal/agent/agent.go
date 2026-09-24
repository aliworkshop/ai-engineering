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
	"fmt"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// maxSteps caps how many tool rounds one question may take before we give up.
// Without it, a model that keeps asking for the same tool loops until your
// credit does.
const maxSteps = 6

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
	// The system prompt is message zero and never moves. Anything that trims
	// this later has to keep it: an agent that compacts away its own
	// instructions forgets what it is mid-conversation.
	history []Msg

	// OnToolCall, if set, is notified for each tool the agent runs. It is how
	// the UI shows what is happening without this package importing a terminal
	// — the loop reports, the front-end decides what that looks like.
	OnToolCall func(name, args, result string)
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
// A failed turn rewinds the history to where it started. A turn can append
// several messages — the model's request, each tool's result — and a half
// written turn is worse than no turn: a tool call with no result is a
// conversation the API will refuse on the next question.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	start := len(a.history)
	a.history = append(a.history, Msg{Role: "user", Text: input})

	answer, err := a.run(ctx)
	if err != nil {
		a.history = a.history[:start]
		return "", err
	}
	return answer, nil
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
func (a *Agent) run(ctx context.Context) (string, error) {
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
		a.runTools(ctx, reply.ToolCalls)
	}
	return "", fmt.Errorf("stopped after %d tool rounds without an answer", maxSteps)
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
func (a *Agent) runTools(ctx context.Context, calls []ToolCall) {
	for _, call := range calls {
		result := a.tools.Dispatch(ctx, call.Name, call.Args)
		if a.OnToolCall != nil {
			a.OnToolCall(call.Name, call.Args, result)
		}
		a.history = append(a.history, Msg{
			Role: "tool", Text: result, ToolCallID: call.ID,
		})
	}
}
