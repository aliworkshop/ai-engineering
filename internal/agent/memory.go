package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
)

// Three different things, kept apart on purpose. Collapsing them is the single
// most common reason a long agent session gets slower, dumber, and more
// expensive with every turn:
//
//   - HISTORY is everything that happened. It lives in the durable event log
//     and the workflow file. It is never sent to the model wholesale.
//   - STATE is a compact running summary of older work. Concrete facts survive
//     it: paths, commands, values, what was already done.
//   - CONTEXT is what the model sees this turn. It is assembled fresh, on
//     demand, from the system prompt + the pinned goal + the summary + the
//     recent turns — and it is bounded.
//
// Context is a runtime decision, not a chat log.

// Msg is one message in our own vocabulary rather than the SDK's.
//
// Owning this type is what makes the rest of the harness possible: it is
// JSON-round-trippable, so a whole conversation can be a durable checkpoint
// (Part 2); it is cheap to measure, so compaction can budget in tokens rather
// than guess in turns (Part 4); and it is comparable in a test without standing
// up an SDK union.
type Msg struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one tool the model asked for. ID matters beyond bookkeeping: it
// is the key the approval gate checkpoints a human decision under, so it has to
// survive a round trip through disk.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// Turn is one user question and everything that followed it — the assistant's
// replies, the tools it called, and what they returned.
//
// Compaction works in whole turns because the alternative corrupts the
// transcript: cut between an assistant's tool call and the tool's result and
// the next request carries an orphaned tool message, which most APIs reject
// outright.
type Turn struct {
	Msgs []Msg `json:"msgs"`
}

// Budgets are deliberately generous but finite. The point is not to squeeze the
// prompt, it is that the number stops growing: an agent left running all day
// should cost the same per turn at 5pm as it did at 9am.
const (
	defaultMaxContextTokens  = 6000
	defaultKeepContextTokens = 2500
)

// Memory holds state and the recent turns, and knows how to assemble context.
type Memory struct {
	System  string // the standing instructions, never summarized away
	Summary string // STATE: older work, compacted
	Turns   []Turn // recent work, verbatim

	// MaxContextTokens is the ceiling that triggers compaction;
	// KeepContextTokens is what we compact down to. Two numbers rather than one
	// so compaction fires occasionally and does real work, instead of firing
	// every single turn to shave off one message.
	MaxContextTokens  int
	KeepContextTokens int
}

// NewMemory starts a memory with the standing system prompt.
func NewMemory(system string) *Memory {
	return &Memory{
		System:            system,
		MaxContextTokens:  defaultMaxContextTokens,
		KeepContextTokens: defaultKeepContextTokens,
	}
}

// Context assembles what the model sees this turn: standing instructions, the
// summary of older work, the recent turns, and finally the turn in flight.
//
// The current goal is pinned by construction — working is the live turn, and it
// always starts with the user's question — so the one thing the model must not
// lose is the one thing compaction can never reach.
func (m *Memory) Context(working []Msg) []Msg {
	out := make([]Msg, 0, 2+len(m.Turns)*4+len(working))
	out = append(out, Msg{Role: "system", Text: m.System})
	if m.Summary != "" {
		out = append(out, Msg{
			Role: "system",
			Text: "Summary of earlier work in this session:\n" + m.Summary,
		})
	}
	for _, turn := range m.Turns {
		out = append(out, turn.Msgs...)
	}
	return append(out, working...)
}

// Commit files a finished turn into recent memory.
func (m *Memory) Commit(msgs []Msg) {
	if len(msgs) == 0 {
		return
	}
	m.Turns = append(m.Turns, Turn{Msgs: msgs})
}

// Overflowing reports whether the assembled context has outgrown its budget.
// Measuring the *assembled* context rather than counting turns is the whole
// idea: one turn where the model batched six tool calls is worth more than five
// chatty ones, and a turn count cannot tell them apart.
func (m *Memory) Overflowing() bool {
	return EstimateTokens(m.Context(nil)) > m.MaxContextTokens
}

// Overflow returns the oldest turns that have to go to get back under budget,
// and keeps the rest. It always keeps at least the most recent turn: a
// follow-up question nearly always refers to it, and a summary blurs exactly
// the details — the path, the command, the number — that it refers to.
func (m *Memory) Overflow() []Turn {
	if len(m.Turns) <= 1 {
		return nil
	}
	cut := 0
	for cut < len(m.Turns)-1 {
		trial := &Memory{System: m.System, Summary: m.Summary, Turns: m.Turns[cut:]}
		if EstimateTokens(trial.Context(nil)) <= m.KeepContextTokens {
			break
		}
		cut++
	}
	if cut == 0 {
		return nil
	}
	old := m.Turns[:cut]
	m.Turns = append([]Turn(nil), m.Turns[cut:]...)
	return old
}

// EstimateTokens is the usual four-characters-per-token approximation. It is
// wrong in the third significant figure and right about the thing that matters:
// whether the prompt is growing without bound. A real tokenizer here would cost
// a dependency and buy nothing a budget check can use.
func EstimateTokens(msgs []Msg) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Role) + len(m.Text) + len(m.ToolCallID)
		for _, c := range m.ToolCalls {
			chars += len(c.Name) + len(c.Args) + len(c.ID)
		}
	}
	return chars / 4
}

// Transcript flattens messages into plain text for the summarizer, tool calls
// and results included — a summary that silently drops what the tools returned
// is how an agent forgets the file it just wrote.
func Transcript(turns []Turn) string {
	var b strings.Builder
	for _, turn := range turns {
		for _, m := range turn.Msgs {
			switch m.Role {
			case "user":
				fmt.Fprintf(&b, "User: %s\n", m.Text)
			case "assistant":
				if m.Text != "" {
					fmt.Fprintf(&b, "Assistant: %s\n", m.Text)
				}
				for _, c := range m.ToolCalls {
					fmt.Fprintf(&b, "Assistant called %s(%s)\n", c.Name, c.Args)
				}
			case "tool":
				fmt.Fprintf(&b, "Tool result: %s\n", m.Text)
			}
		}
	}
	return b.String()
}

// --- translation to and from the SDK's message union -------------------------
//
// One direction each, in one place. Everything else in the package works in
// Msg, so if the SDK's shape changes this is the only file that has to.

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

// fromAssistant converts one reply back into our vocabulary.
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

// compactArgs shortens a tool's JSON arguments for display. Handy in events,
// where a 4 KB file body in an args field makes the stream unreadable.
func compactArgs(args string) string {
	var probe any
	if json.Unmarshal([]byte(args), &probe) != nil {
		return args
	}
	if len(args) > 200 {
		return args[:200] + "…"
	}
	return args
}
