// Command agent is a CLI AI assistant you talk to in a loop. It answers from
// its own knowledge, searches the web, writes and runs scripts, and edits
// files — asking a human before anything dangerous.
//
// Run:  go run ./agent
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	"no_tools/agent/internal/agent"
	"no_tools/agent/internal/llm"
	"no_tools/agent/internal/tools"
	"no_tools/agent/internal/ui"
)

// Model is the OpenRouter model the agent talks to. gpt-4o-mini supports tools.
const Model = "openai/gpt-4o-mini"

func main() {
	godotenv.Load()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}

	// Wire the layers together: the console is the human approver, the tools use
	// it to gate dangerous actions, and the agent drives the tools.
	console := ui.New(os.Stdin, os.Stdout)
	toolbox := tools.Default(console)
	assistant := agent.New(llm.NewOpenRouter(apiKey), Model, toolbox)

	console.Run(context.Background(), assistant)
}
