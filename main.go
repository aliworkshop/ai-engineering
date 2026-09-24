// Command agent is a support-triage agent you talk to in a loop — and, wrapped
// around it, the beginnings of a harness: every move it makes is an event, and
// the terminal is the inspector.
//
// It is deliberately brittle. The conversation lives in a slice and nothing
// else, so killing the process loses it; and sendReply emails a customer the
// instant the model asks, with nothing in the way. Those two failures are the
// reason the rest of the harness gets built.
//
// This file does one thing: build the pieces and connect them.
//
// Run:
//
//	go run .             talk to it
//	go run . -sample     work the three sample items, then exit
//	go run . -task "..." work one task, then exit
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/ui"
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

func main() {
	sample := flag.Bool("sample", false, "work the three sample items, then exit")
	task := flag.String("task", "", "work one task, then exit")
	flag.Parse()

	_ = godotenv.Load()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}

	// Wire the layers together, outermost last: a client, the tools, the agent
	// that drives the tools, and the event stream it reports through. Each one
	// knows only the layer beneath it.
	client := llm.NewOpenRouter(apiKey)
	toolbox := tools.Default()
	assistant := agent.New(client, Model, toolbox).
		WithEvents(events.NewConsole(os.Stdout))

	if *sample {
		*task = agent.SampleTask
	}
	if *task != "" {
		exitOn(once(assistant, *task))
		return
	}

	ui.New(os.Stdin, os.Stdout).Run(context.Background(), assistant)
}

// once works a single task and prints the answer — the shape anything outside
// Go needs to drive this: a shell script, a CI step, a demo you want to watch
// the event stream scroll past.
func once(assistant *agent.Agent, task string) error {
	answer, err := assistant.Ask(context.Background(), task)
	if err != nil {
		return err
	}
	fmt.Println("\nagent>", answer)
	return nil
}

func exitOn(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
