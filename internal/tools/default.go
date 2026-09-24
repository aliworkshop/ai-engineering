package tools

// Default builds the standard toolset. This is the single place that decides
// which tools the agent has: three that change nothing, and one that emails a
// customer. Nothing in the list marks that difference yet — which is exactly
// the problem the rest of the harness is built to answer.
func Default() *Registry {
	return NewRegistry(
		ClassifyItem{},
		SearchKnowledgeBase{},
		DraftReply{},
		SendReply{},
	)
}
