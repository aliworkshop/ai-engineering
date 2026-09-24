// Command agent is a small assistant you talk to in a loop, and which can
// search the web.
//
// The loop itself has moved to internal/agent: this file builds the pieces,
// connects them, and reads lines from the terminal.
//
// Run:  go run .     (needs OPENROUTER_API_KEY in .env)
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

func main() {
	_ = godotenv.Load()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}

	client := llm.NewOpenRouter(apiKey)
	toolbox := tools.Default(client)
	assistant := agent.New(client, Model, toolbox)

	// The agent reports what it is doing rather than printing it, so the
	// terminal decides what that looks like.
	assistant.OnToolCall = func(name, args, _ string) {
		fmt.Printf("  [%s] %s\n", name, args)
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

		answer, err := assistant.Ask(context.Background(), line)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\nagent>", answer)
	}
}
