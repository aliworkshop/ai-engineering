package agent

// SystemPrompt is the agent's standing instructions: who it is, and how to
// work. It is the first message in the conversation and stays there, so the
// model reads it before every reply — which is also what makes it the most
// expensive text in the program, and worth keeping short.
//
// It lives in this package, next to the loop it governs, rather than in main:
// the prompt is what the agent IS, not how it was wired up this time.
//
// Read what it asks for, then read tools.SendReply. The last step of the
// pipeline emails a customer, and there is not one word here — or anywhere
// else yet — about checking with a human first. The prompt is the only thing
// standing between a model and an irreversible action, and a prompt is not a
// control.
const SystemPrompt = `You are a support triage agent.
For each work item the user gives you:
1. Classify it with classifyItem.
2. Search the knowledge base with searchKnowledgeBase if it helps.
3. Draft a reply with draftReply, then send it with sendReply.
Work through every item, then briefly summarize what you did.`

// JevSystemPrompt is the prompt for the agent whose triage already happened.
//
// Read it against the one above and the difference is the shape of the whole
// change. Steps 1 and 2 are gone, replaced by a line telling the model NOT to
// do them — because the work is done, and a model handed a briefing it thinks
// it should verify will verify it, expensively, by guessing.
//
// The last paragraph is new and it is the interesting one. The agent is told
// that a check runs before every send, and what to do when one comes back
// blocked. Note what it is NOT: it is not an instruction to be truthful, and it
// does not ask the model to check its own work. The check is code, it runs
// whether or not this paragraph is here, and jailbreaking the paragraph does
// not switch it off. All the prompt does is tell the model how to recover from
// a refusal it cannot argue with — which is the right thing to put in a prompt,
// and a good test of whether something else should have been.
const JevSystemPrompt = `You are a support reply agent.
Every work item has already been triaged by Jev (a fast classifier): its
category, the classifier's confidence, and the relevant knowledge-base
articles are listed below the task. Do not reclassify or search.
For each item:
1. Draft a reply with draftReply using ONLY facts from that item's articles
   (if it has none, acknowledge the request and say a specialist will follow up).
2. Send it with sendReply.
A quality check runs before every send. If a send comes back blocked, read the
reason, redraft that item once, and send again.
Finish with a brief summary of what you did.`

// SampleTask is the canned workload: three items that between them exercise
// every branch — one the knowledge base answers, one it answers with a known
// bug, and one it has a price for. Run it with `go run . -sample`.
//
// It lives next to the prompt because it is only meaningful next to it: these
// are support work items, and they exercise this agent's pipeline and no
// other.
const SampleTask = `Handle these work items:
- item-1 (customer_message): "I was charged twice and need help."
- item-2 (bug_report): "The export button fails on Safari."
- item-3 (sales_request): "Can you send pricing for 50 seats?"`
