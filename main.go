// Command agent is an AI assistant you talk to in a loop — and, underneath it,
// the harness that makes the loop survivable: durable execution, a sandbox for
// model-written code, bounded context, least-privilege specialists, parallel
// supervision, and human approval that can wait for days.
//
// Run:
//
//	go run .                    terminal
//	go run . -http :8080        browser
//	go run . -list              workflows, and which are waiting on you
//	go run . -approve <id>      approve a parked action and finish the run
//	go run . -deny <id>         refuse it and finish the run
//	go run . -resume <id>       pick up a run that crashed
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/approval"
	"github.com/aliworkshop/ai-engineering-course/internal/durable"
	"github.com/aliworkshop/ai-engineering-course/internal/events"
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

// The harness keeps its state in one directory, so everything the runtime owns
// is inspectable with ls and deletable with rm -rf. All of it is plain files by
// design: a workflow is a JSON file, the event log is JSONL, a human decision
// is a one-line JSON file. Swapping in Postgres later changes the storage, not
// a single idea above it.
const harnessDir = ".harness"

func harnessPath(parts ...string) string {
	return filepath.Join(append([]string{harnessDir}, parts...)...)
}

func main() {
	addr := flag.String("http", "", "serve the browser UI on this address (e.g. :8080) instead of running in the terminal")
	approveID := flag.String("approve", "", "approve the action a parked workflow is waiting on, then finish the run")
	denyID := flag.String("deny", "", "refuse the action a parked workflow is waiting on, then finish the run")
	resumeID := flag.String("resume", "", "resume a workflow that stopped early")
	list := flag.Bool("list", false, "list workflows and their status")
	verbose := flag.Bool("v", false, "print the full harness event stream, including every model turn and tool call")
	flag.Parse()

	verboseWiring = *verbose
	loadEnv()

	if *list {
		exitOn(listWorkflows())
		return
	}

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Println("Set OPENROUTER_API_KEY in your .env first.")
		os.Exit(1)
	}
	client := llm.NewOpenRouter(apiKey)

	switch {
	case *approveID != "":
		exitOn(decide(client, *approveID, true))
	case *denyID != "":
		exitOn(decide(client, *denyID, false))
	case *resumeID != "":
		exitOn(resume(client, *resumeID))
	case *addr != "":
		serveWeb(*addr, client)
	default:
		runTerminal(client, *verbose)
	}
}

// bus is the harness's event stream (Part 1): one glyphed line per event on the
// terminal, and every event appended to a durable JSONL log.
//
// The log is the quiet win here. Because everything is already an event with a
// timestamp, cost and latency reporting is a script over a file — no new
// instrumentation, no changes to the harness. That is what events-first design
// buys you.
func bus(out io.Writer, quiet bool) events.Emitter {
	console := events.NewConsole(out)
	console.Quiet = quiet
	// One JSONL sink per process, shared by every session: each holds a mutex,
	// and two of them appending to the same file would serialize against
	// nothing.
	return events.Bus{console, logSink}
}

// logSink is the process's durable event log. Built once, at startup, because
// the browser front-end builds a harness per session and they all write here.
var logSink = events.NewJSONL(harnessPath("events.jsonl"))

// harness builds the runtime: the event stream, the workflow store, the durable
// approval gate, and the agent wired to all three.
//
// approver is the interactive human — the console or a browser session. It may
// be nil, which is the headless case: every gated action parks the workflow for
// the approve CLI instead of asking anyone.
func harness(client *openrouter.OpenRouter, approver *approverOf, emitter events.Emitter) (*agent.Agent, *durable.Store, error) {
	store, err := durable.NewStore(harnessPath("wf"), emitter)
	if err != nil {
		return nil, nil, err
	}

	gate, err := approval.NewGate(harnessPath("decisions"), approver.live, emitter)
	if err != nil {
		return nil, nil, err
	}

	search := tools.WithOpenRouterSearch(client, SearchModel)

	// Investigators get their OWN registry, built with an approver that refuses
	// everything. Read-only is then a property of the box they hold rather than
	// a rule they are trusted to follow — a sub-agent cannot reach a dangerous
	// tool because there is no dangerous tool in its registry to reach.
	investigatorTools := tools.Default(refuseAll{}, search, tools.WithSandbox(harnessPath("sandbox"))).
		Subset(agent.InvestigatorTools...)
	supervisor := agent.NewSupervisor(client, Model, investigatorTools, emitter)

	// The full toolbox: every tool, with the durable gate on the dangerous ones.
	roster := agent.Roster{
		agent.AssistantName: {Name: agent.AssistantName, Purpose: agent.AssistantPurpose},
		agent.OperatorName:  {Name: agent.OperatorName, Purpose: agent.OperatorPurpose},
	}
	registry := tools.Default(gate, search,
		tools.WithSandbox(harnessPath("sandbox")),
		tools.WithSpecialists(roster.Purposes()),
		tools.WithExtra(agent.InvestigateTool{Sup: supervisor}),
	)

	// An agent is data: a name, a prompt, and the subset of tools it holds
	// (Part 5). Whatever changes the machine belongs to the operator; the
	// assistant does not hold it, and hands the work over instead.
	roster[agent.AssistantName] = agent.Spec{
		Name: agent.AssistantName, Purpose: agent.AssistantPurpose,
		Prompt: agent.AssistantPrompt, Tools: registry.Subset(agent.AssistantTools...),
	}
	roster[agent.OperatorName] = agent.Spec{
		Name: agent.OperatorName, Purpose: agent.OperatorPurpose,
		Prompt: agent.OperatorPrompt, Tools: registry.Subset(agent.OperatorTools...),
	}

	assistant := agent.New(client, Model, registry).
		WithRoster(roster, agent.AssistantName).
		WithStore(store).
		WithGate(gate).
		WithEvents(emitter)

	describeRoster(roster)
	return assistant, store, nil
}

