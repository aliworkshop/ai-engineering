package tools

import (
	"net/http"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/aliworkshop/ai-engineering-course/session-2/agent/internal/tools/diagram"
)

// Option tweaks the default toolset. Options exist for tools that need
// something Default can't build on its own — an authenticated API client, say —
// so callers that don't have one (tests, mostly) can keep calling Default with
// just an approver.
type Option func(*settings)

type settings struct {
	searchClient *openrouter.OpenRouter
	searchModel  string
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

// Default builds the standard toolset, wiring the human-in-the-loop approver
// into every dangerous tool. This is the single place that decides which tools
// the agent has and which of them are gated.
func Default(approver Approver, opts ...Option) *Registry {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	client := &http.Client{Timeout: 15 * time.Second}

	// read-only — no approval. GenerateDiagram does write, but only ever to the
	// one canvas.svg it owns, so it can't be steered into clobbering anything
	// and doesn't need a y/n on every redraw.
	list := []Tool{
		ReadFile{},
		GetWeather{HTTP: client},
		diagram.GenerateDiagram{},
		diagram.AddElements{},
		diagram.UpdateElements{},
		diagram.RemoveElements{},
	}
	if s.searchClient != nil {
		list = append(list, NativeWebSearch{Client: s.searchClient, Model: s.searchModel})
	}

	// dangerous — approval required
	list = append(list,
		WriteFile{Approver: approver},
		EditFile{Approver: approver},
		DeleteFile{Approver: approver},
		RunCommand{Approver: approver},
	)

	return NewRegistry(list...)
}
