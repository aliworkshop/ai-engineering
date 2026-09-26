// Command agent is a support-triage agent you talk to in a loop — and,
// underneath it, a runtime: every move is an event, and every model turn and
// tool call is a checkpointed step, so a crash mid-task costs a replay rather
// than a second email to the customer.
//
// Durability is DBOS Transact, holding its state in Postgres. Point
// DBOS_SYSTEM_DATABASE_URL at a database and the agent is durable and recovers
// itself on startup; leave it unset and the same loop runs in memory,
// checkpointing nothing and saying so.
//
// It is still missing the other half. sendReply emails a customer the instant
// the model asks, with no human in the way. That is what Part 7 is for.
//
// This file does one thing: build the pieces and connect them.
//
// Run:
//
//	go run .                 recover what crashed, then talk to it
//	go run . -sample         work the three sample items, then exit
//	go run . -task "..."     work one task, then exit
//	go run . -recover        launch and do nothing else; watch it finish itself
//	go run . -inspect [id]   the engine's own receipts, out of Postgres
//	go run . -audit          did any work happen twice?
//	go run . -crash-at 2     die mid-run, after real side effects
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/runtime"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/ui"
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

// SampleTask is the canned workload: three items that between them exercise
// every branch — one the knowledge base answers, one it answers with a known
// bug, and one it has a price for.
const SampleTask = `Handle these work items:
- item-1 (customer_message): "I was charged twice and need help."
- item-2 (bug_report): "The export button fails on Safari."
- item-3 (sales_request): "Can you send pricing for 50 seats?"`

// harnessDir holds what the process owns rather than the engine: the event
// log. Workflow state lives in Postgres now, which is why this directory has
// one file in it instead of a tree.
const harnessDir = ".harness"

// recoveryWindow is how long -recover stays alive while DBOS replays in the
// background. Long enough for a stalled run to finish its model calls.
const recoveryWindow = 60 * time.Second

func harnessPath(parts ...string) string {
	return filepath.Join(append([]string{harnessDir}, parts...)...)
}

func main() {
	sample := flag.Bool("sample", false, "work the three sample items, then exit")
	task := flag.String("task", "", "work one task, then exit")
	recover := flag.Bool("recover", false, "launch and do nothing else, while DBOS replays what crashed")
	inspect := flag.Bool("inspect", false, "print the engine's own workflow receipts; add an id for its steps")
	audit := flag.Bool("audit", false, "read the event log back and report whether any work happened twice")
	crashAt := flag.Int("crash-at", -1, "DEMO: exit before this step, simulating a crash mid-task")
	flag.Parse()

	_ = godotenv.Load()

	// The audit reads the runtime's own log and never talks to a model, so it
	// works without a key — which matters, because the moment you want to
	// audit a log is usually not the moment you want to spend money.
	if *audit {
		exitOn(auditLog())
		return
	}

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}

	// The event stream goes two places at once: the terminal, live, and a
	// JSONL file that outlives the process. Neither is a summary of the other.
	bus := events.Bus{
		events.NewConsole(os.Stdout),
		events.NewJSONL(harnessPath("events.jsonl")),
	}

	// Connecting is also what starts recovery: anything a previous process
	// left half-done begins replaying here, before we have asked for anything.
	engine, err := runtime.New(context.Background(), runtime.Options{
		Client: llm.NewOpenRouter(apiKey), Model: Model,
		Registry: tools.Default(), Bus: bus, CrashAt: *crashAt,
	})
	exitOn(err)
	defer engine.Close()

	if !engine.Durable() {
		fmt.Printf("(%s is not set — running without checkpoints; a crash loses the run)\n",
			runtime.DatabaseEnv)
	}

	switch {
	case *inspect:
		exitOn(engine.Inspect(flag.Arg(0)))
	case *recover:
		engine.Wait(recoveryWindow)
	case *sample || *task != "":
		if *sample {
			*task = SampleTask
		}
		answer, err := engine.Ask(context.Background(), *task)
		exitOn(err)
		fmt.Println("\nagent>", answer)
	default:
		ui.New(os.Stdin, os.Stdout).Run(context.Background(), engine)
	}
}

// auditLog answers the question durable execution is built around: did a crash
// ever cause a side effect to happen twice?
//
// The engine has its own receipts — see -inspect — but this reads OUR log, and
// the difference is worth keeping. The event stream is the one thing that
// still exists when the database does not, and it is the same stream whether
// the run was durable or not.
func auditLog() error {
	report, err := events.Audit(harnessPath("events.jsonl"))
	if os.IsNotExist(err) {
		fmt.Println("No event log yet — run the agent once.")
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Printf("%s — %d events across %d workflows\n\n", report.Path, report.Total, report.Workflows)
	for _, row := range report.ByType {
		fmt.Printf("  %-22s %d\n", row.Type, row.Count)
	}

	fmt.Printf("\n  tool calls executed    %d\n", report.Calls)
	if len(report.Duplicated) == 0 {
		fmt.Println("  repeated side effects  0  ✔ nothing ran twice")
		return nil
	}

	fmt.Printf("  repeated side effects  %d  ✘ a tool ran more than once\n\n", len(report.Duplicated))
	for _, d := range report.Duplicated {
		fmt.Printf("    %d×  %s  (%s) in %s\n", d.Count, d.Name, d.Call, d.Workflow)
	}
	return nil
}

func exitOn(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
