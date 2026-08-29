package agent

import "github.com/aliworkshop/ai-engineering-course/internal/tools"

// Part 5 · routing and handoffs.
//
// The honest answer that most multi-agent material skips: usually you don't
// need this. One capable model with good tools is a generalist and handles a
// mixed workload fine, and reaching for a swarm of agents is normally
// over-engineering. A handoff earns its keep when the other agent is genuinely
// a different thing, and the case that always qualifies is least privilege.
//
// So the split here is not "a teacher and a research agent" — that would be
// decoration. It is: the agent you talk to cannot change your disk. It answers
// English questions and searches its own reference; if a job needs anything
// done to the machine, it hands over to the operator, which holds those tools
// and nothing else. The teacher cannot be prompted into a destructive action,
// because there is no destructive tool in its list to call.

// Spec is an agent as data: a name, a prompt, and the subset of tools it holds.
// Adding a specialist is configuration, not machinery — the same durable loop
// runs whichever spec is current.
type Spec struct {
	Name    string
	Prompt  string
	Tools   ToolBox
	Purpose string // one line, shown to other agents in the handoff tool
}

// Roster is every agent the harness can run, keyed by name.
type Roster map[string]Spec

// Purposes renders the roster for the handoff tool's description.
func (r Roster) Purposes() map[string]string {
	out := make(map[string]string, len(r))
	for name, spec := range r {
		out[name] = spec.Purpose
	}
	return out
}

// The two agents, and the tools each holds. These are name lists rather than
// tool values so the roster can be declared here, next to the prompts that
// describe it, while the tools themselves stay in the tools package.
const (
	TeacherName  = "teacher"
	OperatorName = "operator"

	// AssistantName is what an agent running WITHOUT a roster calls itself. It
	// names no spec below: one agent holding every tool is the pre-Part-5 shape
	// that the eval harness still runs, and it needs something to answer to.
	AssistantName = "assistant"
)

// TeacherTools is what the English teacher holds: the reference it looks rules
// up in, and the handoff it uses to get rid of anything that isn't its job.
// Note what is missing — everything that changes the machine, and everything
// unrelated to English. A tool an agent does not hold is a tool it cannot be
// talked into using, and a toolset that matches the job is also what keeps an
// answer on the question that was asked.
var TeacherTools = []string{
	tools.KnowledgeTool,
	tools.HandoffTool,
}

// OperatorTools is the specialist's set: every tool that changes the machine,
// plus what it needs to check its own work. The gated tools it was built to
// hold are gone, so the list is down to run_code — the split stays wired up,
// and a new dangerous tool is given to the operator by adding one line here
// rather than by rebuilding the routing around it.
var OperatorTools = []string{
	"run_code",
}

// InvestigatorTools is what a Part 6 sub-agent holds: the read-only tools it
// needs to find things out, and nothing that changes the machine. An
// investigation runs in the background, where a side effect nobody watched
// happen is a surprise nobody asked for.
var InvestigatorTools = []string{
	"get_weather",
	"openrouter_web_search",
	"run_code",
}

const TeacherPurpose = "English: correcting text, and questions about grammar and usage"

const OperatorPurpose = "anything that changes the machine"

// TeacherRoster is the wiring: the teacher and the operator, over one registry.
//
// It lives here rather than in main so that everything which needs the real
// agent gets the same one — the CLI, the browser, and the evals. An eval that
// builds its own approximation of the roster grades an agent nobody runs, and
// the divergence is invisible until the day it matters.
func TeacherRoster(registry *tools.Registry) Roster {
	return Roster{
		TeacherName: {
			Name: TeacherName, Purpose: TeacherPurpose,
			Prompt: TeacherPrompt, Tools: registry.Subset(TeacherTools...),
		},
		OperatorName: {
			Name: OperatorName, Purpose: OperatorPurpose,
			Prompt: OperatorPrompt, Tools: registry.Subset(OperatorTools...),
		},
	}
}

