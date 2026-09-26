// Package agent is what the agent IS: its vocabulary, its standing
// instructions, the tools it may reach for, and the one call that asks a model
// what to do next.
//
// It is deliberately not how the agent RUNS. The loop, durability, recovery
// and the event stream all live one layer out, in package runtime — because
// with a real durable engine underneath, the loop has to be a plain function
// that engine can register and resurrect by name, and that is a property of
// the runtime rather than of the agent.
//
// What is left here is small and has no opinion about execution: it needs a
// model client and a tool box, and it can be exercised from a test with
// neither a database nor a terminal in sight.
package agent

import (
	"context"
	"fmt"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// ToolBox is the set of tools the agent can use. tools.Registry satisfies it;
// depending on the interface rather than the struct keeps this package free of
// tool implementations — and lets a test pass three lines of stub.
type ToolBox interface {
	Specs() []components.ChatFunctionTool
	Dispatch(ctx context.Context, name, args string) string
}

// Think is one model call: the conversation so far goes out, with the tools
// the model is allowed to ask for, and one reply comes back.
//
// This is the only thing in the agent that is non-deterministic, which is why
// the runtime wraps exactly this in a checkpointed step. It is a plain
// function rather than a method for the same reason: the caller that needs it
// is a workflow body, which cannot carry a receiver.
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
