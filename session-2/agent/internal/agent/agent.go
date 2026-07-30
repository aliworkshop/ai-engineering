// Package agent drives the tool-calling loop: it asks the model what to do,
// runs any tools the model requests, and feeds the results back until the model
// produces a final answer.
package agent

import (
	"context"
	"fmt"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// SystemPrompt tells the model how to behave and when to reach for a tool.
const SystemPrompt = `You are a helpful command-line assistant with access to tools.

Rules:
- Answer directly from your own knowledge when you can. Do NOT call a tool for
  things you already know (math, definitions, general facts).
- Don't make up facts. If you don't know, say so.
- Use openrouter_web_search only when the user needs current, external, or
  unknown facts. It answers with source URLs — keep them in your reply.
- To create and run a script: write_file, then run_command, then read_file to
  check the result.
- To change an existing file: read_file first, then edit_file.
- If read_file, edit_file, or delete_file reports that a file is missing, do NOT
  conclude it doesn't exist. First locate it with run_command — e.g.
  "find . -name README.md" or "ls <dir>" — then retry with the real path.
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
	Specs() []components.ChatFunctionTool
	Dispatch(ctx context.Context, name, args string) string
}

// Agent holds one running conversation and the loop that advances it.
type Agent struct {
	client  *openrouter.OpenRouter
	model   string
	tools   ToolBox
	history []components.ChatMessages
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
func New(client *openrouter.OpenRouter, model string, tools ToolBox) *Agent {
	return &Agent{
		client:       client,
		model:        model,
		tools:        tools,
		CompactEvery: defaultCompactEvery,
		history: []components.ChatMessages{
			systemMsg(SystemPrompt),
		},
	}
}

// systemMsg, userMsg, and toolMsg build the SDK's role-tagged message union so
// the rest of the loop can construct history entries without repeating the
// verbose constructor calls.
func systemMsg(text string) components.ChatMessages {
	return components.CreateChatMessagesSystem(components.ChatSystemMessage{
		Role:    components.ChatSystemMessageRoleSystem,
		Content: components.CreateChatSystemMessageContentStr(text),
	})
}

func userMsg(text string) components.ChatMessages {
	return components.CreateChatMessagesUser(components.ChatUserMessage{
		Role:    components.ChatUserMessageRoleUser,
		Content: components.CreateChatUserMessageContentStr(text),
	})
}

func toolMsg(callID, text string) components.ChatMessages {
	return components.CreateChatMessagesTool(components.ChatToolMessage{
		Role:       components.ChatToolMessageRoleTool,
		Content:    components.CreateChatToolMessageContentStr(text),
		ToolCallID: callID,
	})
}

// assistantText pulls the plain-text content out of an assistant message, which
// the SDK models as an optional string-or-array union. Tool-call-only replies
// have no text and yield "".
func assistantText(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}

// Ask sends one user message and returns the agent's final answer, running any
// tools the model requests along the way. The conversation is remembered, so
// follow-up questions have context.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	a.history = append(a.history, userMsg(input))

	for step := 0; step < maxSteps; step++ {
		// 1. ask the model what to do next
		reply, err := a.think(ctx)
		if err != nil {
			return "", err
		}
		//    remember what it said
		a.history = append(a.history, components.CreateChatMessagesAssistant(reply))

		// 2. no tools requested?
		if len(reply.ToolCalls) == 0 {
			// A plain message = the final answer. The conversation is now at a
			// safe boundary (no tool call awaits its result), so it's the right
			// moment to compact if we've hit the interval.
			a.turns++
			a.maybeCompact(ctx)
			//    → that's the final answer, done
			return assistantText(reply), nil
		}
		// 3. run the tools it asked for,
		a.runTools(ctx, reply.ToolCalls)
		//    append results, loop again
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
		if a.history[i].ChatUserMessage != nil {
			lastUser = i
			break
		}
	}
	if lastUser <= 1 {
		return nil // at most one turn after the system prompt — nothing older to fold
	}

	older := a.history[1:lastUser]
	recent := a.history[lastUser:]

	reply, err := a.send(ctx, []components.ChatMessages{
		systemMsg(compactPrompt),
		userMsg(renderTranscript(older)),
	}, nil)
	if err != nil {
		return err
	}
	summary := strings.TrimSpace(assistantText(reply))
	if summary == "" {
		return nil // nothing usable came back; keep the real history
	}

	compacted := make([]components.ChatMessages, 0, 2+len(recent))
	compacted = append(compacted, a.history[0]) // system prompt, untouched
	compacted = append(compacted, systemMsg("Summary of the conversation so far:\n"+summary))
	compacted = append(compacted, recent...) // most recent turn, verbatim
	a.history = compacted

	if a.OnCompact != nil {
		a.OnCompact(summary)
	}
	return nil
}

// renderTranscript flattens messages into plain text the model can summarize,
// including tool calls and their results so no context is silently dropped.
func renderTranscript(msgs []components.ChatMessages) string {
	var b strings.Builder
	for _, m := range msgs {
		switch {
		case m.ChatUserMessage != nil:
			fmt.Fprintf(&b, "User: %s\n", strDeref(m.ChatUserMessage.Content.Str))
		case m.ChatAssistantMessage != nil:
			if txt := assistantText(*m.ChatAssistantMessage); txt != "" {
				fmt.Fprintf(&b, "Assistant: %s\n", txt)
			}
			for _, tc := range m.ChatAssistantMessage.ToolCalls {
				fmt.Fprintf(&b, "Assistant called %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case m.ChatToolMessage != nil:
			fmt.Fprintf(&b, "Tool result: %s\n", strDeref(m.ChatToolMessage.Content.Str))
		}
	}
	return b.String()
}

// strDeref returns the pointed-to string, or "" if the pointer is nil (the
// content was supplied as a structured-part array rather than a plain string).
func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// think asks the model for its next move given the conversation so far.
func (a *Agent) think(ctx context.Context) (components.ChatAssistantMessage, error) {
	return a.send(ctx, a.history, a.tools.Specs())
}

// send issues one chat completion and returns the assistant message from the
// first choice. It's the single place we call the OpenRouter SDK, so both the
// main loop and compaction share the same request shape and error handling.
func (a *Agent) send(ctx context.Context, messages []components.ChatMessages, toolSpecs []components.ChatFunctionTool) (components.ChatAssistantMessage, error) {
	res, err := a.client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(a.model),
		Messages: messages,
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

// runTools executes every tool the model asked for and appends the results to
// the conversation so the next round can use them.
func (a *Agent) runTools(ctx context.Context, calls []components.ChatToolCall) {
	for _, call := range calls {
		result := a.tools.Dispatch(ctx, call.Function.Name, call.Function.Arguments)
		if a.OnToolCall != nil {
			a.OnToolCall(call.Function.Name, call.Function.Arguments, result)
		}
		a.history = append(a.history, toolMsg(call.ID, result))
	}
}