// TeacherPrompt is the agent's standing instructions — the product, really.
// Two things in it are load-bearing beyond the role.
//
// The output shape is fixed, because a correction the user has to hunt for is
// worth less than one they can paste straight back: the corrected text first
// and whole, then what changed and why.
//
// And it says, twice, to answer the question that was asked and then stop. An
// answer that wanders into neighbouring rules is not merely longer, it is less
// relevant — which is exactly what the AnswerRelevancyMetric in
// internal/evalscore measures, statement by statement.
const TeacherPrompt = `You are an English teacher. You do two things and nothing else:
correct English text, and answer questions about English grammar and usage.

Look it up before you explain it:
- Call search_knowledge before you name or explain a rule. Query it the way a
  reference book would phrase the topic — "present perfect vs past simple",
  "comma before and", "a vs an" — not with the user's whole sentence.
- The results carry a similarity score, but it only ranks them against each
  other — the numbers are small even for a perfect match. Decide coverage by
  READING what came back: cite a file only if its text states the rule you are
  using. If none of them do, answer from your own knowledge and say the
  reference does not cover it. Never invent a rule name or a source.

WHEN THE USER SENDS TEXT TO CORRECT, reply in exactly this shape:

**Corrected**
the full text, every error fixed and nothing else touched

**Changes**
- "the wrong bit" -> "the fixed bit" — the rule, in one clause (source: file-name)

one bullet per change, in the order the changes appear. And:
- Fix grammar, spelling, punctuation and wrong word choice. Keep the writer's
  meaning, voice and level of formality. Do NOT rewrite for style; if a
  sentence is correct but clumsy, leave it and add at most two suggestions
  under a final "Style (optional)" heading.
- If the text is already correct, say so in one line and stop. Never invent a
  change to have something to report.
- Correct the text; do not answer it. "Where is the station" is a sentence to
  punctuate, not a question to answer.
- Follow the user's spelling variety, British or American. If they mix the two,
  standardise and say which you chose.

WHEN THE USER ASKS A GRAMMAR QUESTION:
- Answer that question in two or three sentences, then one or two short
  examples — the correct form and, where it helps, the wrong form it replaces.
- Name the rule and cite the reference file you used.
- Stop there. No tour of neighbouring rules, no recap of what you just said.

Always: you cannot change anything on this machine — no files, no commands. If
a request needs that, hand off to the operator. Keep the reply under 200 words
unless the text you were given is longer than that.`

// OperatorPrompt is the specialist's. It is stricter on purpose: this is the
// agent that can actually break something, and its prompt should read like it.
const OperatorPrompt = `You are the operator: the agent that is allowed to change the machine.

A tool that changes something asks a human for approval before it runs, and
what it did cannot be taken back. Behave accordingly:

- Do exactly the work you were handed. Do not tidy up, refactor, or improve
  anything that wasn't asked for.
- Approval is asked for automatically, so do not ask for permission in your
  reply first. Change exactly what you were asked to change, and nothing else.
- If a human denies an action, do not retry it and do not work around it. Say
  what was refused and stop.
- Use run_code to work something out before or after you act. It runs in an
  EMPTY sandbox with no network and cannot touch the user's machine, so it is
  never the way to do the work itself.
- When the work is done, summarize what changed in one or two lines.`

// SystemPrompt is the single-agent prompt: one agent holding every tool, the
// way the harness ran before Part 5 split it in two.
//
// It is kept because a roster is opt-in. The eval harness runs without one, so
// its scenarios keep measuring the loop rather than a routing decision — and
// anyone who wants the pre-handoff behaviour back gets it by not passing a
// roster, rather than by reverting anything.
const SystemPrompt = `You are a helpful command-line assistant with access to tools.

Rules:
- Answer directly from your own knowledge when you can. Do NOT call a tool for
  things you already know (math, definitions, general facts).
- Don't make up facts. If you don't know, say so.
- Use openrouter_web_search only when the user needs current, external, or
  unknown facts. It answers with source URLs — keep them in your reply.
- Use run_code when a task needs lookup plus filtering, counting, or arithmetic:
  ONE program that does the whole job instead of several tool calls. It runs in
  an EMPTY sandbox with no network; everything from outside comes through the
  tool bridge.
- Keep answers short and clear.`
