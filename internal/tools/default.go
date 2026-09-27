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

// Reply builds the toolset for an agent whose classifying and retrieving are
// done before it is asked anything — see internal/triage.
//
// What is left is the one job only a language model can do: writing the reply,
// and saying when it is ready to go out. Taking the other two away is not
// tidiness. A model that still HAD classifyItem would go on calling it, because
// a tool in the list is an invitation, and it would file the item a second time
// under a category nothing would read.
func Reply() *Registry {
	return NewRegistry(
		DraftReply{},
		SendReply{},
	)
}
