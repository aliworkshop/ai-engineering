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

	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/ui"
)

// Model is the OpenRouter model the agent talks to. gpt-4o-mini supports tools.
const Model = "openai/gpt-4o-mini"

// SearchModel writes up the results of openrouter_web_search. OpenRouter's web
// plugin does the actual searching, so this model only has to summarize what it
// is handed — a small one is plenty.
const SearchModel = "openai/gpt-4o-mini"

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
	client := llm.NewOpenRouter(apiKey)
	// The OpenRouter search tool talks to the API itself, so it gets the same
	// client. SearchModel only formats the hits the plugin returns, so it stays
	// small and cheap regardless of which model the agent itself runs on.
	toolbox := tools.Default(console, tools.WithOpenRouterSearch(client, SearchModel))
	assistant := agent.New(client, Model, toolbox)

	console.Run(context.Background(), assistant)
}

// loadEnv reads the .env from the locations the app is actually launched from.
// Launched as `go run .` the working directory is this package, but launched
// from the parent as `go run ./agent` it isn't — and a bare godotenv.Load()
// only ever looks at ./.env, so it would miss the agent's own .env in that
// second case. We load both candidates; godotenv keeps the first value seen for
// a key, so nothing already set is overwritten. Missing files are fine — Load
// just returns an error we ignore.
func loadEnv() {
	for _, path := range []string{".env", "agent/.env"} {
		_ = godotenv.Load(path)
	}
}
