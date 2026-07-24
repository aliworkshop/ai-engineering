// Package agent drives the tool-calling loop: it asks the model what to do,
// runs any tools the model requests, and feeds the results back until the model
// produces a final answer.
package agent

import (
	"context"
	"fmt"
	"strings"

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

// defaultCompactEvery is how many completed questions trigger a history
// compaction. Left unbounded, the conversation — every question, answer, tool
// call, and tool result — is resent on every request, so the token cost per
// turn keeps climbing. Compacting folds the running history into one short
// summary at a safe boundary, capping that growth.
const defaultCompactEvery = 5

// compactPrompt instructs the model to distill the conversation into a summary
// dense enough that follow-up questions still have the context they need.
const compactPrompt = `Summarize the conversation below into a concise briefing for continuing it.
Preserve: what the user is trying to do, decisions and answers reached, any file
paths / commands / values that matter, and anything still unresolved. Drop
pleasantries and redundant detail. Write it as notes, not prose.`

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
	turns   int // completed questions, for deciding when to compact

	// CompactEvery is how many completed questions trigger a history compaction.
	// New sets it to defaultCompactEvery; set it to 0 to disable compaction.
	CompactEvery int

	// OnToolCall, if set, is notified for each tool the agent runs — used by the
	// UI to show what's happening. Optional.
	OnToolCall func(name, args, result string)

	// OnCompact, if set, is notified after the history is compacted, with the
	// summary that replaced it — lets the UI tell the user it happened. Optional.
	OnCompact func(summary string)
}

// New starts an agent with the system prompt already in its history.
func New(client *openai.Client, model string, tools ToolBox) *Agent {
	return &Agent{
		client:       client,
		model:        model,
		tools:        tools,
		CompactEvery: defaultCompactEvery,
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
			// A plain message = the final answer. The conversation is now at a
			// safe boundary (no tool call awaits its result), so it's the right
			// moment to compact if we've hit the interval.
			a.turns++
			a.maybeCompact(ctx)
			return reply.Content, nil
		}
		a.runTools(ctx, reply.ToolCalls)
	}
	return "", fmt.Errorf("stopped after %d tool steps without a final answer", maxSteps)
}

// maybeCompact folds the history into a summary once every CompactEvery turns.
// It runs only at a safe boundary (see Ask) and is best-effort: if summarizing
// fails, the full history is kept so the conversation never loses context.
func (a *Agent) maybeCompact(ctx context.Context) {
	if a.CompactEvery <= 0 || a.turns%a.CompactEvery != 0 {
		return
	}
	if err := a.compact(ctx); err != nil && a.OnCompact != nil {
		a.OnCompact("(compaction skipped: " + err.Error() + ")")
	}
}

// compact folds the OLDER part of the conversation into one summary message
// while keeping the system prompt and the most recent turn verbatim. Sliding
// the window this way caps token growth without blurring the turn a follow-up
// question is most likely to reference (its exact file paths, commands, and
// outputs stay intact).
func (a *Agent) compact(ctx context.Context) error {
	// The most recent turn starts at the last user message. Everything from the
	// system prompt up to there is "older" and gets summarized; the recent turn
	// is kept as-is. Cutting on a user message keeps tool call/result pairs whole
	// on both sides, so the next request never sees an orphaned tool message.
	lastUser := -1
	for i := len(a.history) - 1; i >= 1; i-- {
		if a.history[i].Role == openai.ChatMessageRoleUser {
			lastUser = i
			break
		}
	}
	if lastUser <= 1 {
		return nil // at most one turn after the system prompt — nothing older to fold
	}

	older := a.history[1:lastUser]
	recent := a.history[lastUser:]

	resp, err := a.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: compactPrompt},
			{Role: openai.ChatMessageRoleUser, Content: renderTranscript(older)},
		},
	})
	if err != nil {
		return err
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return nil // nothing usable came back; keep the real history
	}

	compacted := make([]openai.ChatCompletionMessage, 0, 2+len(recent))
	compacted = append(compacted, a.history[0]) // system prompt, untouched
	compacted = append(compacted, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: "Summary of the conversation so far:\n" + summary,
	})
	compacted = append(compacted, recent...) // most recent turn, verbatim
	a.history = compacted

	if a.OnCompact != nil {
		a.OnCompact(summary)
	}
	return nil
}

// renderTranscript flattens messages into plain text the model can summarize,
// including tool calls and their results so no context is silently dropped.
func renderTranscript(msgs []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case openai.ChatMessageRoleUser:
			fmt.Fprintf(&b, "User: %s\n", m.Content)
		case openai.ChatMessageRoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "Assistant: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "Assistant called %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case openai.ChatMessageRoleTool:
			fmt.Fprintf(&b, "Tool result: %s\n", m.Content)
		}
	}
	return b.String()
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
