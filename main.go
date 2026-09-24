// Command agent is a small loop that talks to a model and can search the web.
//
// The loop is the thing worth reading — the model does not run anything itself,
// it only *asks*, and this program decides what actually happens. Which tools
// exist is no longer its business: it asks a tools.Registry for the specs and
// hands it the calls that come back.
//
// Run:  go run .     (needs OPENROUTER_API_KEY in .env)
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

// SystemPrompt is the agent's standing instructions: who it is, and how to
// behave. It is the first message in the conversation and stays there, so the
// model reads it before every reply — which is also what makes it the most
// expensive text in the program, and worth keeping short.
//
// Two of these lines are doing real work. "Answer from your own knowledge when
// you can" is what stops a model with a search tool from searching for things
// it knows; a tool in the list is an invitation, and an agent that searches for
// 12 * 9 is slower, costlier and no more correct. And "keep the source URLs" is
// what makes a searched answer checkable — without it the model happily
// summarizes away the evidence.
const SystemPrompt = `You are a command-line assistant. You answer questions, and you can search the web.

- Answer from your own knowledge when you can. Do NOT search for things you
  already know: arithmetic, definitions, how something works, general facts.
- Use web_search when the answer depends on something current or changing —
  news, prices, releases, versions, who holds a post today — or on anything
  after your training cutoff. Keep the source URLs in your reply when you do.
- Don't make things up. If you don't know and cannot find out, say so.
- Keep answers short: a few sentences, or a short list. No preamble, no
  restating the question, no offer of further help.`

// maxSteps caps how many tool rounds one question may take before we give up.
// Without it, a model that keeps asking for the same tool loops until your
// credit does.
const maxSteps = 6

func main() {
	_ = godotenv.Load()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}
	client := llm.NewOpenRouter(apiKey)

	// The one place that decides which tools exist is tools.Default. From here
	// on this file only knows there is a toolbox.
	toolbox := tools.Default(client)

	// The conversation, in the order it happened. This slice is the agent's
	// entire memory: it is sent whole on every turn, which is why a long chat
	// gets slower and more expensive as it goes.
	//
	// The system prompt is message zero and never moves. Anything that trims
	// this history later has to keep it — an agent that compacts away its own
	// instructions forgets what it is halfway through a conversation.
	history := []components.ChatMessages{
		components.CreateChatMessagesSystem(components.ChatSystemMessage{
			Role:    components.ChatSystemMessageRoleSystem,
			Content: components.CreateChatSystemMessageContentStr(SystemPrompt),
		}),
	}

	fmt.Println("Type a message, or 'exit' to quit.")
	input := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !input.Scan() {
			return
		}
		line := strings.TrimSpace(input.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return
		}

		// Where this turn started. A turn can append several messages — the
		// model's request, each tool's result — so a failure rewinds to here
		// rather than dropping one message and leaving a half-written turn.
		start := len(history)
		history = append(history, components.CreateChatMessagesUser(components.ChatUserMessage{
			Role:    components.ChatUserMessageRoleUser,
			Content: components.CreateChatUserMessageContentStr(line),
		}))

		answer, err := turn(context.Background(), client, toolbox, &history)
		if err != nil {
			history = history[:start]
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\nagent>", answer)
	}
}

// turn runs one question to completion: think, run whatever tools the model
// asked for, think again with the results, and stop when it replies with no
// tool calls. That last sentence is the whole agent loop.
//
// history is a pointer because a turn grows it — the model's tool request and
// each result have to be in the conversation, or the next model call has no
// idea why it is being asked again.
func turn(ctx context.Context, client *openrouter.OpenRouter, toolbox *tools.Registry, history *[]components.ChatMessages) (string, error) {
	for step := 0; step < maxSteps; step++ {
		reply, err := think(ctx, client, toolbox, *history)
		if err != nil {
			return "", err
		}
		*history = append(*history, components.CreateChatMessagesAssistant(reply))

		// No tool calls means the model is answering rather than acting.
		if len(reply.ToolCalls) == 0 {
			return text(reply), nil
		}

		for _, call := range reply.ToolCalls {
			fmt.Printf("  [%s] %s\n", call.Function.Name, call.Function.Arguments)

			// A tool's result goes back as a message keyed to the call that
			// asked for it. Matching those ids is what lets the model batch
			// several calls in one turn.
			*history = append(*history, components.CreateChatMessagesTool(components.ChatToolMessage{
				Role:       components.ChatToolMessageRoleTool,
				Content:    components.CreateChatToolMessageContentStr(toolbox.Dispatch(ctx, call.Function.Name, call.Function.Arguments)),
				ToolCallID: call.ID,
			}))
		}
	}
	return "", fmt.Errorf("stopped after %d tool rounds without an answer", maxSteps)
}

// think is one model call: the whole conversation goes out, with the tools the
// model is allowed to ask for, and one reply comes back.
func think(ctx context.Context, client *openrouter.OpenRouter, toolbox *tools.Registry, history []components.ChatMessages) (components.ChatAssistantMessage, error) {
	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(Model),
		Messages: history,
		Tools:    toolbox.Specs(),
	}, nil)
	if err != nil {
		return components.ChatAssistantMessage{}, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return components.ChatAssistantMessage{}, fmt.Errorf("model returned no choices")
	}

	reply := res.ChatResult.Choices[0].Message
	// Set explicitly: this message goes straight back into the history, and a
	// message with no role is not one the API will accept on the next turn.
	reply.Role = components.ChatAssistantMessageRoleAssistant
	return reply, nil
}

// text pulls the reply out of the SDK's optional string-or-array union. A
// reply that only asks for tools has no text, and yields "".
func text(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}
