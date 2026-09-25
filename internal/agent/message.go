package agent

import (
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// Msg is one message in our own vocabulary rather than the SDK's.
//
// Owning this type is what buys the package boundary. The conversation becomes
// a plain Go value: easy to read in a debugger, easy to build in a test, and —
// now that model turns are checkpointed — easy to write to disk. That last one
// is why the JSON tags are here: you can only checkpoint what serializes, and
// the SDK's rich message type does not.
//
// The SDK's union types stay in the two translation functions below, where a
// change to the wire format is a change to one file.
type Msg struct {
	Role string `json:"role"` // "system", "user", "assistant", "tool"
	Text string `json:"text,omitempty"`

	// ToolCalls is what an assistant message ASKED for. A reply that only asks
	// for tools has no Text at all.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID ties a "tool" message back to the call it answers. Matching
	// those ids is what lets the model batch several calls in one turn.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is one request from the model to run something.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // the model's JSON arguments, verbatim
}

// toSDK translates the conversation into the SDK's message union, once per
// turn, at the boundary.
func toSDK(msgs []Msg) []components.ChatMessages {
	out := make([]components.ChatMessages, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			out = append(out, components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Role:    components.ChatSystemMessageRoleSystem,
				Content: components.CreateChatSystemMessageContentStr(m.Text),
			}))

		case "user":
			out = append(out, components.CreateChatMessagesUser(components.ChatUserMessage{
				Role:    components.ChatUserMessageRoleUser,
				Content: components.CreateChatUserMessageContentStr(m.Text),
			}))

		case "tool":
			out = append(out, components.CreateChatMessagesTool(components.ChatToolMessage{
				Role:       components.ChatToolMessageRoleTool,
				Content:    components.CreateChatToolMessageContentStr(m.Text),
				ToolCallID: m.ToolCallID,
			}))

		case "assistant":
			assistant := components.ChatAssistantMessage{
				Role: components.ChatAssistantMessageRoleAssistant,
			}
			if m.Text != "" {
				content := components.CreateChatAssistantMessageContentStr(m.Text)
				assistant.Content = optionalnullable.From(&content)
			}
			for _, c := range m.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, components.ChatToolCall{
					ID:   c.ID,
					Type: components.ChatToolCallTypeFunction,
					Function: components.ChatToolCallFunction{
						Name:      c.Name,
						Arguments: c.Args,
					},
				})
			}
			out = append(out, components.CreateChatMessagesAssistant(assistant))
		}
	}
	return out
}

// fromAssistant translates a reply back into our own vocabulary.
func fromAssistant(m components.ChatAssistantMessage) Msg {
	msg := Msg{Role: "assistant", Text: assistantText(m)}
	for _, c := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:   c.ID,
			Name: c.Function.Name,
			Args: c.Function.Arguments,
		})
	}
	return msg
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
