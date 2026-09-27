package tools

// Default builds the standard toolset. This is the single place that decides
// which tools the agent has: four that change nothing, and one that emails a
// customer. Nothing in the list marks that difference yet — which is exactly
// the problem the rest of the harness is built to answer.
//
// runCode is the odd one out in a different way. It changes nothing either,
// but it is the only tool that runs code nobody in this repo wrote, which is
// why it is the only one with a door in front of it.
func Default() *Registry {
	return NewRegistry(
		ClassifyItem{},
		SearchKnowledgeBase{},
		DraftReply{},
		SendReply{},
		RunCode{},
	)
}
