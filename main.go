// Command agent is a support-triage agent you talk to in a loop — and,
// underneath it, a harness: every move is an event, and every model turn and
// tool call is a checkpointed step, so a crash mid-task costs a replay rather
// than a second email to the customer.
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
//	go run . -list           what the runtime is holding
//	go run . -resume <id>    replay one workflow
//	go run . -audit          did any work happen twice?
//	go run . -crash-at 2     die mid-run, after real side effects
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
	"github.com/aliworkshop/ai-engineering-course/internal/jev"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
	"github.com/aliworkshop/ai-engineering-course/internal/tools"
	"github.com/aliworkshop/ai-engineering-course/internal/triage"
	"github.com/aliworkshop/ai-engineering-course/internal/ui"
)

// Model is the OpenRouter model the agent talks to. It has to support tools.
const Model = "openai/gpt-4o-mini"

// The harness keeps its state in one directory, so everything the runtime owns
// is inspectable with ls and deletable with rm -rf. All of it is plain files by
// design: a workflow is a JSON file, the event log is JSONL. Swapping in
// Postgres later changes the storage, not a single idea above it.
const harnessDir = ".harness"

func harnessPath(parts ...string) string {
	return filepath.Join(append([]string{harnessDir}, parts...)...)
}

func main() {
	sample := flag.Bool("sample", false, "work the three sample items, then exit")
	task := flag.String("task", "", "work one task, then exit")
	list := flag.Bool("list", false, "list workflows and their status")
	resume := flag.String("resume", "", "replay a workflow that stopped early")
	audit := flag.Bool("audit", false, "read the event log back and report whether any work happened twice")
	noJev := flag.Bool("no-jev", false, "run the agent from before Jev: the model classifies and searches for itself, and nothing checks a draft")
	crashAt := flag.Int("crash-at", -1, "DEMO: exit before this step, simulating a crash mid-task")
	flag.Parse()

	_ = godotenv.Load()

	// Two commands read the runtime's own files and never talk to a model, so
	// they work without a key — which matters, because the moment you want to
	// audit a log is usually not the moment you want to spend money.
	if *audit {
		exitOn(auditLog())
		return
	}
	if *list {
		exitOn(listWorkflows())
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

	store, err := durable.NewStore(harnessPath("wf"), bus)
	exitOn(err)

	// Which agent this is, decided in one place. With Jev the model is left
	// with the one job only it can do — writing the reply — and gets the
	// toolbox for that; without it, the model does all four jobs itself.
	registry, judge := tools.Reply(), triage.New(jev.New(apiKey).WithEvents(bus), bus)
	if *noJev {
		registry, judge = tools.Default(), nil
		fmt.Println("(-no-jev — the model classifies and searches for itself, and nothing checks a draft)")
	}

	assistant := agent.New(llm.NewOpenRouter(apiKey), Model, registry).
		WithEvents(bus).
		WithStore(store).
		WithJudge(judge).
		WithCrashAt(*crashAt)

	if *resume != "" {
		exitOn(once(assistant.Resume, *resume))
		return
	}

	// Recover FIRST, then take new work. This is exactly what a durable engine
	// does on launch: find every workflow that was mid-flight when the process
	// last died and replay it forward. Nothing asks you to do it, and nothing
	// re-runs — which is the whole claim, made without being announced.
	exitOn(recoverPending(assistant, store))

	if *sample {
		*task = agent.SampleTask
	}
	if *task != "" {
		exitOn(once(assistant.Ask, *task))
		return
	}

	ui.New(os.Stdin, os.Stdout).Run(context.Background(), assistant)
}

// recoverPending replays every workflow that crashed.
func recoverPending(assistant *agent.Agent, store *durable.Store) error {
	pending, err := store.Pending()
	if err != nil {
		return err
	}
	for _, row := range pending {
		fmt.Printf("recovering %s from its last completed step…\n", row.ID)
		if _, err := assistant.Resume(context.Background(), row.ID); err != nil {
			// One unrecoverable workflow should not stop the others, or the
			// process. Report it and carry on.
			fmt.Printf("  %s could not be recovered: %v\n", row.ID, err)
		}
	}
	return nil
}

// once works a single task and prints the answer — the shape anything outside
// Go needs to drive this: a shell script, a CI step, a demo you want to watch
// the event stream scroll past.
func once(work func(context.Context, string) (string, error), arg string) error {
	answer, err := work(context.Background(), arg)
	if err != nil {
		return err
	}
	fmt.Println("\nagent>", answer)
	return nil
}

// listWorkflows shows what the runtime is holding. A workflow still marked
// running is one that crashed: nobody is running it.
func listWorkflows() error {
	store, err := durable.NewStore(harnessPath("wf"), nil)
	if err != nil {
		return err
	}
	rows, err := store.List()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No workflows yet.")
		return nil
	}

	fmt.Printf("%-10s %-9s %-6s %s\n", "ID", "STATUS", "STEPS", "TASK")
	for _, row := range rows {
		fmt.Printf("%-10s %-9s %-6d %s\n", row.ID, row.Status, row.Steps, truncate(row.Input, 50))
	}
	fmt.Printf("\nEvent log: %s\n", harnessPath("events.jsonl"))
	return nil
}

// auditLog answers the question the whole part is built around: did a crash
// ever cause a side effect to happen twice?
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

	fmt.Printf("\n  %-22s %d\n", "tool calls executed", report.Calls)
	fmt.Printf("  %-22s %d\n", "judgments bought", report.Judgments)
	fmt.Printf("  %-22s %d  (work a resumed run did not redo)\n", "steps replayed", report.Replayed)

	// Two claims, because they fail differently. A repeated tool call is a
	// customer emailed twice; a repeated judgment is a decision already made
	// and written down, bought again.
	repeats("repeated side effects", "a tool ran more than once", report.Duplicated)
	repeats("repeated judgments", "a decision was bought twice", report.Rejudged)
	return nil
}

func repeats(label, complaint string, found []events.Duplicate) {
	if len(found) == 0 {
		fmt.Printf("  %-22s 0  ✔\n", label)
		return
	}
	fmt.Printf("  %-22s %d  ✘ %s\n\n", label, len(found), complaint)
	for _, d := range found {
		fmt.Printf("    %d×  %s  (%s) in %s\n", d.Count, d.Name, d.Call, d.Workflow)
	}
}

// truncate keeps a listed task to one line. The newlines matter: a task is
// often several work items, and a table that wraps is not a table.
func truncate(s string, max int) string {
	flat := []rune(strings.NewReplacer("\n", " ", "\r", " ").Replace(s))
	if len(flat) <= max {
		return string(flat)
	}
	return string(flat[:max]) + "…"
}

func exitOn(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
