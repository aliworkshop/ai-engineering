// Command agent is an AI assistant you talk to in a loop. It answers from its
// own knowledge, searches the web, writes and runs scripts, and edits files —
// asking a human before anything dangerous.
//
// Run:  go run .            (terminal)
//
//	go run . -http :8080  (browser)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/ui"
	"github.com/aliworkshop/ai-engineering-course/internal/web"
)

// Model is the OpenRouter model the agent talks to. gpt-4o-mini supports tools.
const Model = "openai/gpt-4o-mini"

// SearchModel writes up the results of openrouter_web_search. OpenRouter's web
// plugin does the actual searching, so this model only has to summarize what it
// is handed — a small one is plenty.
const SearchModel = "openai/gpt-4o-mini"

func main() {
	addr := flag.String("http", "", "serve the browser UI on this address (e.g. :8080) instead of running in the terminal")
	flag.Parse()

	loadEnv()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}
	client := llm.NewOpenRouter(apiKey)

	if *addr != "" {
		serveWeb(*addr, client)
		return
	}

	// Wire the layers together: the console is the human approver, the tools use
	// it to gate dangerous actions, and the agent drives the tools.
	console := ui.New(os.Stdin, os.Stdout)
	assistant := newAgent(client, console)
	console.Run(context.Background(), assistant)
}

// newAgent builds an agent whose dangerous tools are gated by the given
// approver. Both front-ends go through here, so the terminal and the browser
// get exactly the same agent — the only difference is who answers the y/n.
func newAgent(client *openrouter.OpenRouter, approver tools.Approver) *agent.Agent {
	// The OpenRouter search tool talks to the API itself, so it gets the same
	// client. SearchModel only formats the hits the plugin returns, so it stays
	// small and cheap regardless of which model the agent itself runs on.
	toolbox := tools.Default(approver, tools.WithOpenRouterSearch(client, SearchModel))
	return agent.New(client, Model, toolbox)
}

// serveWeb runs the browser UI. Each browser session gets its own agent, with
// that session standing in for the human: it approves dangerous tools and
// receives the progress the console would otherwise print.
func serveWeb(addr string, client *openrouter.OpenRouter) {
	fmt.Printf("Browser UI on http://localhost%s — Ctrl-C to stop.\n", addr)

	err := web.ListenAndServe(addr, func(session *web.Session) web.Assistant {
		assistant := newAgent(client, session)
		assistant.OnToolCall = session.LogTool
		assistant.OnCompact = session.LogCompact
		return assistant
	})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
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
