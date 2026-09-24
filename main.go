// Command agent is the smallest thing that deserves the name: a loop that
// sends what you type to a model, prints what comes back, and remembers the
// conversation so far.
//
// There is no system prompt, no tools, no persistence. Every one of those is
// something to add on purpose, one at a time, once you have felt what the loop
// cannot do without it.
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

// Model is the OpenRouter model the agent talks to.
const Model = "openai/gpt-4o-mini"

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

		history = append(history, components.CreateChatMessagesUser(components.ChatUserMessage{
			Role:    components.ChatUserMessageRoleUser,
			Content: components.CreateChatUserMessageContentStr(line),
		}))

		reply, err := ask(context.Background(), client, history)
		if err != nil {
			// The turn failed, so it never happened: drop the question that
			// went unanswered rather than leaving the history half-written.
			history = history[:len(history)-1]
			fmt.Println("error:", err)
			continue
		}

		history = append(history, components.CreateChatMessagesAssistant(reply))
		fmt.Println("\nagent>", text(reply))
	}
}

// ask is one model call: the whole conversation goes out, one reply comes back.
func ask(ctx context.Context, client *openrouter.OpenRouter, history []components.ChatMessages) (components.ChatAssistantMessage, error) {
	res, err := client.Chat.Send(ctx, components.ChatRequest{
		Model:    openrouter.String(Model),
		Messages: history,
	}, nil)
	if err != nil {
		return components.ChatAssistantMessage{}, err
	}
	if res.ChatResult == nil || len(res.ChatResult.Choices) == 0 {
		return components.ChatAssistantMessage{}, fmt.Errorf("model returned no choices")
	}
	return res.ChatResult.Choices[0].Message, nil
}

// text pulls the reply out of the SDK's optional string-or-array union.
func text(m components.ChatAssistantMessage) string {
	if c, ok := m.Content.Get(); ok && c != nil && c.Str != nil {
		return *c.Str
	}
	return ""
}
