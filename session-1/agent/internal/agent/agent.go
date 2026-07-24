// Package agent drives the tool-calling loop: it asks the model what to do,
// runs any tools the model requests, and feeds the results back until the model
// produces a final answer.
package agent

import (
	"context"
	"fmt"

	openai "github.com/sashabaranov/go-openai"
)

// SystemPrompt tells the model how to behave and when to reach for a tool.
const SystemPrompt = `You are a helpful command-line assistant with access to tools.

Rules:
- Answer directly from your own knowledge when you can. Do NOT call a tool for
  things you already know (math, definitions, general facts).
- Use web_search only when the user needs current, external, or unknown facts.
- To create and run a script: write_file, then run_command, then read_file to
  check the result.
- To change an existing file: read_file first, then edit_file.
- Keep answers short and clear.`

// maxSteps caps how many tool rounds the model may take before we force a stop,
// so a misbehaving model can't loop forever.
const maxSteps = 12

// ToolBox is the set of tools the agent can use. tools.Registry satisfies it;
// depending on an interface keeps this package free of tool implementations.
type ToolBox interface {
	Specs() []openai.Tool
	Dispatch(ctx context.Context, name, args string) string
}

// Agent holds one running conversation and the loop that advances it.
type Agent struct {
	client  *openai.Client
	model   string
	tools   ToolBox
	history []openai.ChatCompletionMessage

	// OnToolCall, if set, is notified for each tool the agent runs — used by the
	// UI to show what's happening. Optional.
	OnToolCall func(name, args, result string)
}

// New starts an agent with the system prompt already in its history.
func New(client *openai.Client, model string, tools ToolBox) *Agent {
	return &Agent{
		client: client,
		model:  model,
		tools:  tools,
		history: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: SystemPrompt},
		},
	}
}

// Ask sends one user message and returns the agent's final answer, running any
// tools the model requests along the way. The conversation is remembered, so
// follow-up questions have context.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	a.history = append(a.history, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: input,
	})

	for step := 0; step < maxSteps; step++ {
		reply, err := a.think(ctx)
		if err != nil {
			return "", err
		}
		a.history = append(a.history, reply)

		if len(reply.ToolCalls) == 0 {
			return reply.Content, nil // a plain message = the final answer
		}
		a.runTools(ctx, reply.ToolCalls)
	}
	return "", fmt.Errorf("stopped after %d tool steps without a final answer", maxSteps)
}

// think asks the model for its next move given the conversation so far.
func (a *Agent) think(ctx context.Context) (openai.ChatCompletionMessage, error) {
	resp, err := a.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    a.model,
		Messages: a.history,
		Tools:    a.tools.Specs(),
	})
	if err != nil {
		return openai.ChatCompletionMessage{}, err
	}
	return resp.Choices[0].Message, nil
}

// runTools executes every tool the model asked for and appends the results to
// the conversation so the next round can use them.
func (a *Agent) runTools(ctx context.Context, calls []openai.ToolCall) {
	for _, call := range calls {
		result := a.tools.Dispatch(ctx, call.Function.Name, call.Function.Arguments)
		if a.OnToolCall != nil {
			a.OnToolCall(call.Function.Name, call.Function.Arguments, result)
		}
		a.history = append(a.history, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    result,
			ToolCallID: call.ID,
		})
	}
}
