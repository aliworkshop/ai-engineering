package tools

import (
	"net/http"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
)

// Option tweaks the default toolset. Options exist for tools that need
// something Default can't build on its own — an authenticated API client, say —
// so callers that don't have one (tests, mostly) can keep calling Default with
// just an approver.
type Option func(*settings)

type settings struct {
	searchClient *openrouter.OpenRouter
	searchModel  string
	sandboxDir   string
	specialists  map[string]string
	extra        []Tool
}

// WithExtra adds tools built outside this package. The supervisor (Part 6)
// needs one: it drives a model and its own sub-agents, so it belongs in the
// agent package — but it is still just a Tool, and the registry should not have
// to know where a tool came from to advertise it.
func WithExtra(list ...Tool) Option {
	return func(s *settings) { s.extra = append(s.extra, list...) }
}

// WithOpenRouterSearch enables the openrouter_web_search tool, which searches
// through OpenRouter's own web plugin and therefore needs an authenticated
// client. Without this option that tool simply isn't advertised to the model —
// and since it's the agent's only way to search, the agent then has to answer
// from its own knowledge alone.
func WithOpenRouterSearch(client *openrouter.OpenRouter, model string) Option {
	return func(s *settings) {
		s.searchClient = client
		s.searchModel = model
	}
}

// WithSandbox enables run_code — code mode — with dir as the scratch space each
// run gets a throwaway directory under.
//
// It is opt-in rather than always-on because it changes how the agent solves
// things: given run_code, a model will often write one program where it would
// otherwise have made four tool calls. That is the win, but a test that asserts
// a task must reach for a particular tool wants the old shape, and should not
// have to be rewritten to keep passing.
func WithSandbox(dir string) Option {
	return func(s *settings) { s.sandboxDir = dir }
}

// WithSpecialists enables the handoff tool, naming the agents that can be
// handed to and what each is for. Without it the agent has no way to transfer
// control, which is the right default for a single-agent setup.
func WithSpecialists(specialists map[string]string) Option {
	return func(s *settings) { s.specialists = specialists }
}

// Default builds the standard toolset, wiring the human-in-the-loop approver
// into every dangerous tool. This is the single place that decides which tools
// the agent has and which of them are gated.
//
// Nothing in the set is dangerous at the moment, so approver goes unused — it
// stays in the signature because the gate is a property of the toolset, not of
// any one tool: the next tool that changes the machine is constructed here with
// it, and is gated from its first line.
func Default(approver Approver, opts ...Option) *Registry {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	client := &http.Client{Timeout: 15 * time.Second}

	// read-only — no approval.
	list := []Tool{
		GetWeather{HTTP: client},
	}
	if s.searchClient != nil {
		list = append(list, NativeWebSearch{Client: s.searchClient, Model: s.searchModel})
	}

	// Held as a pointer so its wiring can be completed after the registry
	// exists: run_code lends the registry's own tools to sandboxed code, and
	// the registry cannot be built until every tool in it is built.
	var code *RunCode
	if s.sandboxDir != "" {
		code = &RunCode{Scratch: s.sandboxDir}
		list = append(list, code)
	}

	if len(s.specialists) > 0 {
		list = append(list, Handoff{To: s.specialists})
	}
	list = append(list, s.extra...)

	// dangerous — approval required. Empty for now; a tool added here takes
	// Approver: approver and declares Sensitive() so the rest of the harness —
	// the sandbox bridge, the roster, the durable gate — picks it up for free.

	registry := NewRegistry(list...)

	if code != nil {
		code.Dispatch = registry.Dispatch
		code.Expose = sandboxable(registry)
	}
	return registry
}

// sandboxable is the tool list model-written code may reach: everything
// read-only, minus the two that only make sense at the harness level.
//
// run_code is excluded so a program cannot spawn another sandbox — recursion
// that buys nothing and makes timeouts meaningless. handoff is excluded because
// transferring control is the loop's decision to make, not a subroutine's.
func sandboxable(r *Registry) []string {
	var out []string
	for _, name := range r.SafeNames() {
		if name == "run_code" || name == HandoffTool {
			continue
		}
		out = append(out, name)
	}
	return out
}