// describeRoster prints who holds what, once, at startup.
//
// Least privilege is only worth anything if you can see it, and "which tools
// does this agent actually have?" is otherwise a question you answer by reading
// three files and hoping. Printing it also catches the boring failure — a tool
// misspelled in a roster list is silently skipped, and the only symptom is a
// model that never uses a capability you were sure it had.
func describeRoster(roster agent.Roster) {
	if !verboseWiring {
		return
	}
	for _, name := range []string{agent.AssistantName, agent.OperatorName} {
		spec, ok := roster[name]
		if !ok {
			continue
		}
		box, ok := spec.Tools.(*tools.Registry)
		if !ok {
			continue
		}
		fmt.Printf("  · %-9s holds %d tools: %s\n", name, len(box.Names()), strings.Join(box.Names(), ", "))
	}
}

// verboseWiring mirrors the -v flag for the one place that needs it below the
// call that parsed it. A package-level bool is the least ceremony that works
// and the flag is read-only after main sets it.
var verboseWiring bool

// approverOf carries the interactive human, if there is one. It exists so
// harness can take "maybe a human" without the nil-interface trap: a nil
// *ui.Console stored in an interface is not a nil interface, and the gate would
// then call a method on nothing.
type approverOf struct {
	live interface{ Confirm(string) bool }
}

// refuseAll is the approver investigators get. They should never reach a gated
// tool — they don't hold one — so if this is ever called, something is wrong and
// refusing is the answer.
type refuseAll struct{}

func (refuseAll) Confirm(string) bool { return false }

func runTerminal(client *openrouter.OpenRouter, verbose bool) {
	// The console is the human approver, the tools use it to gate dangerous
	// actions, and the agent drives the tools.
	console := ui.New(os.Stdin, os.Stdout)

	// Quiet unless asked: the console already prints its own [tool] lines, and
	// two renderings of the same event is noise, not observability.
	// Events render through the console's writer so each line pauses the
	// spinner first — see ui.Console.EventWriter.
	assistant, _, err := harness(client, &approverOf{live: console}, bus(console.EventWriter(), !verbose))
	exitOn(err)

	console.Run(context.Background(), assistant)
}

// serveWeb runs the browser UI. Each browser session gets its own agent, with
// that session standing in for the human: it approves dangerous tools and
// receives the progress the console would otherwise print.
func serveWeb(addr string, client *openrouter.OpenRouter) {
	fmt.Printf("Browser UI on http://localhost%s — Ctrl-C to stop.\n", addr)

	stream := bus(os.Stdout, false)
	err := web.ListenAndServe(addr, func(session *web.Session) web.Assistant {
		assistant, _, err := harness(client, &approverOf{live: session}, stream)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		assistant.OnToolCall = session.LogTool
		assistant.OnCompact = session.LogCompact
		return assistant
	})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}

// decide is the human side of Part 7: record a verdict for a parked workflow,
// then resume it. The two halves are deliberately separable — writing the
// decision is all that is strictly needed, and the resume could just as well
// happen on a schedule, in a worker, or on another machine.
func decide(client *openrouter.OpenRouter, workflowID string, approved bool) error {
	if err := approval.Write(harnessPath("decisions"), workflowID, approved, "cli"); err != nil {
		return err
	}
	verdict := "denied"
	if approved {
		verdict = "approved"
	}
	fmt.Printf("%s %s — resuming.\n\n", workflowID, verdict)
	return resume(client, workflowID)
}

// resume re-runs a workflow. There is no recovery code path: replay serves the
// checkpointed steps from disk — no model calls, no repeated side effects — and
// execution races forward to exactly where it stopped.
func resume(client *openrouter.OpenRouter, workflowID string) error {
	// No interactive human on this path. If the run hits a SECOND gate it parks
	// again rather than blocking a CLI invocation nobody is watching.
	assistant, _, err := harness(client, &approverOf{}, bus(os.Stdout, false))
	if err != nil {
		return err
	}

	answer, err := assistant.Resume(context.Background(), workflowID)
	if parked, ok := durable.IsSuspended(err); ok {
		fmt.Printf("\n⏸  %s\n   go run . -approve %s\n", parked.Reason, workflowID)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println("\nagent>", answer)
	return nil
}

// listWorkflows shows what the runtime is holding — most usefully, what is
// waiting on a human.
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

	fmt.Printf("%-10s %-10s %-10s %-6s %s\n", "ID", "STATUS", "AGENT", "STEPS", "TASK")
	for _, row := range rows {
		fmt.Printf("%-10s %-10s %-10s %-6d %s\n", row.ID, row.Status, row.Agent, row.Steps, row.Input)

		// A suspended run is a question addressed to a human. Showing the id
		// alone would make them run the agent again just to find out what they
		// are being asked to approve.
		if pending, ok := approval.Waiting(harnessPath("decisions"), row.ID); ok {
			fmt.Printf("%-10s └─ waiting on: %s\n", "", pending.Action)
			fmt.Printf("%-10s   go run . -approve %s   (or -deny %s)\n", "", row.ID, row.ID)
		}
	}
	fmt.Printf("\nEvent log: %s\n", harnessPath("events.jsonl"))
	return nil
}

func exitOn(err error) {
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
