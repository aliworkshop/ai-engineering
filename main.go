// Command agent is a small assistant you talk to in a loop, and which can
// search the web.
//
// This file does one thing: build the pieces and connect them. Every decision
// worth reading lives a layer in — the loop in internal/agent, the tools in
// internal/tools, the terminal in internal/ui — and main is the only place that
// knows all three exist.
//
// Run:  go run .     (needs OPENROUTER_API_KEY in .env)
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/ui"
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

	// Wire the layers together, outermost last: a client, the tools that use
	// it, the agent that drives the tools, and the console that drives the
	// agent. Each one knows only the layer beneath it.
	client := llm.NewOpenRouter(apiKey)
	toolbox := tools.Default(client)
	assistant := agent.New(client, Model, toolbox)
	console := ui.New(os.Stdin, os.Stdout)

	console.Run(context.Background(), assistant)
}
