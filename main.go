// Command agent is a small loop that talks to a model and can search the web.
//
// It is still deliberately thin: no system prompt, no persistence, one tool.
// The loop is the thing worth reading — the model does not run anything itself,
// it only *asks*, and this program decides what actually happens.
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
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

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
	client := openrouter.New(openrouter.WithSecurity(apiKey))

	// The conversation, in the order it happened. This slice is the agent's
	// entire memory: it is sent whole on every turn, which is why a long chat
	// gets slower and more expensive as it goes.
	var history []components.ChatMessages

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

		answer, err := turn(context.Background(), client, &history)
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
func turn(ctx context.Context, client *openrouter.OpenRouter, history *[]components.ChatMessages) (string, error) {
	for step := 0; step < maxSteps; step++ {
		reply, err := think(ctx, client, *history)
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
				Content:    components.CreateChatToolMessageContentStr(dispatch(ctx, client, call)),
				ToolCallID: call.ID,
			}))
		}
	}
	return "", fmt.Errorf("stopped after %d tool rounds without an answer", maxSteps)
}

// think is one model call: the whole conversation goes out, with the tools the
// model is allowed to ask for, and one reply comes back.
func think(ctx context.Context, client *openrouter.OpenRouter, history []components.ChatMessages) (components.ChatAssistantMessage, error) {
	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(Model),
		Messages: history,
		Tools:    []components.ChatFunctionTool{searchSpec()},
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

// dispatch runs the tool the model asked for and ALWAYS returns a string,
// turning any failure into text the model can read and react to. A tool that
// crashed the program would take the conversation with it; a tool that says
// "that didn't work" lets the model try something else.
func dispatch(ctx context.Context, client *openrouter.OpenRouter, call components.ChatToolCall) string {
	switch call.Function.Name {
	case SearchTool:
		result, err := webSearch(ctx, client, call.Function.Arguments)
		if err != nil {
			return "error: " + err.Error()
		}
		return result
	default:
		return "error: unknown tool " + call.Function.Name
	}
}

// text pulls the reply out of the SDK's optional string-or-array union. A
// reply that only asks for tools has no text, and yields "".
func text(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}
