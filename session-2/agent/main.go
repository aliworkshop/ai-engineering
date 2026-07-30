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

	"agent/internal/agent"
	"agent/internal/llm"
	"agent/internal/tools"
	"agent/internal/ui"
)

// Model is the OpenRouter model the agent talks to. gpt-4o-mini supports tools.
const Model = "openai/gpt-4o-mini"

func main() {
	loadEnv()

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

// loadEnv reads the .env from the locations the app is actually launched from.
// `go run ./agent` runs with the working directory at the repo root, so a bare
// godotenv.Load() only sees ./.env there and misses the agent's own .env (where
// keys like TAVILY_API_KEY live). We load both candidates; godotenv keeps the
// first value seen for a key, so nothing already set is overwritten. Missing
// files are fine — Load just returns an error we ignore.
func loadEnv() {
	for _, path := range []string{".env", "agent/.env"} {
		_ = godotenv.Load(path)
	}
}
